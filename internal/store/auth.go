package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/crypt0rr/artist-tracker/internal/security"
)

func (s *Store) UserCount(ctx context.Context) (int, error) {
	var n int
	return n, s.readerDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
}
func validateUsername(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) < 3 || len(value) > 32 {
		return "", ErrInvalidUsername
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return "", ErrInvalidUsername
	}
	return value, nil
}
func derivedUsername(email string, id int64, taken map[string]struct{}) string {
	local := email
	if at := strings.IndexByte(local, '@'); at >= 0 {
		local = local[:at]
	}
	var b strings.Builder
	lastSeparator := false
	for _, r := range strings.ToLower(local) {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		if valid {
			b.WriteRune(r)
			lastSeparator = r == '.' || r == '_' || r == '-'
		} else if !lastSeparator && b.Len() > 0 {
			b.WriteByte('-')
			lastSeparator = true
		}
	}
	base := strings.Trim(b.String(), "._-")
	if len(base) < 3 {
		base = fmt.Sprintf("user-%d", id)
	}
	if len(base) > 32 {
		base = base[:32]
	}
	for ordinal := 1; ; ordinal++ {
		candidate := base
		if ordinal > 1 {
			suffix := fmt.Sprintf("-%d", ordinal)
			candidate = base
			if len(candidate)+len(suffix) > 32 {
				candidate = candidate[:32-len(suffix)]
			}
			candidate += suffix
		}
		if _, exists := taken[strings.ToLower(candidate)]; !exists {
			return candidate
		}
	}
}

