package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

func (c ResolutionCandidate) Artist() Artist {
	return Artist{
		MBID: c.MBID, Name: c.Name, SortName: c.SortName, Type: c.Type,
		Country: c.Country, Disambiguation: c.Disambiguation,
		Genres: append([]string(nil), c.Genres...),
	}
}
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
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
	reader, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
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
		rows, queryErr := tx.QueryContext(ctx, query)
		if queryErr != nil {
			return rollback(fmt.Errorf("read %s.%s: %w", item.table, item.column, queryErr))
		}
		type update struct {
			id   int64
			text string
		}
		var updates []update
		for rows.Next() {
			var id int64
			var raw string
			if scanErr := rows.Scan(&id, &raw); scanErr != nil {
				_ = rows.Close()
				return rollback(fmt.Errorf("scan %s.%s: %w", item.table, item.column, scanErr))
			}
			parsed, parseErr := parseTime(raw)
			if parseErr != nil {
				_ = rows.Close()
				return rollback(fmt.Errorf("invalid timestamp in %s.%s row %d: %w", item.table, item.column, id, parseErr))
			}
			canonical := timeText(parsed)
			if raw != canonical {
				updates = append(updates, update{id: id, text: canonical})
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			_ = rows.Close()
			return rollback(fmt.Errorf("read %s.%s: %w", item.table, item.column, rowsErr))
		}
		_ = rows.Close()
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
	rows, err := tx.QueryContext(ctx, `SELECT id,email FROM users ORDER BY id`)
	if err != nil {
		return rollback(err)
	}
	type legacyUser struct {
		id    int64
		email string
	}
	var users []legacyUser
	for rows.Next() {
		var user legacyUser
		if err := rows.Scan(&user.id, &user.email); err != nil {
			_ = rows.Close()
			return rollback(err)
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return rollback(err)
	}
	_ = rows.Close()
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
func (s *Store) migrateITunesFallback(ctx context.Context) error {
	if _, err := s.DB.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer func() { _, _ = s.DB.ExecContext(ctx, `PRAGMA foreign_keys=ON`) }()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := s.migrateITunesFallbackTx(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
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
func (s *Store) Healthy(ctx context.Context) error {
	var one int
	return s.readerDB().QueryRowContext(ctx, `SELECT 1`).Scan(&one)
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

func parseNullableTime(v string) *time.Time {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	t, err := parseTime(v)
	if err != nil {
		return nil
	}
	return &t
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
