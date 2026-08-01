package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"

	"github.com/crypt0rr/artist-tracker/internal/logging"
	"github.com/crypt0rr/artist-tracker/internal/security"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct{ DB *sql.DB }

type User struct {
	ID           int64
	Email        string
	PasswordHash string
	Role         string
	Timezone     string
	ReminderTime string
	CreatedAt    time.Time
}

type AdminUser struct {
	ID               int64
	Email            string
	Role             string
	Timezone         string
	ReminderTime     string
	FollowCount      int
	DestinationCount int
	CreatedAt        time.Time
}

var (
	ErrAdminRequired    = errors.New("administrator access is required")
	ErrCannotDeleteSelf = errors.New("you cannot delete your own account")
	ErrLastAdmin        = errors.New("the last administrator cannot be deleted")
)

type Session struct {
	User      User
	CSRFToken string
	ExpiresAt time.Time
}

type Artist struct {
	ID                 int64
	MBID               string
	Name               string
	SortName           string
	Type               string
	Country            string
	Disambiguation     string
	SpotifyID          string
	SpotifyURL         string
	SpotifyImageURL    string
	LastCheckedAt      *time.Time
	SpotifyNextCheckAt *time.Time
	BaselineSynced     bool
}

type Release struct {
	ID               int64
	MBID             string
	ArtistID         int64
	ArtistName       string
	Title            string
	PrimaryType      string
	SecondaryTypes   []string
	FirstReleaseDate string
	DatePrecision    int
	MusicBrainzURL   string
	SpotifyID        string
	SpotifyURL       string
	SpotifyImageURL  string
	Source           string
	FirstObservedAt  time.Time
}

type ReleaseBatch struct {
	Provider string
	Releases []Release
}

type Destination struct {
	ID           int64
	UserID       int64
	Name         string
	Service      string
	EncryptedURL []byte
	Enabled      bool
}

type Delivery struct {
	ID           int64
	EventID      int64
	Destination  Destination
	Title        string
	Body         string
	Attempts     int
	NextAttempt  time.Time
	EventType    string
	ReleaseTitle string
}

type DeliveryHistory struct {
	Title       string
	EventType   string
	Destination string
	Status      string
	Attempts    int
	LastError   string
	CreatedAt   time.Time
	SentAt      *time.Time
}

type AdminDeliveryHistory struct {
	UserEmail   string
	Title       string
	Body        string
	EventType   string
	Destination string
	Service     string
	Status      string
	Attempts    int
	LastError   string
	CreatedAt   time.Time
	NextAttempt *time.Time
	SentAt      *time.Time
}

type ManualSyncRequest struct {
	ID          int64
	RequestedBy int64
	Scope       string
	ArtistID    *int64
	Status      string
	CreatedAt   time.Time
	StartedAt   *time.Time
	FinishedAt  *time.Time
	LastError   string
}

type ProviderHealth struct {
	Provider      string
	LastSuccessAt *time.Time
	LastFailureAt *time.Time
	LastError     string
	NextCheckAt   *time.Time
	RateLimited   bool
	QuotaExceeded bool
	UpdatedAt     time.Time
}

type AdminArtist struct {
	ID   int64
	Name string
	MBID string
}

type ResolutionCandidate struct {
	MBID           string   `json:"mbid"`
	Name           string   `json:"name"`
	SortName       string   `json:"sort_name"`
	Type           string   `json:"type"`
	Country        string   `json:"country"`
	Disambiguation string   `json:"disambiguation"`
	Aliases        []string `json:"aliases,omitempty"`
	Score          int      `json:"score"`
}

func (c ResolutionCandidate) Artist() Artist {
	return Artist{
		MBID: c.MBID, Name: c.Name, SortName: c.SortName, Type: c.Type,
		Country: c.Country, Disambiguation: c.Disambiguation,
	}
}

type ArtistResolution struct {
	ID          int64
	UserID      int64
	Provider    string
	ProviderID  string
	DisplayName string
	ProviderURL string
	ImageURL    string
	Status      string
	Candidates  []ResolutionCandidate
	Attempts    int
	NextAttempt *time.Time
	LastError   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type syncedRelease struct {
	release  Release
	isNew    bool
	provider string
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{DB: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations
		(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		version, err := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("invalid migration %s", entry.Name())
		}
		var exists int
		if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=?`, version).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, string(body)); err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?,?)`, version, nowText())
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) Healthy(ctx context.Context) error {
	var one int
	return s.DB.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
}

func (s *Store) UserCount(ctx context.Context) (int, error) {
	var n int
	return n, s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
}

func (s *Store) CreateUser(ctx context.Context, email, hash, role, timezone string) (int64, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") {
		return 0, errors.New("a valid email address is required")
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return 0, errors.New("invalid IANA timezone")
	}
	result, err := s.DB.ExecContext(ctx, `INSERT INTO users(email,password_hash,role,timezone,created_at)
		VALUES(?,?,?,?,?)`, email, hash, role, timezone, nowText())
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var created string
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.Timezone, &u.ReminderTime, &created)
	u.CreatedAt, _ = parseTime(created)
	return u, err
}

func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT id,email,password_hash,role,timezone,reminder_time,created_at
		FROM users WHERE email=?`, strings.ToLower(strings.TrimSpace(email))))
}

func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	return scanUser(s.DB.QueryRowContext(ctx, `SELECT id,email,password_hash,role,timezone,reminder_time,created_at
		FROM users WHERE id=?`, id))
}

func (s *Store) AdminUsers(ctx context.Context) ([]AdminUser, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT u.id,u.email,u.role,u.timezone,u.reminder_time,u.created_at,
		COUNT(DISTINCT f.artist_id),COUNT(DISTINCT d.id)
		FROM users u
		LEFT JOIN follows f ON f.user_id=u.id
		LEFT JOIN destinations d ON d.user_id=u.id
		GROUP BY u.id,u.email,u.role,u.timezone,u.reminder_time,u.created_at
		ORDER BY CASE WHEN u.role='admin' THEN 0 ELSE 1 END,lower(u.email)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []AdminUser
	for rows.Next() {
		var user AdminUser
		var created string
		if err := rows.Scan(
			&user.ID, &user.Email, &user.Role, &user.Timezone, &user.ReminderTime, &created,
			&user.FollowCount, &user.DestinationCount,
		); err != nil {
			return nil, err
		}
		user.CreatedAt, _ = parseTime(created)
		users = append(users, user)
	}
	return users, rows.Err()
}

func (s *Store) DeleteUser(ctx context.Context, actingAdminID, userID int64) error {
	if actingAdminID == userID {
		return ErrCannotDeleteSelf
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var actingRole string
	if err := tx.QueryRowContext(ctx, `SELECT role FROM users WHERE id=?`, actingAdminID).Scan(&actingRole); err != nil {
		return err
	}
	if actingRole != "admin" {
		return ErrAdminRequired
	}
	var email, role string
	if err := tx.QueryRowContext(ctx, `SELECT email,role FROM users WHERE id=?`, userID).Scan(&email, &role); err != nil {
		return err
	}
	if role == "admin" {
		var admins int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&admins); err != nil {
			return err
		}
		if admins <= 1 {
			return ErrLastAdmin
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_tokens WHERE email=? OR created_by=?`, email, userID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id=?`, userID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func (s *Store) UpdatePassword(ctx context.Context, userID int64, hash string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, hash, userID); err == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID)
	}
	if err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateProfile(ctx context.Context, userID int64, timezone, reminder string) error {
	if _, err := time.LoadLocation(timezone); err != nil {
		return errors.New("invalid IANA timezone")
	}
	if _, err := time.Parse("15:04", reminder); err != nil {
		return errors.New("reminder time must use HH:MM")
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE users SET timezone=?, reminder_time=? WHERE id=?`, timezone, reminder, userID)
	return err
}

func (s *Store) CreateSession(ctx context.Context, userID int64, ttl time.Duration) (raw, csrf string, err error) {
	raw, err = security.Token(32)
	if err != nil {
		return "", "", err
	}
	csrf, err = security.Token(24)
	if err != nil {
		return "", "", err
	}
	now := time.Now().UTC()
	_, err = s.DB.ExecContext(ctx, `INSERT INTO sessions(token_hash,user_id,csrf_token,expires_at,created_at)
		VALUES(?,?,?,?,?)`, security.Digest(raw), userID, csrf, timeText(now.Add(ttl)), timeText(now))
	return raw, csrf, err
}

