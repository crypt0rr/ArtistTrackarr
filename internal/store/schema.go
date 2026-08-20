package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
)

func (c ResolutionCandidate) Artist() Artist {
	return Artist{
		MBID: c.MBID, Name: c.Name, SortName: c.SortName, Type: c.Type,
		Country: c.Country, Disambiguation: c.Disambiguation,
		Genres: append([]string(nil), c.Genres...),
	}
}
func Open(path string) (*Store, error) {
	dsn := sqliteDSN(path, false)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{DB: db, dataDir: filepath.Dir(path)}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Open the reader pool only after migrations have completed. The read-only
	// URI prevents accidental writes from production query paths and keeps the
	// writer connection available for migrations and transactions.
	reader, err := sql.Open("sqlite", sqliteDSN(path, true))
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	reader.SetMaxOpenConns(4)
	reader.SetMaxIdleConns(4)
	if err := reader.Ping(); err != nil {
		_ = reader.Close()
		_ = db.Close()
		return nil, err
	}
	s.Reader = reader
	return s, nil
}

// sqliteDSN builds a file URI without allowing filesystem characters such as
// '?' and '#' to become URI query/fragment delimiters. Keep path separators
// unescaped so SQLite preserves absolute and relative path semantics while
// escaping every other path component character.
func sqliteDSN(path string, readOnly bool) string {
	escapedPath := strings.ReplaceAll(url.PathEscape(filepath.ToSlash(path)), "%2F", "/")
	query := url.Values{}
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	if readOnly {
		query.Set("mode", "ro")
	}
	return "file:" + escapedPath + "?" + query.Encode()
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
		if version == 8 {
			if err := s.migrateITunesFallback(ctx); err != nil {
				return fmt.Errorf("migration %d: %w", version, err)
			}
			continue
		}
		if version == 11 {
			if err := s.migrateUsernames(ctx, body); err != nil {
				return fmt.Errorf("migration %d: %w", version, err)
			}
			continue
		}
		if version == 12 {
			if err := s.migrateOperationalTimestamps(ctx); err != nil {
				return fmt.Errorf("migration %d: %w", version, err)
			}
			continue
		}
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, string(body)); err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?,?)`, version, nowText())
		}
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if err := s.verifyForeignKeyEnforcement(ctx); err != nil {
		return fmt.Errorf("verify foreign-key enforcement after migrations: %w", err)
	}
	return nil
}

// verifyForeignKeyEnforcement is deliberately run after the complete
// migration sequence, not only after the one migration that temporarily
// disables SQLite foreign keys. A future migration may use the same rebuild
// pattern, and startup must fail closed rather than keep serving with cascades
// and ownership constraints disabled.
func (s *Store) verifyForeignKeyEnforcement(ctx context.Context) error {
	if s == nil || s.DB == nil {
		return errors.New("database handle is unavailable")
	}
	var enabled int
	if err := s.DB.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&enabled); err != nil {
		return err
	}
	if enabled != 1 {
		return fmt.Errorf("PRAGMA foreign_keys=%d; want 1", enabled)
	}
	return nil
}

// migrateOperationalTimestamps rewrites every persisted timestamp into the
// canonical UTC representation used by current writes. Older releases stored
// application-log offsets and one migration used SQLite's space-separated
// datetime format; normalizing both makes the existing text indexes usable.
// The whole conversion is one transaction so malformed data never produces a
// partially migrated database.
func (s *Store) migrateOperationalTimestamps(ctx context.Context) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := func(e error) error { _ = tx.Rollback(); return e }
	columns := []struct {
		table  string
		column string
	}{
		{"schema_migrations", "applied_at"},
		{"users", "created_at"},
		{"sessions", "expires_at"}, {"sessions", "created_at"},
		{"auth_tokens", "expires_at"}, {"auth_tokens", "used_at"}, {"auth_tokens", "created_at"},
		{"login_attempts", "first_at"}, {"login_attempts", "blocked_until"},
		{"artists", "last_checked_at"}, {"artists", "next_check_at"}, {"artists", "spotify_next_check_at"},
		{"artists", "spotify_last_change_at"}, {"artists", "created_at"}, {"artists", "updated_at"},
		{"follows", "created_at"}, {"follows", "baseline_synced_at"}, {"follows", "spotify_baseline_synced_at"},
		{"release_groups", "first_observed_at"}, {"release_groups", "updated_at"},
		{"release_groups", "itunes_artwork_checked_at"}, {"release_groups", "itunes_artwork_next_check_at"},
		{"provider_observations", "observed_at"},
		{"import_jobs", "created_at"}, {"destinations", "created_at"},
		{"notification_events", "created_at"}, {"deliveries", "next_attempt_at"}, {"deliveries", "sent_at"},
		{"application_logs", "created_at"},
		{"manual_sync_requests", "created_at"}, {"manual_sync_requests", "started_at"}, {"manual_sync_requests", "finished_at"},
		{"provider_health", "last_success_at"}, {"provider_health", "last_failure_at"},
		{"provider_health", "next_check_at"}, {"provider_health", "updated_at"},
		{"artist_resolutions", "next_attempt_at"}, {"artist_resolutions", "created_at"}, {"artist_resolutions", "updated_at"},
		{"notification_preferences", "updated_at"},
		{"artist_listenbrainz_stats", "checked_at"}, {"artist_listenbrainz_stats", "next_check_at"}, {"artist_listenbrainz_stats", "updated_at"},
		{"artist_genres", "updated_at"},
	}
	for _, item := range columns {
		query := fmt.Sprintf("SELECT rowid,%s FROM %s WHERE %s IS NOT NULL AND %s<>''", item.column, item.table, item.column, item.column)
		type update struct {
			id   int64
			text string
		}
		var updates []update
		if err := func() error {
			rows, queryErr := tx.QueryContext(ctx, query)
			if queryErr != nil {
				return fmt.Errorf("read %s.%s: %w", item.table, item.column, queryErr)
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var id int64
				var raw string
				if scanErr := rows.Scan(&id, &raw); scanErr != nil {
					return fmt.Errorf("scan %s.%s: %w", item.table, item.column, scanErr)
				}
				parsed, parseErr := parseTime(raw)
				if parseErr != nil {
					return fmt.Errorf("invalid timestamp in %s.%s row %d: %w", item.table, item.column, id, parseErr)
				}
				canonical := timeText(parsed)
				if raw != canonical {
					updates = append(updates, update{id: id, text: canonical})
				}
			}
			if rowsErr := rows.Err(); rowsErr != nil {
				return fmt.Errorf("read %s.%s: %w", item.table, item.column, rowsErr)
			}
			return nil
		}(); err != nil {
			return rollback(err)
		}
		for _, change := range updates {
			statement := fmt.Sprintf("UPDATE %s SET %s=? WHERE rowid=?", item.table, item.column)
			if _, updateErr := tx.ExecContext(ctx, statement, change.text, change.id); updateErr != nil {
				return rollback(fmt.Errorf("update %s.%s row %d: %w", item.table, item.column, change.id, updateErr))
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(12,?)`, nowText()); err != nil {
		return rollback(err)
	}
	return tx.Commit()
}

