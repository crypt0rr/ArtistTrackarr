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

func validateTimezone(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("invalid IANA timezone")
	}
	if _, err := time.LoadLocation(value); err != nil {
		return "", errors.New("invalid IANA timezone")
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
	timezone, err := validateTimezone(timezone)
	if err != nil {
		return 0, err
	}
	return withWriteTxResult(s, ctx, func(tx *sql.Tx) (int64, error) {
		candidate := strings.TrimSpace(username)
		if candidate == "" {
			var id int64
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0)+1 FROM users`).Scan(&id); err != nil {
				return 0, err
			}
			taken, err := usernamesTakenTx(ctx, tx)
			if err != nil {
				return 0, err
			}
			candidate = derivedUsername(email, id, taken)
		}
		validatedUsername, err := validateUsername(candidate)
		if err != nil {
			return 0, err
		}
		candidate = validatedUsername
		taken, err := usernameTakenTx(ctx, tx, candidate, 0)
		if err != nil {
			return 0, err
		}
		if taken {
			return 0, ErrUsernameTaken
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO users(email,username,password_hash,role,timezone,created_at)
			VALUES(?,?,?,?,?,?)`, email, candidate, hash, role, timezone, nowText())
		if err != nil {
			return 0, err
		}
		return result.LastInsertId()
	})
}