func (s *Store) Session(ctx context.Context, raw string) (Session, error) {
	var session Session
	var expires, created string
	err := s.DB.QueryRowContext(ctx, `SELECT u.id,u.email,u.password_hash,u.role,u.timezone,u.reminder_time,u.created_at,
		s.csrf_token,s.expires_at FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token_hash=? AND s.expires_at>?`, security.Digest(raw), nowText()).Scan(
		&session.User.ID, &session.User.Email, &session.User.PasswordHash, &session.User.Role,
		&session.User.Timezone, &session.User.ReminderTime, &created, &session.CSRFToken, &expires)
	session.User.CreatedAt, _ = parseTime(created)
	session.ExpiresAt, _ = parseTime(expires)
	return session, err
}

func (s *Store) DeleteSession(ctx context.Context, raw string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, security.Digest(raw))
	return err
}

func (s *Store) CreateAuthToken(ctx context.Context, kind, email string, userID *int64, creator int64, ttl time.Duration) (string, error) {
	raw, err := security.Token(32)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	_, err = s.DB.ExecContext(ctx, `INSERT INTO auth_tokens(token_hash,kind,email,user_id,expires_at,created_by,created_at)
		VALUES(?,?,?,?,?,?,?)`, security.Digest(raw), kind, strings.ToLower(strings.TrimSpace(email)), userID,
		timeText(now.Add(ttl)), creator, timeText(now))
	return raw, err
}

func (s *Store) ConsumeAuthToken(ctx context.Context, raw, kind string) (email string, userID *int64, err error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	var id sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT email,user_id FROM auth_tokens
		WHERE token_hash=? AND kind=? AND used_at IS NULL AND expires_at>?`,
		security.Digest(raw), kind, nowText()).Scan(&email, &id)
	if err != nil {
		tx.Rollback()
		return "", nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE auth_tokens SET used_at=? WHERE token_hash=?`, nowText(), security.Digest(raw)); err != nil {
		tx.Rollback()
		return "", nil, err
	}
	if err = tx.Commit(); err != nil {
		return "", nil, err
	}
	if id.Valid {
		userID = &id.Int64
	}
	return email, userID, nil
}

func (s *Store) LoginAllowed(ctx context.Context, key string) (bool, error) {
	var blocked sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT blocked_until FROM login_attempts WHERE key_hash=?`, security.Digest(key)).Scan(&blocked)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !blocked.Valid {
		return true, nil
	}
	t, _ := parseTime(blocked.String)
	return time.Now().UTC().After(t), nil
}

func (s *Store) RecordLoginFailure(ctx context.Context, key string) error {
	now := time.Now().UTC()
	var failures int
	var first string
	err := s.DB.QueryRowContext(ctx, `SELECT failures,first_at FROM login_attempts WHERE key_hash=?`, security.Digest(key)).Scan(&failures, &first)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.DB.ExecContext(ctx, `INSERT INTO login_attempts(key_hash,failures,first_at) VALUES(?,?,?)`,
			security.Digest(key), 1, timeText(now))
		return err
	}
	if err != nil {
		return err
	}
	firstAt, _ := parseTime(first)
	if now.Sub(firstAt) > 15*time.Minute {
		failures = 0
		firstAt = now
	}
	failures++
	var blocked any
	if failures >= 5 {
		blocked = timeText(now.Add(15 * time.Minute))
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE login_attempts SET failures=?,first_at=?,blocked_until=? WHERE key_hash=?`,
		failures, timeText(firstAt), blocked, security.Digest(key))
	return err
}

func (s *Store) ClearLoginFailures(ctx context.Context, key string) {
	s.DB.ExecContext(ctx, `DELETE FROM login_attempts WHERE key_hash=?`, security.Digest(key))
}

func (s *Store) UpsertArtist(ctx context.Context, a Artist) (Artist, error) {
	now := nowText()
	_, err := s.DB.ExecContext(ctx, `INSERT INTO artists(mbid,name,sort_name,artist_type,country,disambiguation,
		spotify_id,spotify_url,spotify_image_url,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(mbid) DO UPDATE SET name=excluded.name,sort_name=excluded.sort_name,
		artist_type=excluded.artist_type,country=excluded.country,disambiguation=excluded.disambiguation,
		spotify_id=COALESCE(excluded.spotify_id,artists.spotify_id),
		spotify_url=COALESCE(excluded.spotify_url,artists.spotify_url),
		spotify_image_url=COALESCE(excluded.spotify_image_url,artists.spotify_image_url),updated_at=excluded.updated_at`,
		a.MBID, a.Name, a.SortName, a.Type, a.Country, a.Disambiguation,
		nullString(a.SpotifyID), nullString(a.SpotifyURL), nullString(a.SpotifyImageURL), now, now)
	if err != nil {
		return Artist{}, err
	}
	return s.ArtistByMBID(ctx, a.MBID)
}

func (s *Store) ArtistByMBID(ctx context.Context, mbid string) (Artist, error) {
	var a Artist
	var sid, surl, image sql.NullString
	var checked sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT id,mbid,name,sort_name,artist_type,country,disambiguation,
		spotify_id,spotify_url,spotify_image_url,last_checked_at FROM artists WHERE mbid=?`, mbid).Scan(
		&a.ID, &a.MBID, &a.Name, &a.SortName, &a.Type, &a.Country, &a.Disambiguation,
		&sid, &surl, &image, &checked)
	a.SpotifyID, a.SpotifyURL, a.SpotifyImageURL = sid.String, surl.String, image.String
	if checked.Valid {
		t, _ := parseTime(checked.String)
		a.LastCheckedAt = &t
	}
	return a, err
}

func (s *Store) Follow(ctx context.Context, userID, artistID int64) (bool, error) {
	result, err := s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO follows(user_id,artist_id,created_at) VALUES(?,?,?)`,
		userID, artistID, nowText())
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

func (s *Store) Unfollow(ctx context.Context, userID, artistID int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM follows WHERE user_id=? AND artist_id=?`, userID, artistID)
	return err
}

func (s *Store) CreateArtistResolution(ctx context.Context, userID int64, providerID, name, providerURL, imageURL string) (ArtistResolution, bool, error) {
	providerID, name, providerURL = strings.TrimSpace(providerID), strings.TrimSpace(name), strings.TrimSpace(providerURL)
	if providerID == "" || name == "" || providerURL == "" {
		return ArtistResolution{}, false, errors.New("Spotify artist identity is incomplete")
	}
	now := nowText()
	result, err := s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO artist_resolutions
		(user_id,provider,provider_id,display_name,provider_url,image_url,status,next_attempt_at,created_at,updated_at)
		VALUES(?,'spotify',?,?,?,?, 'pending',?,?,?)`,
		userID, providerID, name, providerURL, strings.TrimSpace(imageURL), now, now, now)
	if err != nil {
		return ArtistResolution{}, false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return ArtistResolution{}, false, err
	}
	resolution, err := s.artistResolutionByProvider(ctx, userID, "spotify", providerID)
	return resolution, changed > 0, err
}

func scanArtistResolution(row interface{ Scan(...any) error }) (ArtistResolution, error) {
	var resolution ArtistResolution
	var candidates, nextAttempt, created, updated string
	var nullableNext sql.NullString
	err := row.Scan(
		&resolution.ID, &resolution.UserID, &resolution.Provider, &resolution.ProviderID,
		&resolution.DisplayName, &resolution.ProviderURL, &resolution.ImageURL, &resolution.Status,
		&candidates, &resolution.Attempts, &nullableNext, &resolution.LastError, &created, &updated,
	)
	if err != nil {
		return ArtistResolution{}, err
	}
	if candidates != "" {
		_ = json.Unmarshal([]byte(candidates), &resolution.Candidates)
	}
	if nullableNext.Valid {
		nextAttempt = nullableNext.String
		parsed, parseErr := parseTime(nextAttempt)
		if parseErr == nil {
			resolution.NextAttempt = &parsed
		}
	}
	resolution.CreatedAt, _ = parseTime(created)
	resolution.UpdatedAt, _ = parseTime(updated)
	return resolution, nil
}

