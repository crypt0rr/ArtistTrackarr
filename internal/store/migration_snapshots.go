package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

const (
	// DefaultMigrationSnapshotRetention keeps a small rollback window without
	// allowing pre-migration database copies to grow without bound.
	DefaultMigrationSnapshotRetention = 2
	// MinMigrationSnapshotRetention and MaxMigrationSnapshotRetention bound the
	// operator-controlled snapshot history size.
	MinMigrationSnapshotRetention = 1
	MaxMigrationSnapshotRetention = 10
	migrationSnapshotDirectory    = "migration-snapshots"
	migrationSnapshotTimeLayout   = "20060102T150405.000000000Z"
)

var migrationSnapshotFilename = regexp.MustCompile(`^pre-migration-v([0-9]+)-to-v([0-9]+)-([0-9]{8}T[0-9]{6}\.[0-9]{9}Z)-([A-Za-z0-9_-]+)\.db$`)

// MigrationSnapshotInfo is the redacted filesystem metadata shown in admin
// diagnostics. It contains no database contents or absolute filesystem paths.
type MigrationSnapshotInfo struct {
	Name        string
	FromVersion int
	ToVersion   int
	CreatedAt   time.Time
	SizeBytes   int64
}

func (s *Store) createPreMigrationSnapshot(ctx context.Context, fromVersion, toVersion int) error {
	if s == nil || s.DB == nil {
		return fmt.Errorf("database handle is unavailable")
	}
	if s.dataDir == "" {
		return fmt.Errorf("database data directory is unavailable")
	}
	directory := filepath.Join(s.dataDir, migrationSnapshotDirectory)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create private snapshot directory: %w", err)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("inspect snapshot directory: %w", err)
	}
	if !directoryInfo.IsDir() {
		return fmt.Errorf("snapshot path is not a directory")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("restrict snapshot directory permissions: %w", err)
	}
	createdAt := time.Now().UTC()
	staging, err := os.CreateTemp(directory, ".pre-migration-snapshot-*.tmp")
	if err != nil {
		return fmt.Errorf("reserve temporary snapshot file: %w", err)
	}
	stagingPath := staging.Name()
	if err := staging.Close(); err != nil {
		_ = os.Remove(stagingPath)
		return fmt.Errorf("close temporary snapshot file: %w", err)
	}
	if err := os.Remove(stagingPath); err != nil {
		return fmt.Errorf("prepare temporary snapshot target: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(stagingPath)
		}
	}()
	if _, err := s.DB.ExecContext(ctx, `VACUUM INTO ?`, stagingPath); err != nil {
		return fmt.Errorf("copy database with VACUUM INTO: %w", err)
	}
	if err := os.Chmod(stagingPath, 0o600); err != nil {
		return fmt.Errorf("restrict snapshot file permissions: %w", err)
	}
	file, err := os.OpenFile(stagingPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open completed snapshot: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync completed snapshot: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close completed snapshot: %w", err)
	}
	prefix := fmt.Sprintf("pre-migration-v%03d-to-v%03d-%s-", fromVersion, toVersion, createdAt.Format(migrationSnapshotTimeLayout))
	reserved, err := os.CreateTemp(directory, prefix+"*.db")
	if err != nil {
		return fmt.Errorf("reserve completed snapshot name: %w", err)
	}
	finalPath := reserved.Name()
	if err := reserved.Close(); err != nil {
		_ = os.Remove(finalPath)
		return fmt.Errorf("close completed snapshot name: %w", err)
	}
	if err := os.Remove(finalPath); err != nil {
		return fmt.Errorf("prepare completed snapshot name: %w", err)
	}
	if err := os.Rename(stagingPath, finalPath); err != nil {
		return fmt.Errorf("publish completed snapshot: %w", err)
	}
	complete = true
	return nil
}

func (s *Store) migrationSnapshots() ([]MigrationSnapshotInfo, error) {
	if s == nil || s.dataDir == "" {
		return nil, nil
	}
	directory := filepath.Join(s.dataDir, migrationSnapshotDirectory)
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read snapshot directory: %w", err)
	}
	snapshots := make([]MigrationSnapshotInfo, 0, len(entries))
	for _, entry := range entries {
		match := migrationSnapshotFilename.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect snapshot %s: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		fromVersion, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("parse source version for snapshot %s: %w", entry.Name(), err)
		}
		toVersion, err := strconv.Atoi(match[2])
		if err != nil {
			return nil, fmt.Errorf("parse target version for snapshot %s: %w", entry.Name(), err)
		}
		createdAt, err := time.Parse(migrationSnapshotTimeLayout, match[3])
		if err != nil {
			return nil, fmt.Errorf("parse timestamp for snapshot %s: %w", entry.Name(), err)
		}
		snapshots = append(snapshots, MigrationSnapshotInfo{
			Name: entry.Name(), FromVersion: fromVersion, ToVersion: toVersion,
			CreatedAt: createdAt, SizeBytes: info.Size(),
		})
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].CreatedAt.Equal(snapshots[j].CreatedAt) {
			return snapshots[i].Name > snapshots[j].Name
		}
		return snapshots[i].CreatedAt.After(snapshots[j].CreatedAt)
	})
	return snapshots, nil
}

func (s *Store) pruneMigrationSnapshots() error {
	if s == nil || s.dataDir == "" {
		return nil
	}
	retention := s.migrationSnapshotRetention
	if retention == 0 {
		retention = DefaultMigrationSnapshotRetention
	}
	if retention < MinMigrationSnapshotRetention || retention > MaxMigrationSnapshotRetention {
		return fmt.Errorf("migration snapshot retention must be between %d and %d", MinMigrationSnapshotRetention, MaxMigrationSnapshotRetention)
	}
	snapshots, err := s.migrationSnapshots()
	if err != nil {
		return err
	}
	if len(snapshots) <= retention {
		return nil
	}
	directory := filepath.Join(s.dataDir, migrationSnapshotDirectory)
	for _, snapshot := range snapshots[retention:] {
		if err := os.Remove(filepath.Join(directory, snapshot.Name)); err != nil {
			return fmt.Errorf("remove expired snapshot %s: %w", snapshot.Name, err)
		}
	}
	return nil
}