// CreateInitialAdmin atomically establishes the first account.  The setup
// handler may be reached concurrently by two browser sessions; checking the
// count on a read connection before inserting is not sufficient to protect
// the one-time setup invariant.
func (s *Store) CreateInitialAdmin(ctx context.Context, email, hash, timezone, username string) (int64, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") {
		return 0, errors.New("a valid email address is required")
	}
	timezone, err := validateTimezone(timezone)
	if err != nil {
		return 0, err
	}
	return withWriteTxResult(s, ctx, func(tx *sql.Tx) (int64, error) {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
			return 0, err
		}
		if count != 0 {
			return 0, ErrSetupCompleted
		}
		candidate := strings.TrimSpace(username)
		if candidate == "" {
			candidate = derivedUsername(email, 1, nil)
		}
		candidate, err := validateUsername(candidate)
		if err != nil {
			return 0, err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO users(email,username,password_hash,role,timezone,created_at)
			VALUES(?,?,?,?,?,?)`, email, candidate, hash, "admin", timezone, nowText())
		if err != nil {
			return 0, err
		}
		return result.LastInsertId()
	})
}

func usernameTakenTx(ctx context.Context, tx *sql.Tx, username string, exceptID int64) (bool, error) {
	query := `SELECT EXISTS(SELECT 1 FROM users WHERE username COLLATE NOCASE=?`
	args := []any{username}
	if exceptID > 0 {
		query += ` AND id<>?`
		args = append(args, exceptID)
	}
	query += `)`
	var taken int
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&taken); err != nil {
		return false, err
	}
	return taken != 0, nil
}

func usernamesTakenTx(ctx context.Context, tx *sql.Tx) (map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `SELECT username FROM users WHERE username<>''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	taken := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		taken[strings.ToLower(name)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return taken, nil
}
func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var created string
	err := row.Scan(&u.ID, &u.Email, &u.Username, &u.PasswordHash, &u.Role, &u.Timezone, &u.ReminderTime, &created)
	if err != nil {
		return u, err
	}
	u.CreatedAt, err = parseStoredTime(created, "user created_at")
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
		user.CreatedAt, err = parseStoredTime(created, "admin user created_at")
		if err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}
func (s *Store) DeleteUser(ctx context.Context, actingAdminID, userID int64) error {
	if actingAdminID == userID {
		return ErrCannotDeleteSelf
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
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
		// delivery_attempts references destinations with ON DELETE SET NULL and
		// carries no foreign key to deliveries, so the cascade below would leave
		// this member's audit rows behind. Remove them while the destinations
		// that identify them still exist.
		if _, err := tx.ExecContext(ctx, `DELETE FROM delivery_attempts WHERE destination_id IN (
			SELECT id FROM destinations WHERE user_id=?)`, userID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id=?`, userID)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return sql.ErrNoRows
		}
		return nil
	})
}
func (s *Store) UpdatePassword(ctx context.Context, userID int64, hash string) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, hash, userID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return sql.ErrNoRows
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID); err != nil {
			return err
		}
		// A credential change must also cut off the calendar feed token. It is a
		// year-long bearer credential in a plain URL, usable without a session
		// and without CSRF, and it exposes the member's upcoming releases. A
		// member changing their password after a device is lost reasonably
		// expects that to end access; leaving it live meant the only kill switch
		// was a separate Settings action they were never pointed at.
		if _, err := tx.ExecContext(ctx, `UPDATE calendar_feed_tokens SET revoked_at=?
			WHERE user_id=? AND revoked_at IS NULL`, nowText(), userID); err != nil {
			return err
		}
		return nil
	})
}
func (s *Store) UpdateProfile(ctx context.Context, userID int64, timezone, reminder, username string) error {
	var err error
	if timezone, err = validateTimezone(timezone); err != nil {
		return err
	}
	canonicalReminder, ok := normalizeReminderTime(reminder)
	if !ok {
		return errors.New("reminder time must use HH:MM")
	}
	username, err = validateUsername(username)
	if err != nil {
		return err
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		taken, err := usernameTakenTx(ctx, tx, username, userID)
		if err != nil {
			return err
		}
		if taken {
			return ErrUsernameTaken
		}
		_, err = tx.ExecContext(ctx, `UPDATE users SET username=?, timezone=?, reminder_time=? WHERE id=?`, username, timezone, canonicalReminder, userID)
		return err
	})
}

// CreateUserFromInvite consumes an invitation and creates its account in one
// transaction. Validation and uniqueness failures therefore leave the token
// available for correction and retry.
func (s *Store) CreateUserFromInvite(ctx context.Context, raw, hash, username, timezone string) error {
	var err error
	if timezone, err = validateTimezone(timezone); err != nil {
		return err
	}
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var email string
		if err := tx.QueryRowContext(ctx, `SELECT email FROM auth_tokens WHERE token_hash=? AND kind='invite' AND used_at IS NULL AND expires_at>?`, security.Digest(raw), nowText()).Scan(&email); err != nil {
			return err
		}
		candidate := strings.TrimSpace(username)
		if candidate == "" {
			var nextID int64
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0)+1 FROM users`).Scan(&nextID); err != nil {
				return err
			}
			taken, err := usernamesTakenTx(ctx, tx)
			if err != nil {
				return err
			}
			candidate = derivedUsername(email, nextID, taken)
		}
		candidate, err := validateUsername(candidate)
		if err != nil {
			return err
		}
		taken, err := usernameTakenTx(ctx, tx, candidate, 0)
		if err != nil {
			return err
		}
		if taken {
			return ErrUsernameTaken
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO users(email,username,password_hash,role,timezone,created_at) VALUES(?,?,?,'member',?,?)`, email, candidate, hash, timezone, nowText()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE auth_tokens SET used_at=? WHERE token_hash=? AND kind='invite'`, nowText(), security.Digest(raw))
		return err
	})
}

