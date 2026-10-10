package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestOpenSnapshotsPreviousSchemaBeforeMigrations(t *testing.T) {
	latestVersion := latestMigrationVersion(t)
	previousVersion := latestVersion - 1
	path := createMigrationFixture(t, previousVersion)
	ctx := context.Background()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	snapshots, err := s.migrationSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("snapshots=%d, want one: %#v", len(snapshots), snapshots)
	}
	snapshot := snapshots[0]
	if snapshot.FromVersion != previousVersion || snapshot.ToVersion != latestVersion {
		t.Fatalf("snapshot versions=%d to %d, want %d to %d", snapshot.FromVersion, snapshot.ToVersion, previousVersion, latestVersion)
	}
	if !strings.HasPrefix(snapshot.Name, fmt.Sprintf("pre-migration-v%03d-to-v%03d-", previousVersion, latestVersion)) {
		t.Fatalf("snapshot name=%q does not identify the schema transition", snapshot.Name)
	}
	if snapshot.SizeBytes == 0 {
		t.Fatal("snapshot is empty")
	}
	directoryEntries, err := os.ReadDir(filepath.Join(filepath.Dir(path), migrationSnapshotDirectory))
	if err != nil || len(directoryEntries) != 1 || directoryEntries[0].Name() != snapshot.Name {
		t.Fatalf("snapshot directory entries=%v err=%v, want only the completed snapshot", directoryEntries, err)
	}
	if got := snapshotDirectoryMode(t, path); got != 0o700 {
		t.Fatalf("snapshot directory mode=%#o, want 0700", got)
	}
	if got := snapshotFileMode(t, path, snapshot.Name); got != 0o600 {
		t.Fatalf("snapshot file mode=%#o, want 0600", got)
	}

	snapshotDB, err := sql.Open("sqlite", sqliteDSN(filepath.Join(filepath.Dir(path), migrationSnapshotDirectory, snapshot.Name), true))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshotDB.Close() }()
	var integrity string
	if err := snapshotDB.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("snapshot integrity=%q err=%v, want ok", integrity, err)
	}
	var snapshotVersion int
	if err := snapshotDB.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&snapshotVersion); err != nil || snapshotVersion != previousVersion {
		t.Fatalf("snapshot max schema=%d err=%v, want %d", snapshotVersion, err, previousVersion)
	}
	var currentVersion int
	if err := s.DB.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&currentVersion); err != nil || currentVersion != latestVersion {
		t.Fatalf("current schema=%d err=%v, want %d", currentVersion, err, latestVersion)
	}
	diagnostics, err := s.Diagnostics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics.MigrationSnapshots) != 1 || diagnostics.MigrationSnapshots[0].Name != snapshot.Name {
		t.Fatalf("diagnostics snapshots=%#v, want the created snapshot", diagnostics.MigrationSnapshots)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close() }()
	snapshots, err = current.migrationSnapshots()
	if err != nil || len(snapshots) != 1 || snapshots[0].Name != snapshot.Name {
		t.Fatalf("reopening current database changed snapshots=%#v err=%v", snapshots, err)
	}
}

func TestOpenSnapshotsBeforeSpecialCaseMigration(t *testing.T) {
	path := createMigrationFixture(t, 7)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	snapshots, err := s.migrationSnapshots()
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("snapshots=%#v err=%v, want one before migration 8", snapshots, err)
	}
	snapshotDB, err := sql.Open("sqlite", sqliteDSN(filepath.Join(filepath.Dir(path), migrationSnapshotDirectory, snapshots[0].Name), true))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshotDB.Close() }()
	var snapshotVersion int
	if err := snapshotDB.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&snapshotVersion); err != nil || snapshotVersion != 7 {
		t.Fatalf("snapshot max schema=%d err=%v, want 7 before Go migration 8", snapshotVersion, err)
	}
}

func TestSnapshotFailureLeavesDatabaseAtPreviousSchema(t *testing.T) {
	latestVersion := latestMigrationVersion(t)
	previousVersion := latestVersion - 1
	path := createMigrationFixture(t, previousVersion)
	blocker := filepath.Join(filepath.Dir(path), migrationSnapshotDirectory)
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "pre-migration snapshot") {
		t.Fatalf("Open error=%v, want clear pre-migration snapshot failure", err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, true))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var schemaVersion int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&schemaVersion); err != nil || schemaVersion != previousVersion {
		t.Fatalf("schema after failed snapshot=%d err=%v, want %d", schemaVersion, err, previousVersion)
	}
}