// migrateUsernames adds the required username column and deterministically
// fills legacy rows before creating the case-insensitive uniqueness index.
// It is kept as a Go migration because SQLite does not provide a portable way
// to sanitize arbitrary email local-parts in a single ALTER TABLE statement.
func (s *Store) migrateUsernames(ctx context.Context, body []byte) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := func(e error) error { _ = tx.Rollback(); return e }
	if _, err = tx.ExecContext(ctx, string(body)); err != nil {
		return rollback(err)
	}
	type legacyUser struct {
		id    int64
		email string
	}
	var users []legacyUser
	if err := func() error {
		rows, err := tx.QueryContext(ctx, `SELECT id,email FROM users ORDER BY id`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var user legacyUser
			if err := rows.Scan(&user.id, &user.email); err != nil {
				return err
			}
			users = append(users, user)
		}
		return rows.Err()
	}(); err != nil {
		return rollback(err)
	}
	taken := make(map[string]struct{}, len(users))
	for _, user := range users {
		name := derivedUsername(user.email, user.id, taken)
		if _, err := tx.ExecContext(ctx, `UPDATE users SET username=? WHERE id=?`, name, user.id); err != nil {
			return rollback(err)
		}
		taken[strings.ToLower(name)] = struct{}{}
	}
	if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS users_username_unique ON users(username COLLATE NOCASE)`); err != nil {
		return rollback(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(11,?)`, nowText()); err != nil {
		return rollback(err)
	}
	return tx.Commit()
}