// CreateSession issues a session token. It no longer mints per-session CSRF
// material: the live CSRF implementation is an independent signed double-submit
// cookie that never consults the session, so the second token was generated on
// every sign-in, stored, and read back on the hot path of every authenticated
// request without a single consumer outside this package.
func (s *Store) CreateSession(ctx context.Context, userID int64, ttl time.Duration) (string, error) {
	raw, err := security.Token(32)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	_, err = s.execWriteContext(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at,created_at)
		VALUES(?,?,?,?)`, security.Digest(raw), userID, timeText(now.Add(ttl)), timeText(now))
	return raw, err
}
func (s *Store) Session(ctx context.Context, raw string) (Session, error) {
	var session Session
	var expires, created string
	err := s.readerDB().QueryRowContext(ctx, `SELECT u.id,u.email,u.username,u.password_hash,u.role,u.timezone,u.reminder_time,u.created_at,
		s.expires_at FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token_hash=? AND s.expires_at>?`, security.Digest(raw), nowText()).Scan(
		&session.User.ID, &session.User.Email, &session.User.Username, &session.User.PasswordHash, &session.User.Role,
		&session.User.Timezone, &session.User.ReminderTime, &created, &expires)
	if err != nil {
		return session, err
	}
	session.User.CreatedAt, err = parseStoredTime(created, "session created_at")
	if err != nil {
		return session, err
	}
	session.ExpiresAt, err = parseStoredTime(expires, "session expires_at")
	if err != nil {
		return session, err
	}
	return session, nil
}
func (s *Store) DeleteSession(ctx context.Context, raw string) error {
	_, err := s.execWriteContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, security.Digest(raw))
	return err
}
func (s *Store) CreateAuthToken(ctx context.Context, kind, email string, userID *int64, creator int64, ttl time.Duration) (string, error) {
	raw, err := security.Token(32)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	_, err = s.execWriteContext(ctx, `INSERT INTO auth_tokens(token_hash,kind,email,user_id,expires_at,created_by,created_at)
		VALUES(?,?,?,?,?,?,?)`, security.Digest(raw), kind, strings.ToLower(strings.TrimSpace(email)), userID,
		timeText(now.Add(ttl)), creator, timeText(now))
	return raw, err
}

// ResetPasswordWithToken updates the password, revokes existing sessions, and
// consumes a reset token in one transaction. A transient database failure no
// longer burns a still-valid recovery link.
func (s *Store) ResetPasswordWithToken(ctx context.Context, raw, hash string) error {
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var userID sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT user_id FROM auth_tokens
			WHERE token_hash=? AND kind='reset' AND used_at IS NULL AND expires_at>?`,
			security.Digest(raw), nowText()).Scan(&userID); err != nil {
			return err
		}
		if !userID.Valid {
			return errors.New("reset token has no user")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash=? WHERE id=?`, hash, userID.Int64); err != nil {
			return err
		}
		// Same reasoning as UpdatePassword: a reset is a credential change, so
		// the year-long calendar feed token must not outlive it.
		if _, err := tx.ExecContext(ctx, `UPDATE calendar_feed_tokens SET revoked_at=?
			WHERE user_id=? AND revoked_at IS NULL`, nowText(), userID.Int64); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID.Int64); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE auth_tokens SET used_at=?
			WHERE token_hash=? AND kind='reset' AND used_at IS NULL`, nowText(), security.Digest(raw))
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed != 1 {
			return sql.ErrNoRows
		}
		return nil
	})
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
	t, err := parseStoredTime(blocked.String, "login attempt blocked_until")
	if err != nil {
		return false, err
	}
	return time.Now().UTC().After(t), nil
}
func (s *Store) RecordLoginFailure(ctx context.Context, key string) error {
	now := time.Now().UTC()
	return s.withWriteTx(ctx, func(tx *sql.Tx) error {
		var failures int
		var first string
		err := tx.QueryRowContext(ctx, `SELECT failures,first_at FROM login_attempts WHERE key_hash=?`, security.Digest(key)).Scan(&failures, &first)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, `INSERT INTO login_attempts(key_hash,failures,first_at) VALUES(?,?,?)`,
				security.Digest(key), 1, timeText(now))
			return err
		}
		if err != nil {
			return err
		}
		firstAt, err := parseStoredTime(first, "login attempt first_at")
		if err != nil {
			return err
		}
		if now.Sub(firstAt) > 15*time.Minute {
			failures = 0
			firstAt = now
		}
		failures++
		var blocked any
		if failures >= 5 {
			blocked = timeText(now.Add(15 * time.Minute))
		}
		_, err = tx.ExecContext(ctx, `UPDATE login_attempts SET failures=?,first_at=?,blocked_until=? WHERE key_hash=?`,
			failures, timeText(firstAt), blocked, security.Digest(key))
		return err
	})
}
func (s *Store) ClearLoginFailures(ctx context.Context, key string) {
	_, _ = s.execWriteContext(ctx, `DELETE FROM login_attempts WHERE key_hash=?`, security.Digest(key))
}