func (s *Store) CreateUser(ctx context.Context, email, hash, role, timezone, username string) (int64, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") {
		return 0, errors.New("a valid email address is required")
	}
	if _, err := time.LoadLocation(timezone); err != nil {
		return 0, errors.New("invalid IANA timezone")
	}
	username = strings.TrimSpace(username)
	if username == "" {
		var id int64
		if err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0)+1 FROM users`).Scan(&id); err != nil {
			return 0, err
		}
		var existing []string
		rows, err := s.DB.QueryContext(ctx, `SELECT username FROM users WHERE username<>''`)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close()
				return 0, err
			}
			existing = append(existing, name)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return 0, err
		}
		_ = rows.Close()
		taken := make(map[string]struct{}, len(existing))
		for _, name := range existing {
			taken[strings.ToLower(name)] = struct{}{}
		}
		username = derivedUsername(email, id, taken)
	}
	validatedUsername, err := validateUsername(username)
	if err != nil {
		return 0, err
	}
	username = validatedUsername
	result, err := s.DB.ExecContext(ctx, `INSERT INTO users(email,username,password_hash,role,timezone,created_at)
		VALUES(?,?,?,?,?,?)`, email, username, hash, role, timezone, nowText())
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "users.username") || strings.Contains(strings.ToLower(err.Error()), "username") {
			return 0, ErrUsernameTaken
		}
		return 0, err
	}
	return result.LastInsertId()
}
func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var created string
	err := row.Scan(&u.ID, &u.Email, &u.Username, &u.PasswordHash, &u.Role, &u.Timezone, &u.ReminderTime, &created)
	u.CreatedAt, _ = parseTime(created)
	return u, err
}
func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(s.readerDB().QueryRowContext(ctx, `SELECT id,email,username,password_hash,role,timezone,reminder_time,created_at
		FROM users WHERE email=?`, strings.ToLower(strings.TrimSpace(email))))
}
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	return scanUser(s.readerDB().QueryRowContext(ctx, `SELECT id,email,username,password_hash,role,timezone,reminder_time,created_at
		FROM users WHERE id=?`, id))
}
func (s *Store) AdminUsers(ctx context.Context) ([]AdminUser, error) {
	rows, err := s.readerDB().QueryContext(ctx, `SELECT u.id,u.email,u.username,u.role,u.timezone,u.reminder_time,u.created_at,
		COUNT(DISTINCT f.artist_id),COUNT(DISTINCT d.id)
		FROM users u
		LEFT JOIN follows f ON f.user_id=u.id
		LEFT JOIN destinations d ON d.user_id=u.id
		GROUP BY u.id,u.email,u.username,u.role,u.timezone,u.reminder_time,u.created_at
		ORDER BY CASE WHEN u.role='admin' THEN 0 ELSE 1 END,lower(u.email)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var users []AdminUser
	for rows.Next() {
		var user AdminUser
		var created string
		if err := rows.Scan(
			&user.ID, &user.Email, &user.Username, &user.Role, &user.Timezone, &user.ReminderTime, &created,
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
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
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
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, hash, userID); err == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID)
	}
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
func (s *Store) UpdateProfile(ctx context.Context, userID int64, timezone, reminder, username string) error {
	if _, err := time.LoadLocation(timezone); err != nil {
		return errors.New("invalid IANA timezone")
	}
	if _, err := time.Parse("15:04", reminder); err != nil {
		return errors.New("reminder time must use HH:MM")
	}
	username, err := validateUsername(username)
	if err != nil {
		return err
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE users SET username=?, timezone=?, reminder_time=? WHERE id=?`, username, timezone, reminder, userID)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "username") {
		return ErrUsernameTaken
	}
	return err
}

// CreateUserFromInvite consumes an invitation and creates its account in one
// transaction. Validation and uniqueness failures therefore leave the token
// available for correction and retry.
func (s *Store) CreateUserFromInvite(ctx context.Context, raw, hash, username, timezone string) error {
	if _, err := time.LoadLocation(timezone); err != nil {
		return errors.New("invalid IANA timezone")
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var email string
	if err := tx.QueryRowContext(ctx, `SELECT email FROM auth_tokens WHERE token_hash=? AND kind='invite' AND used_at IS NULL AND expires_at>?`, security.Digest(raw), nowText()).Scan(&email); err != nil {
		return err
	}
	if strings.TrimSpace(username) == "" {
		var nextID int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0)+1 FROM users`).Scan(&nextID); err != nil {
			return err
		}
		taken := make(map[string]struct{})
		rows, err := tx.QueryContext(ctx, `SELECT username FROM users WHERE username<>''`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close()
				return err
			}
			taken[strings.ToLower(name)] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		username = derivedUsername(email, nextID, taken)
	}
	username, err = validateUsername(username)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO users(email,username,password_hash,role,timezone,created_at) VALUES(?,?,?,'member',?,?)`, email, username, hash, timezone, nowText())
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "username") {
			return ErrUsernameTaken
		}
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE auth_tokens SET used_at=? WHERE token_hash=? AND kind='invite'`, nowText(), security.Digest(raw)); err != nil {
		return err
	}
	return tx.Commit()
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
	err := s.readerDB().QueryRowContext(ctx, `SELECT u.id,u.email,u.username,u.password_hash,u.role,u.timezone,u.reminder_time,u.created_at,
		s.csrf_token,s.expires_at FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token_hash=? AND s.expires_at>?`, security.Digest(raw), nowText()).Scan(
		&session.User.ID, &session.User.Email, &session.User.Username, &session.User.PasswordHash, &session.User.Role,
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
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return "", nil, err
	}
	var id sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT email,user_id FROM auth_tokens
		WHERE token_hash=? AND kind=? AND used_at IS NULL AND expires_at>?`,
		security.Digest(raw), kind, nowText()).Scan(&email, &id)
	if err != nil {
		_ = tx.Rollback()
		return "", nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE auth_tokens SET used_at=? WHERE token_hash=?`, nowText(), security.Digest(raw)); err != nil {
		_ = tx.Rollback()
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
	err := s.readerDB().QueryRowContext(ctx, `SELECT blocked_until FROM login_attempts WHERE key_hash=?`, security.Digest(key)).Scan(&blocked)
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
	_, _ = s.DB.ExecContext(ctx, `DELETE FROM login_attempts WHERE key_hash=?`, security.Digest(key))
}