var migrationFaultHook func(string) error

// migrateITunesFallback rebuilds the two tables whose provider CHECK
// constraints need to accept iTunes. SQLite cannot alter CHECK constraints in
// place, so the tables are copied while foreign-key enforcement is temporarily
// disabled during application startup. The dependent tables keep their
// release_groups foreign-key name and continue to point at the replacement.
func (s *Store) migrateITunesFallback(ctx context.Context) (err error) {
	if _, err = s.DB.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	// foreign_keys cannot be toggled while a transaction is active. Restore
	// it after commit/rollback and verify the connection really enforces it;
	// silently leaving this pragma disabled would undermine every FK cascade.
	defer func() {
		restoreErr := func() error {
			if _, restoreErr := s.DB.ExecContext(ctx, `PRAGMA foreign_keys=ON`); restoreErr != nil {
				return fmt.Errorf("restore foreign-key enforcement: %w", restoreErr)
			}
			var enabled int
			if queryErr := s.DB.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&enabled); queryErr != nil {
				return fmt.Errorf("verify foreign-key enforcement: %w", queryErr)
			}
			if enabled != 1 {
				return errors.New("verify foreign-key enforcement: pragma remains disabled")
			}
			return nil
		}()
		if restoreErr == nil {
			return
		}
		if err == nil {
			err = restoreErr
			return
		}
		err = fmt.Errorf("%w (also: %w)", err, restoreErr)
	}()

	var tx *sql.Tx
	tx, err = s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err = s.migrateITunesFallbackTx(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	err = tx.Commit()
	return err
}