func TestMigrationSnapshotRetentionRunsAfterSuccessfulUpgrade(t *testing.T) {
	latestVersion := latestMigrationVersion(t)
	previousVersion := latestVersion - 1
	path := createMigrationFixture(t, previousVersion)
	db, err := sql.Open("sqlite", sqliteDSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	fixtureStore := &Store{DB: db, dataDir: filepath.Dir(path), migrationSnapshotRetention: DefaultMigrationSnapshotRetention}
	for i := 0; i < 3; i++ {
		if err := fixtureStore.createPreMigrationSnapshot(context.Background(), previousVersion, latestVersion); err != nil {
			_ = db.Close()
			t.Fatalf("create old snapshot %d: %v", i, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := OpenWithOptions(path, OpenOptions{MigrationSnapshotRetention: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	snapshots, err := s.migrationSnapshots()
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("snapshots after successful upgrade=%#v err=%v, want one", snapshots, err)
	}
	if snapshots[0].FromVersion != previousVersion || snapshots[0].ToVersion != latestVersion {
		t.Fatalf("retained snapshot=%#v, want the new %d-to-%d snapshot", snapshots[0], previousVersion, latestVersion)
	}
}

func TestFailedMigrationDoesNotPruneSnapshots(t *testing.T) {
	latestVersion := latestMigrationVersion(t)
	path := createMigrationFixture(t, 7)
	db, err := sql.Open("sqlite", sqliteDSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	fixtureStore := &Store{DB: db, dataDir: filepath.Dir(path), migrationSnapshotRetention: 1}
	for i := 0; i < 3; i++ {
		if err := fixtureStore.createPreMigrationSnapshot(context.Background(), 7, latestVersion); err != nil {
			_ = db.Close()
			t.Fatalf("create old snapshot %d: %v", i, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	previousHook := migrationFaultHook
	migrationFaultHook = func(stage string) error {
		if stage == "itunes-after-rebuild" {
			return os.ErrPermission
		}
		return nil
	}
	defer func() { migrationFaultHook = previousHook }()
	if _, err := OpenWithOptions(path, OpenOptions{MigrationSnapshotRetention: 1}); err == nil {
		t.Fatal("OpenWithOptions succeeded despite the injected migration failure")
	}
	snapshots, err := (&Store{dataDir: filepath.Dir(path)}).migrationSnapshots()
	if err != nil || len(snapshots) != 4 {
		t.Fatalf("snapshots after failed migration=%d err=%v, want three old plus new snapshot", len(snapshots), err)
	}
}

func TestFreshDatabaseDoesNotCreateMigrationSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := s.migrationSnapshots()
	if err != nil || len(snapshots) != 0 {
		t.Fatalf("fresh database snapshots=%#v err=%v, want none", snapshots, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	current, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close() }()
	snapshots, err = current.migrationSnapshots()
	if err != nil || len(snapshots) != 0 {
		t.Fatalf("already-current database snapshots=%#v err=%v, want none", snapshots, err)
	}
}

func TestOpenWithOptionsRejectsInvalidSnapshotRetention(t *testing.T) {
	for _, retention := range []int{-1, MaxMigrationSnapshotRetention + 1} {
		if _, err := OpenWithOptions(filepath.Join(t.TempDir(), "invalid.db"), OpenOptions{MigrationSnapshotRetention: retention}); err == nil {
			t.Errorf("OpenWithOptions accepted retention %d", retention)
		}
	}
}

func createMigrationFixture(t *testing.T, version int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", sqliteDSN(path, false))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	fixture := &Store{DB: db, dataDir: filepath.Dir(path), migrationSnapshotRetention: DefaultMigrationSnapshotRetention}
	if err := applyFixtureMigrationsThrough(context.Background(), fixture, version); err != nil {
		_ = db.Close()
		t.Fatalf("build schema-%d fixture: %v", version, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// applyFixtureMigrationsThrough builds historical databases for upgrade tests
// without adding a migration cutoff to the production startup path.
func applyFixtureMigrationsThrough(ctx context.Context, fixture *Store, maxVersion int) error {
	if _, err := fixture.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations
		(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		version, err := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("invalid migration %s", entry.Name())
		}
		if version > maxVersion {
			break
		}
		body, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		switch version {
		case 8:
			if err := fixture.migrateITunesFallback(ctx); err != nil {
				return fmt.Errorf("migration %d: %w", version, err)
			}
		case 11:
			if err := fixture.migrateUsernames(ctx, body); err != nil {
				return fmt.Errorf("migration %d: %w", version, err)
			}
		case 12:
			if err := fixture.migrateOperationalTimestamps(ctx); err != nil {
				return fmt.Errorf("migration %d: %w", version, err)
			}
		default:
			tx, err := fixture.DB.BeginTx(ctx, nil)
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
	}
	return nil
}

func latestMigrationVersion(t *testing.T) int {
	t.Helper()
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	highest := 0
	for _, entry := range entries {
		version, err := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
		if err != nil {
			t.Fatalf("parse migration name %s: %v", entry.Name(), err)
		}
		if version > highest {
			highest = version
		}
	}
	return highest
}

func snapshotDirectoryMode(t *testing.T, databasePath string) os.FileMode {
	t.Helper()
	info, err := os.Stat(filepath.Join(filepath.Dir(databasePath), migrationSnapshotDirectory))
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func snapshotFileMode(t *testing.T, databasePath, name string) os.FileMode {
	t.Helper()
	info, err := os.Stat(filepath.Join(filepath.Dir(databasePath), migrationSnapshotDirectory, name))
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