const artistResolutionColumns = `id,user_id,provider,provider_id,display_name,provider_url,image_url,status,
	candidate_json,attempts,next_attempt_at,last_error,created_at,updated_at`

func (s *Store) artistResolutionByProvider(ctx context.Context, userID int64, provider, providerID string) (ArtistResolution, error) {
	return scanArtistResolution(s.DB.QueryRowContext(ctx, `SELECT `+artistResolutionColumns+`
		FROM artist_resolutions WHERE user_id=? AND provider=? AND provider_id=?`, userID, provider, providerID))
}

func (s *Store) ArtistResolution(ctx context.Context, userID, resolutionID int64) (ArtistResolution, error) {
	return scanArtistResolution(s.DB.QueryRowContext(ctx, `SELECT `+artistResolutionColumns+`
		FROM artist_resolutions WHERE id=? AND user_id=?`, resolutionID, userID))
}

func (s *Store) ArtistResolutions(ctx context.Context, userID int64) ([]ArtistResolution, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+artistResolutionColumns+`
		FROM artist_resolutions WHERE user_id=? ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ArtistResolution
	for rows.Next() {
		resolution, err := scanArtistResolution(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, resolution)
	}
	return result, rows.Err()
}

func (s *Store) DueArtistResolutions(ctx context.Context, now time.Time, limit int) ([]ArtistResolution, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+artistResolutionColumns+`
		FROM artist_resolutions WHERE status='pending' AND (next_attempt_at IS NULL OR next_attempt_at<=?)
		ORDER BY COALESCE(next_attempt_at,'') LIMIT ?`, timeText(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ArtistResolution
	for rows.Next() {
		resolution, err := scanArtistResolution(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, resolution)
	}
	return result, rows.Err()
}

func (s *Store) MarkArtistResolutionReview(ctx context.Context, userID, resolutionID int64, candidates []ResolutionCandidate) error {
	payload, err := json.Marshal(candidates)
	if err != nil {
		return err
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE artist_resolutions
		SET status='review',candidate_json=?,next_attempt_at=NULL,last_error='',updated_at=?
		WHERE id=? AND user_id=?`, string(payload), nowText(), resolutionID, userID)
	return changedOrNotFound(result, err)
}

func (s *Store) RetryArtistResolution(ctx context.Context, userID, resolutionID int64, attempts int, next time.Time, message string) error {
	result, err := s.DB.ExecContext(ctx, `UPDATE artist_resolutions
		SET status='pending',candidate_json='[]',attempts=?,next_attempt_at=?,last_error=?,updated_at=?
		WHERE id=? AND user_id=?`,
		attempts, timeText(next), message, nowText(), resolutionID, userID)
	return changedOrNotFound(result, err)
}

func (s *Store) CancelArtistResolution(ctx context.Context, userID, resolutionID int64) error {
	result, err := s.DB.ExecContext(ctx, `DELETE FROM artist_resolutions WHERE id=? AND user_id=?`, resolutionID, userID)
	return changedOrNotFound(result, err)
}

func changedOrNotFound(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) CompleteArtistResolution(ctx context.Context, resolution ArtistResolution, artist Artist) (Artist, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Artist{}, false, err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artist_resolutions WHERE id=? AND user_id=?`,
		resolution.ID, resolution.UserID).Scan(&exists); err != nil || exists == 0 {
		tx.Rollback()
		if err != nil {
			return Artist{}, false, err
		}
		return Artist{}, false, sql.ErrNoRows
	}
	now := nowText()
	_, err = tx.ExecContext(ctx, `INSERT INTO artists(mbid,name,sort_name,artist_type,country,disambiguation,
		spotify_id,spotify_url,spotify_image_url,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(mbid) DO UPDATE SET name=excluded.name,sort_name=excluded.sort_name,
		artist_type=excluded.artist_type,country=excluded.country,disambiguation=excluded.disambiguation,
		spotify_id=COALESCE(excluded.spotify_id,artists.spotify_id),
		spotify_url=COALESCE(excluded.spotify_url,artists.spotify_url),
		spotify_image_url=COALESCE(excluded.spotify_image_url,artists.spotify_image_url),updated_at=excluded.updated_at`,
		artist.MBID, artist.Name, artist.SortName, artist.Type, artist.Country, artist.Disambiguation,
		nullString(artist.SpotifyID), nullString(artist.SpotifyURL), nullString(artist.SpotifyImageURL), now, now)
	if err != nil {
		tx.Rollback()
		return Artist{}, false, err
	}
	var sid, surl, image, checked sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id,mbid,name,sort_name,artist_type,country,disambiguation,
		spotify_id,spotify_url,spotify_image_url,last_checked_at FROM artists WHERE mbid=?`, artist.MBID).Scan(
		&artist.ID, &artist.MBID, &artist.Name, &artist.SortName, &artist.Type, &artist.Country,
		&artist.Disambiguation, &sid, &surl, &image, &checked)
	if err != nil {
		tx.Rollback()
		return Artist{}, false, err
	}
	artist.SpotifyID, artist.SpotifyURL, artist.SpotifyImageURL = sid.String, surl.String, image.String
	if checked.Valid {
		parsed, _ := parseTime(checked.String)
		artist.LastCheckedAt = &parsed
	}
	follow, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO follows(user_id,artist_id,created_at) VALUES(?,?,?)`,
		resolution.UserID, artist.ID, now)
	if err != nil {
		tx.Rollback()
		return Artist{}, false, err
	}
	added, _ := follow.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM artist_resolutions WHERE id=? AND user_id=?`,
		resolution.ID, resolution.UserID); err != nil {
		tx.Rollback()
		return Artist{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Artist{}, false, err
	}
	return artist, added > 0, nil
}

func (s *Store) FollowedArtists(ctx context.Context, userID int64) ([]Artist, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT a.id,a.mbid,a.name,a.sort_name,a.artist_type,a.country,a.disambiguation,
		a.spotify_id,a.spotify_url,a.spotify_image_url,a.last_checked_at,f.baseline_synced_at
		FROM follows f JOIN artists a ON a.id=f.artist_id WHERE f.user_id=?
		ORDER BY lower(trim(a.name)), lower(trim(a.sort_name)), a.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Artist
	for rows.Next() {
		var a Artist
		var sid, surl, image, checked, baseline sql.NullString
		if err := rows.Scan(&a.ID, &a.MBID, &a.Name, &a.SortName, &a.Type, &a.Country, &a.Disambiguation,
			&sid, &surl, &image, &checked, &baseline); err != nil {
			return nil, err
		}
		a.SpotifyID, a.SpotifyURL, a.SpotifyImageURL = sid.String, surl.String, image.String
		if checked.Valid {
			t, _ := parseTime(checked.String)
			a.LastCheckedAt = &t
		}
		a.BaselineSynced = baseline.Valid
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) FollowedArtistCount(ctx context.Context, userID int64) (int, error) {
	var count int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM follows WHERE user_id=?`, userID).Scan(&count)
	return count, err
}

func (s *Store) ArtistsDue(ctx context.Context, now time.Time, limit int) ([]Artist, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT a.id,a.mbid,a.name,a.sort_name,a.artist_type,a.country,a.disambiguation,
		a.spotify_id,a.spotify_url,a.spotify_image_url,a.spotify_next_check_at
		FROM artists a JOIN follows f ON f.artist_id=a.id
		WHERE (a.next_check_at IS NULL OR a.next_check_at<=?)
		   OR (a.spotify_id IS NOT NULL AND (a.spotify_next_check_at IS NULL OR a.spotify_next_check_at<=?))
		ORDER BY COALESCE(a.next_check_at,a.spotify_next_check_at,'') LIMIT ?`,
		timeText(now), timeText(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Artist
	for rows.Next() {
		var a Artist
		var spotifyID, spotifyURL, spotifyImage, spotifyNext sql.NullString
		if err := rows.Scan(
			&a.ID, &a.MBID, &a.Name, &a.SortName, &a.Type, &a.Country, &a.Disambiguation,
			&spotifyID, &spotifyURL, &spotifyImage, &spotifyNext,
		); err != nil {
			return nil, err
		}
		a.SpotifyID, a.SpotifyURL, a.SpotifyImageURL = spotifyID.String, spotifyURL.String, spotifyImage.String
		if spotifyNext.Valid {
			t, _ := parseTime(spotifyNext.String)
			a.SpotifyNextCheckAt = &t
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) MarkArtistChecked(ctx context.Context, artistID int64, now time.Time, interval time.Duration) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE artists SET last_checked_at=?,next_check_at=? WHERE id=?`,
		timeText(now), timeText(now.Add(interval)), artistID)
	return err
}

func (s *Store) ScheduleArtistCheck(ctx context.Context, artistID int64, next time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE artists SET next_check_at=? WHERE id=?`, timeText(next), artistID)
	return err
}

func (s *Store) MarkSpotifyChecked(ctx context.Context, artistID int64, now time.Time, interval time.Duration) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE artists SET spotify_next_check_at=? WHERE id=?`,
		timeText(now.Add(spotifyPollDelay(artistID, interval))), artistID)
	return err
}

// spotifyPollDelay spreads artists deterministically across the polling
// interval. The offset is stable for an artist but does not need another
// persisted column, so restarts retain the same distribution.
func spotifyPollDelay(artistID int64, interval time.Duration) time.Duration {
	if interval <= 0 {
		return interval
	}
	sum := sha256.Sum256([]byte(strconv.FormatInt(artistID, 10)))
	span := interval
	offset := time.Duration(binary.BigEndian.Uint64(sum[:8]) % uint64(span))
	return interval/2 + offset
}

func (s *Store) ScheduleSpotifyCheck(ctx context.Context, artistID int64, next time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE artists SET spotify_next_check_at=? WHERE id=?`, timeText(next), artistID)
	return err
}