func (s *Store) migrateITunesFallbackTx(ctx context.Context, tx *sql.Tx) error {
	rollback := func(err error) error { return err }
	var err error
	if _, err = tx.ExecContext(ctx, `CREATE TABLE release_groups_itunes (
		id INTEGER PRIMARY KEY,
		mbid TEXT NOT NULL UNIQUE,
		artist_id INTEGER NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
		title TEXT NOT NULL,
		primary_type TEXT NOT NULL,
		secondary_types TEXT NOT NULL DEFAULT '[]',
		first_release_date TEXT NOT NULL DEFAULT '',
		date_precision INTEGER NOT NULL DEFAULT 0,
		musicbrainz_url TEXT NOT NULL,
		spotify_url TEXT,
		first_observed_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		spotify_id TEXT,
		spotify_image_url TEXT NOT NULL DEFAULT '',
		itunes_id TEXT,
		itunes_url TEXT,
		source TEXT NOT NULL DEFAULT 'musicbrainz' CHECK(source IN ('musicbrainz','spotify','itunes','both'))
	)`); err != nil {
		return rollback(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO release_groups_itunes
		(id,mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,spotify_url,first_observed_at,updated_at,spotify_id,spotify_image_url,source)
		SELECT id,mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,spotify_url,first_observed_at,updated_at,spotify_id,spotify_image_url,source
		FROM release_groups`); err != nil {
		return rollback(err)
	}
	if _, err = tx.ExecContext(ctx, `DROP TABLE release_groups`); err != nil {
		return rollback(err)
	}
	if _, err = tx.ExecContext(ctx, `ALTER TABLE release_groups_itunes RENAME TO release_groups`); err != nil {
		return rollback(err)
	}
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS releases_artist ON release_groups(artist_id)`,
		`CREATE INDEX IF NOT EXISTS releases_date ON release_groups(first_release_date)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS release_groups_spotify_id ON release_groups(spotify_id) WHERE spotify_id IS NOT NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS release_groups_itunes_id ON release_groups(itunes_id) WHERE itunes_id IS NOT NULL`,
	} {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return rollback(err)
		}
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE artist_resolutions_itunes (
		id INTEGER PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		provider TEXT NOT NULL CHECK(provider IN ('spotify','itunes')),
		provider_id TEXT NOT NULL,
		display_name TEXT NOT NULL,
		provider_url TEXT NOT NULL,
		image_url TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL CHECK(status IN ('pending','review')),
		candidate_json TEXT NOT NULL DEFAULT '[]',
		attempts INTEGER NOT NULL DEFAULT 0,
		next_attempt_at TEXT,
		last_error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		UNIQUE(user_id, provider, provider_id)
	)`); err != nil {
		return rollback(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO artist_resolutions_itunes
		(id,user_id,provider,provider_id,display_name,provider_url,image_url,status,candidate_json,
		 attempts,next_attempt_at,last_error,created_at,updated_at)
		SELECT id,user_id,provider,provider_id,display_name,provider_url,image_url,status,candidate_json,
		 attempts,next_attempt_at,last_error,created_at,updated_at FROM artist_resolutions`); err != nil {
		return rollback(err)
	}
	if _, err = tx.ExecContext(ctx, `DROP TABLE artist_resolutions`); err != nil {
		return rollback(err)
	}
	if _, err = tx.ExecContext(ctx, `ALTER TABLE artist_resolutions_itunes RENAME TO artist_resolutions`); err != nil {
		return rollback(err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE INDEX artist_resolutions_due ON artist_resolutions(status, next_attempt_at)`); err != nil {
		return rollback(err)
	}
	if migrationFaultHook != nil {
		if err := migrationFaultHook("itunes-after-rebuild"); err != nil {
			return rollback(err)
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(8,?)`, nowText())
	return rollback(err)
}
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.readerMu.Lock()
		reader := s.Reader
		s.Reader = nil
		s.readerMu.Unlock()

		var readerErr error
		if reader != nil {
			readerErr = reader.Close()
		}
		var writerErr error
		if s.DB != nil {
			writerErr = s.DB.Close()
		}
		s.closeErr = errors.Join(writerErr, readerErr)
	})
	return s.closeErr
}

// DatabaseHealthState identifies the failure class of a readiness check
// without exposing driver details to unauthenticated callers.
type DatabaseHealthState string

const (
	DatabaseHealthy     DatabaseHealthState = "healthy"
	DatabaseUnavailable DatabaseHealthState = "unavailable"
	DatabaseReadOnly    DatabaseHealthState = "read_only"
	DatabaseFull        DatabaseHealthState = "full"
	DatabaseWriteFailed DatabaseHealthState = "write_failed"
)

// DatabaseHealthError wraps a readiness failure with a safe classification.
// The underlying error is retained for structured server logs and tests, but
// is never written to the unauthenticated response body.
type DatabaseHealthError struct {
	State DatabaseHealthState
	Err   error
}

func (e *DatabaseHealthError) Error() string {
	if e == nil || e.Err == nil {
		return string(DatabaseUnavailable)
	}
	return string(e.State) + ": " + e.Err.Error()
}

