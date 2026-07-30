package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"

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

type Session struct {
	User      User
	CSRFToken string
	ExpiresAt time.Time
}

type Artist struct {
	ID              int64
	MBID            string
	Name            string
	SortName        string
	Type            string
	Country         string
	Disambiguation  string
	SpotifyID       string
	SpotifyURL      string
	SpotifyImageURL string
	LastCheckedAt   *time.Time
	BaselineSynced  bool
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
	FirstObservedAt  time.Time
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

type ImportRow struct {
	ArtistID    int64
	SourceValue string
	DisplayName string
	Status      string
	ArtistName  string
	Reason      string
}

type syncedRelease struct {
	release Release
	isNew   bool
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

func (s *Store) FollowedArtists(ctx context.Context, userID int64) ([]Artist, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT a.id,a.mbid,a.name,a.sort_name,a.artist_type,a.country,a.disambiguation,
		a.spotify_id,a.spotify_url,a.spotify_image_url,a.last_checked_at,f.baseline_synced_at
		FROM follows f JOIN artists a ON a.id=f.artist_id WHERE f.user_id=? ORDER BY a.sort_name,a.name`, userID)
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

func (s *Store) ArtistsDue(ctx context.Context, now time.Time, limit int) ([]Artist, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT DISTINCT a.id,a.mbid,a.name,a.sort_name,a.artist_type,a.country,a.disambiguation
		FROM artists a JOIN follows f ON f.artist_id=a.id
		WHERE a.next_check_at IS NULL OR a.next_check_at<=? ORDER BY COALESCE(a.next_check_at,'') LIMIT ?`,
		timeText(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Artist
	for rows.Next() {
		var a Artist
		if err := rows.Scan(&a.ID, &a.MBID, &a.Name, &a.SortName, &a.Type, &a.Country, &a.Disambiguation); err != nil {
			return nil, err
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

func (s *Store) ApplyReleaseSync(ctx context.Context, artist Artist, releases []Release, observed time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	var savedReleases []syncedRelease
	for _, release := range releases {
		secondary, _ := json.Marshal(release.SecondaryTypes)
		var existed int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM release_groups WHERE mbid=?`, release.MBID).Scan(&existed); err != nil {
			tx.Rollback()
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO release_groups(mbid,artist_id,title,primary_type,secondary_types,
			first_release_date,date_precision,musicbrainz_url,first_observed_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(mbid) DO UPDATE SET title=excluded.title,primary_type=excluded.primary_type,
			secondary_types=excluded.secondary_types,first_release_date=excluded.first_release_date,
			date_precision=excluded.date_precision,updated_at=excluded.updated_at`,
			release.MBID, artist.ID, release.Title, release.PrimaryType, string(secondary),
			release.FirstReleaseDate, release.DatePrecision, release.MusicBrainzURL, timeText(observed), timeText(observed))
		if err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT id,first_observed_at FROM release_groups WHERE mbid=?`, release.MBID).
			Scan(&release.ID, new(string)); err != nil {
			tx.Rollback()
			return err
		}
		payloadHash := sha256.Sum256([]byte(release.Title + "\x00" + release.PrimaryType + "\x00" +
			string(secondary) + "\x00" + release.FirstReleaseDate))
		if _, err := tx.ExecContext(ctx, `INSERT INTO provider_observations
			(provider,provider_id,release_group_id,payload_hash,observed_at) VALUES('musicbrainz',?,?,?,?)
			ON CONFLICT(provider,provider_id) DO UPDATE SET release_group_id=excluded.release_group_id,
			payload_hash=excluded.payload_hash,observed_at=excluded.observed_at`,
			release.MBID, release.ID, fmt.Sprintf("%x", payloadHash), timeText(observed)); err != nil {
			tx.Rollback()
			return err
		}
		savedReleases = append(savedReleases, syncedRelease{release: release, isNew: existed == 0})
	}
	rows, err := tx.QueryContext(ctx, `SELECT user_id,baseline_synced_at FROM follows WHERE artist_id=?`, artist.ID)
	if err != nil {
		tx.Rollback()
		return err
	}
	type follower struct {
		id       int64
		baseline bool
	}
	var followers []follower
	for rows.Next() {
		var f follower
		var baseline sql.NullString
		if err := rows.Scan(&f.id, &baseline); err != nil {
			rows.Close()
			tx.Rollback()
			return err
		}
		f.baseline = baseline.Valid
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
			continue
		}
		for _, item := range savedReleases {
			date, full := releaseDate(item.release.FirstReleaseDate)
			if !item.isNew || !full || date.Before(dayUTC(observed).AddDate(0, 0, -7)) {
				continue
			}
			if err := enqueueEventTx(ctx, tx, follower.id, item.release.ID, "announcement",
				"New release from "+artist.Name,
				fmt.Sprintf("%s has announced %q for %s.\n%s", artist.Name, item.release.Title,
					item.release.FirstReleaseDate, item.release.MusicBrainzURL),
				observed); err != nil {
				tx.Rollback()
				return err
			}
		}
	}
	return tx.Commit()
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
	if eventType == "release_day" {
		return "Released today: " + release.Title,
			fmt.Sprintf("%s's %q is out today.\n%s", artist.Name, release.Title, release.MusicBrainzURL)
	}
	if start.After(today) {
		return "Upcoming release from " + artist.Name,
			fmt.Sprintf("%s's %q is expected %s.\n%s", artist.Name, release.Title,
				release.FirstReleaseDate, release.MusicBrainzURL)
	}
	return "Latest release from " + artist.Name,
		fmt.Sprintf("%s's latest known release is %q (%s).\n%s", artist.Name, release.Title,
			release.FirstReleaseDate, release.MusicBrainzURL)
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
	rows, err := s.DB.QueryContext(ctx, `SELECT f.user_id,rg.id,u.timezone,u.reminder_time,a.name,rg.title,rg.first_release_date
		FROM follows f JOIN users u ON u.id=f.user_id JOIN release_groups rg ON rg.artist_id=f.artist_id
		JOIN artists a ON a.id=rg.artist_id WHERE rg.date_precision=3`)
	if err != nil {
		return err
	}
	type due struct {
		userID, releaseID          int64
		timezone, reminder         string
		artist, title, releaseDate string
	}
	var candidates []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.userID, &d.releaseID, &d.timezone, &d.reminder, &d.artist, &d.title, &d.releaseDate); err != nil {
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
		if err := s.EnqueueEvent(ctx, d.userID, d.releaseID, "release_day",
			"Released today: "+d.title, fmt.Sprintf("%s's %q is out today.", d.artist, d.title), now); err != nil {
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
		rg.secondary_types,rg.first_release_date,rg.date_precision,rg.musicbrainz_url,rg.first_observed_at
		FROM release_groups rg JOIN artists a ON a.id=rg.artist_id JOIN follows f ON f.artist_id=rg.artist_id
		WHERE f.user_id=? ORDER BY CASE WHEN rg.first_release_date='' THEN '0000' ELSE rg.first_release_date END DESC LIMIT ?`,
		userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Release
	for rows.Next() {
		var r Release
		var secondary, observed string
		if err := rows.Scan(&r.ID, &r.MBID, &r.ArtistID, &r.ArtistName, &r.Title, &r.PrimaryType,
			&secondary, &r.FirstReleaseDate, &r.DatePrecision, &r.MusicBrainzURL, &observed); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(secondary), &r.SecondaryTypes)
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

func (s *Store) CreateImportJob(ctx context.Context, userID int64, rows []ImportRow) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO import_jobs(user_id,created_at) VALUES(?,?)`, userID, nowText())
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	id, _ := result.LastInsertId()
	for _, row := range rows {
		var artistID any
		if row.ArtistID > 0 {
			artistID = row.ArtistID
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO import_rows(job_id,source_value,display_name,status,artist_id,reason)
			VALUES(?,?,?,?,?,?)`, id, row.SourceValue, row.DisplayName, row.Status, artistID, row.Reason)
		if err != nil {
			tx.Rollback()
			return 0, err
		}
	}
	return id, tx.Commit()
}

func (s *Store) ImportRows(ctx context.Context, userID, jobID int64) ([]ImportRow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT r.source_value,r.display_name,r.status,COALESCE(a.name,''),r.reason
		FROM import_rows r JOIN import_jobs j ON j.id=r.job_id LEFT JOIN artists a ON a.id=r.artist_id
		WHERE j.user_id=? AND j.id=? ORDER BY r.id`, userID, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ImportRow
	for rows.Next() {
		var r ImportRow
		if err := rows.Scan(&r.SourceValue, &r.DisplayName, &r.Status, &r.ArtistName, &r.Reason); err != nil {
			return nil, err
		}
		result = append(result, r)
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

func nowText() string                       { return timeText(time.Now().UTC()) }
func timeText(t time.Time) string           { return t.UTC().Format(time.RFC3339Nano) }
func parseTime(v string) (time.Time, error) { return time.Parse(time.RFC3339Nano, v) }