func (s *Store) LatestSpotifyReleaseDate(ctx context.Context, artistID int64) (string, error) {
	var date sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT MAX(first_release_date) FROM release_groups
		WHERE artist_id=? AND source IN ('spotify','both')`, artistID).Scan(&date)
	return date.String, err
}

func (s *Store) ApplyReleaseSync(ctx context.Context, artist Artist, releases []Release, observed time.Time) error {
	return s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{
		Provider: "musicbrainz",
		Releases: releases,
	}}, observed)
}

func (s *Store) ApplyReleaseBatches(ctx context.Context, artist Artist, batches []ReleaseBatch, observed time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	var savedReleases []syncedRelease
	spotifyObserved := false
	seenProviders := make(map[string]bool)
	for _, batch := range batches {
		provider := strings.ToLower(strings.TrimSpace(batch.Provider))
		if seenProviders[provider] {
			tx.Rollback()
			return fmt.Errorf("duplicate release batch for %s", provider)
		}
		seenProviders[provider] = true
		if provider == "spotify" {
			spotifyObserved = true
		}
		for _, release := range batch.Releases {
			var saved syncedRelease
			switch provider {
			case "musicbrainz":
				saved, err = saveMusicBrainzReleaseTx(ctx, tx, artist.ID, release, observed)
			case "spotify":
				saved, err = saveSpotifyReleaseTx(ctx, tx, artist.ID, release, observed)
			default:
				err = fmt.Errorf("unsupported release provider %q", provider)
			}
			if err != nil {
				tx.Rollback()
				return err
			}
			savedReleases = append(savedReleases, saved)
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT user_id,baseline_synced_at,spotify_baseline_synced_at
		FROM follows WHERE artist_id=?`, artist.ID)
	if err != nil {
		tx.Rollback()
		return err
	}
	type follower struct {
		id              int64
		baseline        bool
		spotifyBaseline bool
	}
	var followers []follower
	for rows.Next() {
		var f follower
		var baseline, spotifyBaseline sql.NullString
		if err := rows.Scan(&f.id, &baseline, &spotifyBaseline); err != nil {
			rows.Close()
			tx.Rollback()
			return err
		}
		f.baseline = baseline.Valid
		f.spotifyBaseline = spotifyBaseline.Valid
		followers = append(followers, f)
	}
	rows.Close()
	for _, follower := range followers {
		if !follower.baseline {
			if selected, eventType, ok := selectInitialRelease(savedReleases, observed); ok {
				title, body := initialReleaseMessage(artist, selected.release, eventType, observed)
				if err := enqueueEventTx(ctx, tx, follower.id, selected.release.ID, eventType, title, body, observed); err != nil {
					tx.Rollback()
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE follows SET baseline_synced_at=? WHERE user_id=? AND artist_id=?`,
				timeText(observed), follower.id, artist.ID); err != nil {
				tx.Rollback()
				return err
			}
			if spotifyObserved {
				if _, err := tx.ExecContext(ctx, `UPDATE follows SET spotify_baseline_synced_at=?
					WHERE user_id=? AND artist_id=?`, timeText(observed), follower.id, artist.ID); err != nil {
					tx.Rollback()
					return err
				}
			}
			continue
		}
		for _, item := range savedReleases {
			if item.provider == "spotify" && !follower.spotifyBaseline {
				continue
			}
			date, full := releaseDate(item.release.FirstReleaseDate)
			if !item.isNew || !full || date.Before(dayUTC(observed).AddDate(0, 0, -7)) {
				continue
			}
			if err := enqueueEventTx(ctx, tx, follower.id, item.release.ID, "announcement",
				"New release from "+artist.Name,
				fmt.Sprintf("%s has announced %q for %s.\n%s", artist.Name, item.release.Title,
					item.release.FirstReleaseDate, releaseExternalURL(item.release)),
				observed); err != nil {
				tx.Rollback()
				return err
			}
		}
		if spotifyObserved && !follower.spotifyBaseline {
			if _, err := tx.ExecContext(ctx, `UPDATE follows SET spotify_baseline_synced_at=?
				WHERE user_id=? AND artist_id=?`, timeText(observed), follower.id, artist.ID); err != nil {
				tx.Rollback()
				return err
			}
		}
	}
	return tx.Commit()
}

func saveMusicBrainzReleaseTx(
	ctx context.Context, tx *sql.Tx, artistID int64, release Release, observed time.Time,
) (syncedRelease, error) {
	if strings.TrimSpace(release.MBID) == "" {
		return syncedRelease{}, errors.New("MusicBrainz release group ID is required")
	}
	var releaseID int64
	existed := true
	err := tx.QueryRowContext(ctx, `SELECT id FROM release_groups WHERE mbid=?`, release.MBID).Scan(&releaseID)
	if errors.Is(err, sql.ErrNoRows) {
		existed = false
		releaseID, err = matchingReleaseIDTx(ctx, tx, artistID, release, true)
		if errors.Is(err, sql.ErrNoRows) {
			releaseID = 0
			err = nil
		} else if err == nil {
			existed = true
			_, err = tx.ExecContext(ctx, `UPDATE release_groups SET mbid=?,source='both' WHERE id=?`,
				release.MBID, releaseID)
		}
	}
	if err != nil {
		return syncedRelease{}, err
	}
	secondary, _ := json.Marshal(release.SecondaryTypes)
	if releaseID == 0 {
		result, insertErr := tx.ExecContext(ctx, `INSERT INTO release_groups
			(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
			 musicbrainz_url,source,first_observed_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?, 'musicbrainz',?,?)`,
			release.MBID, artistID, release.Title, release.PrimaryType, string(secondary),
			release.FirstReleaseDate, release.DatePrecision, release.MusicBrainzURL,
			timeText(observed), timeText(observed))
		if insertErr != nil {
			return syncedRelease{}, insertErr
		}
		releaseID, _ = result.LastInsertId()
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE release_groups SET
			title=?,primary_type=?,secondary_types=?,
			first_release_date=CASE WHEN ?>=date_precision THEN ? ELSE first_release_date END,
			date_precision=MAX(date_precision,?),musicbrainz_url=?,
			source=CASE WHEN spotify_id IS NULL THEN 'musicbrainz' ELSE 'both' END,updated_at=?
			WHERE id=?`,
			release.Title, release.PrimaryType, string(secondary),
			release.DatePrecision, release.FirstReleaseDate, release.DatePrecision, release.MusicBrainzURL,
			timeText(observed), releaseID)
		if err != nil {
			return syncedRelease{}, err
		}
	}
	if err := upsertProviderObservationTx(ctx, tx, "musicbrainz", release.MBID, releaseID, release, observed); err != nil {
		return syncedRelease{}, err
	}
	saved, err := releaseByIDTx(ctx, tx, releaseID)
	return syncedRelease{release: saved, isNew: !existed, provider: "musicbrainz"}, err
}

func saveSpotifyReleaseTx(
	ctx context.Context, tx *sql.Tx, artistID int64, release Release, observed time.Time,
) (syncedRelease, error) {
	if strings.TrimSpace(release.SpotifyID) == "" {
		return syncedRelease{}, errors.New("Spotify release ID is required")
	}
	var releaseID int64
	existed := true
	err := tx.QueryRowContext(ctx, `SELECT release_group_id FROM provider_observations
		WHERE provider='spotify' AND provider_id=?`, release.SpotifyID).Scan(&releaseID)
	if errors.Is(err, sql.ErrNoRows) {
		err = tx.QueryRowContext(ctx, `SELECT id FROM release_groups WHERE spotify_id=?`, release.SpotifyID).
			Scan(&releaseID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		releaseID, err = matchingReleaseIDTx(ctx, tx, artistID, release, false)
	}
	if errors.Is(err, sql.ErrNoRows) {
		existed, releaseID, err = false, 0, nil
	}
	if err != nil {
		return syncedRelease{}, err
	}
	secondary, _ := json.Marshal(release.SecondaryTypes)
	if releaseID == 0 {
		result, insertErr := tx.ExecContext(ctx, `INSERT INTO release_groups
			(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
			 musicbrainz_url,spotify_id,spotify_url,spotify_image_url,source,first_observed_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,'spotify',?,?)`,
			"spotify:"+release.SpotifyID, artistID, release.Title, release.PrimaryType, string(secondary),
			release.FirstReleaseDate, release.DatePrecision, "", release.SpotifyID, release.SpotifyURL,
			release.SpotifyImageURL, timeText(observed), timeText(observed))
		if insertErr != nil {
			return syncedRelease{}, insertErr
		}
		releaseID, _ = result.LastInsertId()
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE release_groups SET
			spotify_id=COALESCE(spotify_id,?),spotify_url=?,spotify_image_url=?,
			title=CASE WHEN source='spotify' THEN ? ELSE title END,
			primary_type=CASE WHEN source='spotify' THEN ? ELSE primary_type END,
			secondary_types=CASE WHEN source='spotify' THEN ? ELSE secondary_types END,
			first_release_date=CASE WHEN source='spotify' AND ?>=date_precision THEN ? ELSE first_release_date END,
			date_precision=CASE WHEN source='spotify' THEN MAX(date_precision,?) ELSE date_precision END,
			source=CASE WHEN source='musicbrainz' THEN 'both' ELSE source END,updated_at=?
			WHERE id=?`,
			release.SpotifyID, release.SpotifyURL, release.SpotifyImageURL,
			release.Title, release.PrimaryType, string(secondary),
			release.DatePrecision, release.FirstReleaseDate, release.DatePrecision,
			timeText(observed), releaseID)
		if err != nil {
			return syncedRelease{}, err
		}
	}
	if err := upsertProviderObservationTx(ctx, tx, "spotify", release.SpotifyID, releaseID, release, observed); err != nil {
		return syncedRelease{}, err
	}
	saved, err := releaseByIDTx(ctx, tx, releaseID)
	return syncedRelease{release: saved, isNew: !existed, provider: "spotify"}, err
}

func matchingReleaseIDTx(
	ctx context.Context, tx *sql.Tx, artistID int64, candidate Release, spotifyOnly bool,
) (int64, error) {
	sourceClause := "source IN ('musicbrainz','spotify','both')"
	if spotifyOnly {
		sourceClause = "source='spotify'"
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,title,primary_type,first_release_date,date_precision
		FROM release_groups WHERE artist_id=? AND `+sourceClause, artistID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var matches []int64
	for rows.Next() {
		var id int64
		var title, primaryType, releaseDate string
		var precision int
		if err := rows.Scan(&id, &title, &primaryType, &releaseDate, &precision); err != nil {
			return 0, err
		}
		existing := Release{
			Title: title, PrimaryType: primaryType, FirstReleaseDate: releaseDate, DatePrecision: precision,
		}
		if releaseRecordsMatch(existing, candidate) {
			matches = append(matches, id)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(matches) != 1 {
		return 0, sql.ErrNoRows
	}
	return matches[0], nil
}

func releaseRecordsMatch(a, b Release) bool {
	if a.PrimaryType != b.PrimaryType || normalizedReleaseTitle(a.Title) != normalizedReleaseTitle(b.Title) {
		return false
	}
	if a.DatePrecision == 0 || b.DatePrecision == 0 ||
		a.FirstReleaseDate == "" || b.FirstReleaseDate == "" {
		return false
	}
	length := min(len(a.FirstReleaseDate), len(b.FirstReleaseDate))
	if length < 4 {
		return false
	}
	return a.FirstReleaseDate[:length] == b.FirstReleaseDate[:length]
}

func normalizedReleaseTitle(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, pair := range [][2]string{{"(", ")"}, {"[", "]"}} {
		start := strings.LastIndex(value, pair[0])
		if start < 0 || !strings.HasSuffix(value, pair[1]) {
			continue
		}
		suffix := value[start:]
		for _, marker := range []string{"deluxe", "remaster", "expanded", "anniversary", "edition"} {
			if strings.Contains(suffix, marker) {
				value = strings.TrimSpace(value[:start])
				break
			}
		}
	}
	var normalized strings.Builder
	space := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			normalized.WriteRune(r)
			space = false
		} else if !space {
			normalized.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(normalized.String())
}

func upsertProviderObservationTx(
	ctx context.Context, tx *sql.Tx, provider, providerID string, releaseID int64,
	release Release, observed time.Time,
) error {
	secondary, _ := json.Marshal(release.SecondaryTypes)
	payloadHash := sha256.Sum256([]byte(release.Title + "\x00" + release.PrimaryType + "\x00" +
		string(secondary) + "\x00" + release.FirstReleaseDate + "\x00" + release.SpotifyURL))
	_, err := tx.ExecContext(ctx, `INSERT INTO provider_observations
		(provider,provider_id,release_group_id,payload_hash,observed_at) VALUES(?,?,?,?,?)
		ON CONFLICT(provider,provider_id) DO UPDATE SET release_group_id=excluded.release_group_id,
		payload_hash=excluded.payload_hash,observed_at=excluded.observed_at`,
		provider, providerID, releaseID, fmt.Sprintf("%x", payloadHash), timeText(observed))
	return err
}

func releaseByIDTx(ctx context.Context, tx *sql.Tx, releaseID int64) (Release, error) {
	var release Release
	var secondary, observed string
	var spotifyID, spotifyURL sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id,mbid,artist_id,title,primary_type,secondary_types,
		first_release_date,date_precision,musicbrainz_url,spotify_id,spotify_url,spotify_image_url,
		source,first_observed_at FROM release_groups WHERE id=?`, releaseID).Scan(
		&release.ID, &release.MBID, &release.ArtistID, &release.Title, &release.PrimaryType, &secondary,
		&release.FirstReleaseDate, &release.DatePrecision, &release.MusicBrainzURL,
		&spotifyID, &spotifyURL, &release.SpotifyImageURL, &release.Source, &observed,
	)
	if err != nil {
		return Release{}, err
	}
	_ = json.Unmarshal([]byte(secondary), &release.SecondaryTypes)
	release.SpotifyID, release.SpotifyURL = spotifyID.String, spotifyURL.String
	release.FirstObservedAt, _ = parseTime(observed)
	return release, nil
}

func selectInitialRelease(items []syncedRelease, observed time.Time) (syncedRelease, string, bool) {
	var zero syncedRelease
	today := dayUTC(observed)
	var upcoming syncedRelease
	var upcomingStart time.Time
	var latest syncedRelease
	var latestStart time.Time
	hasUpcoming, hasLatest := false, false
	for _, item := range items {
		release := item.release
		start, valid := comparableReleaseDate(release.FirstReleaseDate)
		if !valid {
			continue
		}
		if start.After(today) {
			if !hasUpcoming || start.Before(upcomingStart) ||
				(start.Equal(upcomingStart) && release.MBID < upcoming.release.MBID) {
				upcoming, upcomingStart, hasUpcoming = item, start, true
			}
			continue
		}
		if !hasLatest || start.After(latestStart) ||
			(start.Equal(latestStart) && release.MBID < latest.release.MBID) {
			latest, latestStart, hasLatest = item, start, true
		}
	}
	if hasUpcoming {
		return upcoming, "announcement", true
	}
	if !hasLatest {
		return zero, "", false
	}
	release := latest.release
	if release.DatePrecision == 3 && release.FirstReleaseDate == today.Format("2006-01-02") {
		return latest, "release_day", true
	}
	return latest, "announcement", true
}

func initialReleaseMessage(artist Artist, release Release, eventType string, observed time.Time) (string, string) {
	today := dayUTC(observed)
	start, _ := comparableReleaseDate(release.FirstReleaseDate)
	link := releaseExternalURL(release)
	if eventType == "release_day" {
		return "Released today: " + release.Title,
			fmt.Sprintf("%s's %q is out today.\n%s", artist.Name, release.Title, link)
	}
	if start.After(today) {
		return "Upcoming release from " + artist.Name,
			fmt.Sprintf("%s's %q is expected %s.\n%s", artist.Name, release.Title,
				release.FirstReleaseDate, link)
	}
	return "Latest release from " + artist.Name,
		fmt.Sprintf("%s's latest known release is %q (%s).\n%s", artist.Name, release.Title,
			release.FirstReleaseDate, link)
}

func releaseExternalURL(release Release) string {
	if release.SpotifyURL != "" {
		return release.SpotifyURL
	}
	return release.MusicBrainzURL
}

func comparableReleaseDate(value string) (time.Time, bool) {
	layout := ""
	switch len(value) {
	case 4:
		layout = "2006"
	case 7:
		layout = "2006-01"
	case 10:
		layout = "2006-01-02"
	default:
		return time.Time{}, false
	}
	parsed, err := time.Parse(layout, value)
	return parsed, err == nil
}

func (s *Store) QueueDueReleaseDays(ctx context.Context, now time.Time) error {
	rows, err := s.DB.QueryContext(ctx, `SELECT f.user_id,rg.id,u.timezone,u.reminder_time,a.name,rg.title,
		rg.first_release_date,rg.musicbrainz_url,rg.spotify_url
		FROM follows f JOIN users u ON u.id=f.user_id JOIN release_groups rg ON rg.artist_id=f.artist_id
		JOIN artists a ON a.id=rg.artist_id WHERE rg.date_precision=3`)
	if err != nil {
		return err
	}
	type due struct {
		userID, releaseID          int64
		timezone, reminder         string
		artist, title, releaseDate string
		musicBrainzURL             string
		spotifyURL                 sql.NullString
	}
	var candidates []due
	for rows.Next() {
		var d due
		if err := rows.Scan(
			&d.userID, &d.releaseID, &d.timezone, &d.reminder, &d.artist, &d.title,
			&d.releaseDate, &d.musicBrainzURL, &d.spotifyURL,
		); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, d)
	}
	rows.Close()
	for _, d := range candidates {
		location, err := time.LoadLocation(d.timezone)
		if err != nil {
			continue
		}
		localNow := now.In(location)
		if d.releaseDate != localNow.Format("2006-01-02") || localNow.Format("15:04") < d.reminder {
			continue
		}
		body := fmt.Sprintf("%s's %q is out today.", d.artist, d.title)
		if link := firstNonEmpty(d.musicBrainzURL, d.spotifyURL.String); link != "" {
			body += "\n" + link
		}
		if err := s.EnqueueEvent(ctx, d.userID, d.releaseID, "release_day",
			"Released today: "+d.title, body, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) EnqueueEvent(ctx context.Context, userID, releaseID int64, eventType, title, body string, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := enqueueEventTx(ctx, tx, userID, releaseID, eventType, title, body, now); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func enqueueEventTx(ctx context.Context, tx *sql.Tx, userID, releaseID int64, eventType, title, body string, now time.Time) error {
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO notification_events
		(user_id,release_group_id,event_type,title,body,created_at) VALUES(?,?,?,?,?,?)`,
		userID, releaseID, eventType, title, body, timeText(now))
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return nil
	}
	eventID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO deliveries(event_id,destination_id,status,next_attempt_at)
		SELECT ?,id,'pending',? FROM destinations WHERE user_id=? AND enabled=1`, eventID, timeText(now), userID)
	return err
}

func (s *Store) RecentReleases(ctx context.Context, userID int64, limit int) ([]Release, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT rg.id,rg.mbid,rg.artist_id,a.name,rg.title,rg.primary_type,
		rg.secondary_types,rg.first_release_date,rg.date_precision,rg.musicbrainz_url,
		rg.spotify_id,rg.spotify_url,rg.spotify_image_url,rg.source,rg.first_observed_at
		FROM release_groups rg JOIN artists a ON a.id=rg.artist_id JOIN follows f ON f.artist_id=rg.artist_id
		WHERE f.user_id=? ORDER BY CASE WHEN rg.first_release_date='' THEN '0000' ELSE rg.first_release_date END DESC LIMIT ?`,
		userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReleases(rows)
}

func (s *Store) DashboardReleases(
	ctx context.Context, userID int64, today string, limit int,
) (upcoming []Release, recent []Release, err error) {
	const definitelyFuture = `(
		(rg.date_precision=3 AND length(rg.first_release_date)=10
			AND date(rg.first_release_date) IS NOT NULL AND rg.first_release_date>?)
		OR (rg.date_precision=2 AND length(rg.first_release_date)=7
			AND date(rg.first_release_date || '-01') IS NOT NULL AND rg.first_release_date>substr(?,1,7))
		OR (rg.date_precision=1 AND length(rg.first_release_date)=4
			AND date(rg.first_release_date || '-01-01') IS NOT NULL AND rg.first_release_date>substr(?,1,4))
	)`
	const preferredProvider = `(a.spotify_id IS NULL OR rg.source IN ('spotify','both') OR NOT EXISTS (
		SELECT 1 FROM release_groups newer WHERE newer.artist_id=rg.artist_id AND newer.source IN ('spotify','both')
	))`
	upcomingRows, err := s.DB.QueryContext(ctx, `SELECT rg.id,rg.mbid,rg.artist_id,a.name,rg.title,rg.primary_type,
		rg.secondary_types,rg.first_release_date,rg.date_precision,rg.musicbrainz_url,
		rg.spotify_id,rg.spotify_url,rg.spotify_image_url,rg.source,rg.first_observed_at
		FROM release_groups rg JOIN artists a ON a.id=rg.artist_id JOIN follows f ON f.artist_id=rg.artist_id
		WHERE f.user_id=? AND `+preferredProvider+` AND `+definitelyFuture+`
		ORDER BY rg.first_release_date ASC,rg.id ASC LIMIT ?`,
		userID, today, today, today, limit)
	if err != nil {
		return nil, nil, err
	}
	upcoming, err = scanReleases(upcomingRows)
	upcomingRows.Close()
	if err != nil {
		return nil, nil, err
	}
	recentRows, err := s.DB.QueryContext(ctx, `SELECT rg.id,rg.mbid,rg.artist_id,a.name,rg.title,rg.primary_type,
		rg.secondary_types,rg.first_release_date,rg.date_precision,rg.musicbrainz_url,
		rg.spotify_id,rg.spotify_url,rg.spotify_image_url,rg.source,rg.first_observed_at
		FROM release_groups rg JOIN artists a ON a.id=rg.artist_id JOIN follows f ON f.artist_id=rg.artist_id
		WHERE f.user_id=? AND `+preferredProvider+` AND NOT COALESCE(`+definitelyFuture+`,0)
		ORDER BY CASE WHEN rg.first_release_date='' THEN '0000' ELSE rg.first_release_date END DESC,rg.id DESC LIMIT ?`,
		userID, today, today, today, limit)
	if err != nil {
		return nil, nil, err
	}
	recent, err = scanReleases(recentRows)
	recentRows.Close()
	if err != nil {
		return nil, nil, err
	}
	return upcoming, recent, nil
}

func scanReleases(rows *sql.Rows) ([]Release, error) {
	var result []Release
	for rows.Next() {
		var r Release
		var secondary, observed string
		var spotifyID, spotifyURL sql.NullString
		if err := rows.Scan(&r.ID, &r.MBID, &r.ArtistID, &r.ArtistName, &r.Title, &r.PrimaryType,
			&secondary, &r.FirstReleaseDate, &r.DatePrecision, &r.MusicBrainzURL,
			&spotifyID, &spotifyURL, &r.SpotifyImageURL, &r.Source, &observed); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(secondary), &r.SecondaryTypes)
		r.SpotifyID, r.SpotifyURL = spotifyID.String, spotifyURL.String
		r.FirstObservedAt, _ = parseTime(observed)
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *Store) AddDestination(ctx context.Context, userID int64, name, service string, encrypted []byte) error {
	name, err := destinationName(name)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO destinations(user_id,name,service,encrypted_url,created_at)
		VALUES(?,?,?,?,?)`, userID, name, service, encrypted, nowText())
	return err
}

func (s *Store) RenameDestination(ctx context.Context, userID, destinationID int64, name string) error {
	name, err := destinationName(name)
	if err != nil {
		return err
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE destinations SET name=? WHERE id=? AND user_id=?`,
		name, destinationID, userID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func destinationName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("destination name is required")
	}
	if utf8.RuneCountInString(value) > 80 {
		return "", errors.New("destination name must be 80 characters or fewer")
	}
	return value, nil
}

func (s *Store) Destinations(ctx context.Context, userID int64) ([]Destination, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id,user_id,name,service,encrypted_url,enabled
		FROM destinations WHERE user_id=? ORDER BY name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Destination
	for rows.Next() {
		var d Destination
		if err := rows.Scan(&d.ID, &d.UserID, &d.Name, &d.Service, &d.EncryptedURL, &d.Enabled); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

func (s *Store) Destination(ctx context.Context, userID, id int64) (Destination, error) {
	var d Destination
	err := s.DB.QueryRowContext(ctx, `SELECT id,user_id,name,service,encrypted_url,enabled
		FROM destinations WHERE user_id=? AND id=?`, userID, id).Scan(
		&d.ID, &d.UserID, &d.Name, &d.Service, &d.EncryptedURL, &d.Enabled)
	return d, err
}

func (s *Store) DeleteDestination(ctx context.Context, userID, id int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM destinations WHERE user_id=? AND id=?`, userID, id)
	return err
}

func (s *Store) DueDeliveries(ctx context.Context, now time.Time, limit int) ([]Delivery, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT d.id,d.event_id,d.attempts,d.next_attempt_at,
		dst.id,dst.user_id,dst.name,dst.service,dst.encrypted_url,dst.enabled,
		e.title,e.body,e.event_type,rg.title
		FROM deliveries d JOIN destinations dst ON dst.id=d.destination_id
		JOIN notification_events e ON e.id=d.event_id JOIN release_groups rg ON rg.id=e.release_group_id
		WHERE d.status='pending' AND d.next_attempt_at<=? AND dst.enabled=1 ORDER BY d.next_attempt_at LIMIT ?`,
		timeText(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Delivery
	for rows.Next() {
		var d Delivery
		var next string
		if err := rows.Scan(&d.ID, &d.EventID, &d.Attempts, &next,
			&d.Destination.ID, &d.Destination.UserID, &d.Destination.Name, &d.Destination.Service,
			&d.Destination.EncryptedURL, &d.Destination.Enabled, &d.Title, &d.Body, &d.EventType, &d.ReleaseTitle); err != nil {
			return nil, err
		}
		d.NextAttempt, _ = parseTime(next)
		result = append(result, d)
	}
	return result, rows.Err()
}

func (s *Store) MarkDeliverySent(ctx context.Context, id int64, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE deliveries SET status='sent',attempts=attempts+1,sent_at=?,last_error='' WHERE id=?`,
		timeText(now), id)
	return err
}

func (s *Store) MarkDeliveryFailed(ctx context.Context, id int64, attempts int, message string, now time.Time) error {
	status := "pending"
	if attempts >= 5 {
		status = "failed"
	}
	delay := time.Minute * time.Duration(1<<min(attempts, 6))
	if len(message) > 500 {
		message = message[:500]
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE deliveries SET status=?,attempts=?,next_attempt_at=?,last_error=? WHERE id=?`,
		status, attempts, timeText(now.Add(delay)), message, id)
	return err
}

func (s *Store) DeliveryHistory(ctx context.Context, userID int64, limit int) ([]DeliveryHistory, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT e.title,e.event_type,dst.name,d.status,d.attempts,d.last_error,e.created_at,d.sent_at
		FROM notification_events e LEFT JOIN deliveries d ON d.event_id=e.id
		LEFT JOIN destinations dst ON dst.id=d.destination_id WHERE e.user_id=?
		ORDER BY e.created_at DESC,d.id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []DeliveryHistory
	for rows.Next() {
		var h DeliveryHistory
		var dest, status sql.NullString
		var attempts sql.NullInt64
		var created string
		var sent sql.NullString
		var lastError sql.NullString
		if err := rows.Scan(&h.Title, &h.EventType, &dest, &status, &attempts, &lastError, &created, &sent); err != nil {
			return nil, err
		}
		h.Destination, h.Status, h.Attempts, h.LastError = dest.String, status.String, int(attempts.Int64), lastError.String
		if h.Destination == "" {
			h.Destination, h.Status = "No destination configured", "not sent"
		}
		h.CreatedAt, _ = parseTime(created)
		if sent.Valid {
			t, _ := parseTime(sent.String)
			h.SentAt = &t
		}
		result = append(result, h)
	}
	return result, rows.Err()
}

func (s *Store) AdminDeliveryHistoryCount(ctx context.Context) (int, error) {
	var count int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM notification_events e
		LEFT JOIN deliveries d ON d.event_id=e.id`).Scan(&count)
	return count, err
}

func (s *Store) AdminDeliveryHistory(ctx context.Context, limit, offset int) ([]AdminDeliveryHistory, error) {
	if limit < 1 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT u.email,e.title,e.body,e.event_type,
		dst.name,dst.service,d.status,d.attempts,d.last_error,e.created_at,d.next_attempt_at,d.sent_at
		FROM notification_events e
		JOIN users u ON u.id=e.user_id
		LEFT JOIN deliveries d ON d.event_id=e.id
		LEFT JOIN destinations dst ON dst.id=d.destination_id
		ORDER BY e.created_at DESC,d.id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AdminDeliveryHistory
	for rows.Next() {
		var h AdminDeliveryHistory
		var destination, service, status, lastError sql.NullString
		var attempts sql.NullInt64
		var created string
		var nextAttempt, sent sql.NullString
		if err := rows.Scan(
			&h.UserEmail, &h.Title, &h.Body, &h.EventType,
			&destination, &service, &status, &attempts, &lastError,
			&created, &nextAttempt, &sent,
		); err != nil {
			return nil, err
		}
		h.Destination, h.Service = destination.String, service.String
		h.Status, h.Attempts, h.LastError = status.String, int(attempts.Int64), lastError.String
		if h.Destination == "" {
			h.Destination, h.Status = "No destination configured", "not sent"
		}
		h.CreatedAt, _ = parseTime(created)
		if nextAttempt.Valid {
			t, _ := parseTime(nextAttempt.String)
			h.NextAttempt = &t
		}
		if sent.Valid {
			t, _ := parseTime(sent.String)
			h.SentAt = &t
		}
		result = append(result, h)
	}
	return result, rows.Err()
}

func releaseDate(value string) (time.Time, bool) {
	if len(value) != 10 {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", value)
	return t, err == nil
}

func dayUTC(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func nowText() string                       { return timeText(time.Now().UTC()) }
func timeText(t time.Time) string           { return t.UTC().Format(time.RFC3339Nano) }
func parseTime(v string) (time.Time, error) { return time.Parse(time.RFC3339Nano, v) }

func (s *Store) InsertApplicationLog(ctx context.Context, entry logging.Entry) error {
	attrs, err := json.Marshal(entry.Attributes)
	if err != nil {
		return err
	}
	level := strings.ToUpper(entry.Level)
	if level == "WARNING" {
		level = "WARN"
	}
	if level != "INFO" && level != "WARN" && level != "ERROR" {
		return nil
	}
	_, err = s.DB.ExecContext(ctx, `INSERT INTO application_logs(created_at,level,message,attributes_json) VALUES(?,?,?,?)`, timeText(entry.Time), level, entry.Message, string(attrs))
	return err
}

func (s *Store) ApplicationLogs(ctx context.Context, limit int) ([]logging.Entry, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT created_at,level,message,attributes_json FROM application_logs ORDER BY created_at DESC,id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []logging.Entry
	for rows.Next() {
		var ts, level, msg, attrs string
		if err := rows.Scan(&ts, &level, &msg, &attrs); err != nil {
			return nil, err
		}
		t, _ := parseTime(ts)
		var fields []logging.Field
		_ = json.Unmarshal([]byte(attrs), &fields)
		out = append(out, logging.Entry{Time: t, Level: level, Message: msg, Attributes: fields})
	}
	return out, rows.Err()
}

func (s *Store) PruneApplicationLogs(ctx context.Context, before time.Time) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM application_logs WHERE created_at < ?`, timeText(before))
	return err
}

func (s *Store) CreateManualSyncRequest(ctx context.Context, userID int64, scope string, artistID *int64) (ManualSyncRequest, error) {
	if scope != "artist" && scope != "retry" {
		return ManualSyncRequest{}, errors.New("invalid sync scope")
	}
	var q string
	var args []any
	if scope == "artist" {
		if artistID == nil {
			return ManualSyncRequest{}, errors.New("artist is required")
		}
		q = `SELECT id FROM manual_sync_requests WHERE scope='artist' AND artist_id=? AND status IN ('queued','running') LIMIT 1`
		args = []any{*artistID}
	} else {
		q = `SELECT id FROM manual_sync_requests WHERE scope='retry' AND status IN ('queued','running') LIMIT 1`
	}
	var existing int64
	if err := s.DB.QueryRowContext(ctx, q, args...).Scan(&existing); err == nil {
		return ManualSyncRequest{ID: existing, Scope: scope, Status: "queued"}, nil
	} else if err != sql.ErrNoRows {
		return ManualSyncRequest{}, err
	}
	res, err := s.DB.ExecContext(ctx, `INSERT INTO manual_sync_requests(requested_by,scope,artist_id,status,created_at) VALUES(?,?,?,?,?)`, userID, scope, artistID, "queued", nowText())
	if err != nil {
		return ManualSyncRequest{}, err
	}
	id, _ := res.LastInsertId()
	return ManualSyncRequest{ID: id, RequestedBy: userID, Scope: scope, ArtistID: artistID, Status: "queued", CreatedAt: time.Now().UTC()}, nil
}

func (s *Store) ClaimManualSyncRequests(ctx context.Context, limit int) ([]ManualSyncRequest, error) {
	if limit < 1 {
		limit = 1
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,requested_by,scope,artist_id,created_at FROM manual_sync_requests WHERE status='queued' ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	var out []ManualSyncRequest
	for rows.Next() {
		var r ManualSyncRequest
		var aid sql.NullInt64
		var ts string
		if err := rows.Scan(&r.ID, &r.RequestedBy, &r.Scope, &aid, &ts); err != nil {
			return nil, err
		}
		if aid.Valid {
			v := aid.Int64
			r.ArtistID = &v
		}
		r.Status = "running"
		r.CreatedAt, _ = parseTime(ts)
		out = append(out, r)
		ids = append(ids, r.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	now := nowText()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE manual_sync_requests SET status='running',started_at=? WHERE id=?`, now, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for i := range out {
		t, _ := parseTime(now)
		out[i].StartedAt = &t
	}
	return out, nil
}

func (s *Store) CompleteManualSyncRequest(ctx context.Context, id int64, syncErr error) error {
	status, msg := "completed", ""
	if syncErr != nil {
		status = "failed"
		msg = syncErr.Error()
	}
	if len(msg) > 500 {
		msg = msg[:500]
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE manual_sync_requests SET status=?,finished_at=?,last_error=? WHERE id=?`, status, nowText(), msg, id)
	return err
}
func (s *Store) ManualSyncRequests(ctx context.Context, limit int) ([]ManualSyncRequest, error) {
	if limit < 1 {
		limit = 20
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,requested_by,scope,artist_id,status,created_at,started_at,finished_at,last_error FROM manual_sync_requests ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ManualSyncRequest
	for rows.Next() {
		var r ManualSyncRequest
		var aid sql.NullInt64
		var c, st, ft string
		if err := rows.Scan(&r.ID, &r.RequestedBy, &r.Scope, &aid, &r.Status, &c, &st, &ft, &r.LastError); err != nil {
			return nil, err
		}
		r.CreatedAt, _ = parseTime(c)
		if aid.Valid {
			v := aid.Int64
			r.ArtistID = &v
		}
		if st != "" {
			v, _ := parseTime(st)
			r.StartedAt = &v
		}
		if ft != "" {
			v, _ := parseTime(ft)
			r.FinishedAt = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) UpsertProviderHealth(ctx context.Context, provider string, success bool, next *time.Time, rateLimited, quota bool, lastError string) error {
	now := nowText()
	if len(lastError) > 500 {
		lastError = lastError[:500]
	}
	if success {
		_, err := s.DB.ExecContext(ctx, `INSERT INTO provider_health(provider,last_success_at,last_error,next_check_at,rate_limited,quota_exceeded,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(provider) DO UPDATE SET last_success_at=excluded.last_success_at,last_error='',next_check_at=excluded.next_check_at,rate_limited=0,quota_exceeded=0,updated_at=excluded.updated_at`, provider, now, "", nullableTime(next), 0, 0, now)
		return err
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO provider_health(provider,last_failure_at,last_error,next_check_at,rate_limited,quota_exceeded,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(provider) DO UPDATE SET last_failure_at=excluded.last_failure_at,last_error=excluded.last_error,next_check_at=excluded.next_check_at,rate_limited=excluded.rate_limited,quota_exceeded=excluded.quota_exceeded,updated_at=excluded.updated_at`, provider, now, lastError, nullableTime(next), boolInt(rateLimited), boolInt(quota), now)
	return err
}
func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return timeText(*t)
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func (s *Store) ProviderHealth(ctx context.Context) ([]ProviderHealth, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT provider,last_success_at,last_failure_at,last_error,next_check_at,rate_limited,quota_exceeded,updated_at FROM provider_health ORDER BY provider`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProviderHealth
	for rows.Next() {
		var p ProviderHealth
		var ls, lf, n, u sql.NullString
		var rl, qe int
		if err := rows.Scan(&p.Provider, &ls, &lf, &p.LastError, &n, &rl, &qe, &u); err != nil {
			return nil, err
		}
		if ls.Valid {
			v, _ := parseTime(ls.String)
			p.LastSuccessAt = &v
		}
		if lf.Valid {
			v, _ := parseTime(lf.String)
			p.LastFailureAt = &v
		}
		if n.Valid {
			v, _ := parseTime(n.String)
			p.NextCheckAt = &v
		}
		p.RateLimited = rl != 0
		p.QuotaExceeded = qe != 0
		p.UpdatedAt, _ = parseTime(u.String)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) MarkAllArtistsDue(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE artists SET next_check_at=? WHERE id IN (SELECT DISTINCT artist_id FROM follows)`, nowText())
	return err
}
func (s *Store) ArtistByID(ctx context.Context, id int64) (Artist, error) {
	var a Artist
	var sid, surl, simg, checked, next sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT id,mbid,name,sort_name,artist_type,country,disambiguation,spotify_id,spotify_url,spotify_image_url,last_checked_at,spotify_next_check_at,baseline_synced FROM artists WHERE id=?`, id).Scan(&a.ID, &a.MBID, &a.Name, &a.SortName, &a.Type, &a.Country, &a.Disambiguation, &sid, &surl, &simg, &checked, &next, &a.BaselineSynced)
	a.SpotifyID, a.SpotifyURL, a.SpotifyImageURL = sid.String, surl.String, simg.String
	if checked.Valid {
		t, _ := parseTime(checked.String)
		a.LastCheckedAt = &t
	}
	if next.Valid {
		t, _ := parseTime(next.String)
		a.SpotifyNextCheckAt = &t
	}
	return a, err
}
func (s *Store) AdminArtists(ctx context.Context) ([]AdminArtist, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT a.id,a.name,a.mbid FROM artists a JOIN follows f ON f.artist_id=a.id ORDER BY a.sort_name,a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminArtist
	for rows.Next() {
		var a AdminArtist
		if err := rows.Scan(&a.ID, &a.Name, &a.MBID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