func (e *DatabaseHealthError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// classifyDatabaseHealthError turns SQLite's write error into a small safe
// state vocabulary for readiness and authenticated diagnostics. The driver
// error is retained for logs, but the state is all that crosses the web/API
// boundary.
func classifyDatabaseHealthError(err error) DatabaseHealthState {
	if err == nil {
		return DatabaseHealthy
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		code := sqliteErr.Code() & 0xff
		name := strings.ToUpper(sqlite.ErrorCodeString[code])
		switch {
		case strings.Contains(name, "SQLITE_READONLY"):
			return DatabaseReadOnly
		case strings.Contains(name, "SQLITE_FULL"):
			return DatabaseFull
		}
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "readonly") || strings.Contains(message, "read-only") {
		return DatabaseReadOnly
	}
	if strings.Contains(message, "database or disk is full") || strings.Contains(message, "no space left") {
		return DatabaseFull
	}
	return DatabaseWriteFailed
}

// Healthy proves that the migrated schema can be read. It deliberately uses
// the reader pool so readiness probes remain responsive while the serialized
// writer is committing a long transaction.
func (s *Store) Healthy(ctx context.Context) error {
	if s == nil || s.readerDB() == nil {
		return &DatabaseHealthError{State: DatabaseUnavailable, Err: errors.New("database handle is unavailable")}
	}
	var one int
	// Keep readiness bounded while still proving that migrations completed. A
	// bare SELECT 1 would report an arbitrary SQLite file as ready even when the
	// application schema is absent or corrupt.
	if err := s.readerDB().QueryRowContext(ctx, `SELECT 1 FROM schema_migrations LIMIT 1`).Scan(&one); err != nil {
		return &DatabaseHealthError{State: DatabaseUnavailable, Err: err}
	}
	return nil
}

// Ready proves that the application schema is readable and that the writer
// can complete a tiny rollback-only transaction.  Keeping the two checks in
// this bounded helper gives unauthenticated readiness probes the same safety
// classification used by diagnostics without exposing SQLite details.
func (s *Store) Ready(ctx context.Context) error {
	if err := s.Healthy(ctx); err != nil {
		return err
	}
	return s.Writable(ctx)
}

// Writable proves that the writer can complete a tiny rollback-only write.
// This check is kept separate from Healthy so read-only readiness probes do
// not queue behind application transactions. Diagnostics uses both checks to
// distinguish an unavailable schema from a database that cannot accept work.
func (s *Store) Writable(ctx context.Context) error {
	if s == nil {
		return &DatabaseHealthError{State: DatabaseUnavailable, Err: errors.New("database handle is unavailable")}
	}
	if s.DB == nil {
		return &DatabaseHealthError{State: DatabaseWriteFailed, Err: errors.New("writer handle is unavailable")}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return &DatabaseHealthError{State: classifyDatabaseHealthError(err), Err: err}
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `INSERT INTO application_logs(created_at,level,message,attributes_json) VALUES(?,?,?,?)`, nowText(), "INFO", "health probe", "[]")
	if err != nil {
		return &DatabaseHealthError{State: classifyDatabaseHealthError(err), Err: err}
	}
	probeID, err := result.LastInsertId()
	if err != nil {
		return &DatabaseHealthError{State: classifyDatabaseHealthError(err), Err: err}
	}
	var found int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM application_logs WHERE id=?`, probeID).Scan(&found); err != nil {
		return &DatabaseHealthError{State: DatabaseUnavailable, Err: err}
	}
	if err := tx.Rollback(); err != nil {
		return &DatabaseHealthError{State: classifyDatabaseHealthError(err), Err: err}
	}
	return nil
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
func normalizeGenreKey(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}
func replaceArtistGenresExec(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, artistID int64, genres []string, source string) error {
	if _, err := exec.ExecContext(ctx, `DELETE FROM artist_genres WHERE artist_id=? AND source=?`, artistID, source); err != nil {
		return err
	}
	seen := make(map[string]bool, len(genres))
	for index, raw := range genres {
		label := strings.TrimSpace(raw)
		key := normalizeGenreKey(label)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if _, err := exec.ExecContext(ctx, `INSERT INTO artist_genres(artist_id,genre,genre_key,source,weight,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(artist_id,genre_key) DO UPDATE SET genre=excluded.genre,source=excluded.source,weight=excluded.weight,updated_at=excluded.updated_at`, artistID, label, key, source, len(genres)-index, nowText()); err != nil {
			return err
		}
		if len(seen) >= 10 {
			break
		}
	}
	return nil
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
func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func nowText() string             { return timeText(time.Now().UTC()) }
func timeText(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func parseTime(v string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, v)
	if err == nil {
		return parsed, nil
	}
	return time.ParseInLocation("2006-01-02 15:04:05", v, time.UTC)
}

func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
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
