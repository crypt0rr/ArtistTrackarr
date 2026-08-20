package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"path"
	"strings"
	"sync"
)

//go:embed templates/*.html
var embeddedTemplates embed.FS

//go:embed static/*
var embeddedStatic embed.FS
var templateFiles = embeddedTemplates
var staticFiles, _ = fs.Sub(embeddedStatic, "static")

var (
	staticKeysOnce sync.Once
	staticKeys     map[string]string
)

// staticAssetVersion returns a deterministic cache key for an embedded asset.
// Keeping the semantic version as a prefix preserves the existing URL shape
// while the content digest invalidates immutable browser caches when an asset
// changes without a release-version bump.
func staticAssetVersion(name string) string {
	staticKeysOnce.Do(func() {
		staticKeys = make(map[string]string)
		_ = fs.WalkDir(staticFiles, ".", func(name string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			data, readErr := fs.ReadFile(staticFiles, name)
			if readErr != nil {
				return readErr
			}
			staticKeys[path.Clean(strings.TrimPrefix(name, "./"))] = staticAssetKey(data)
			return nil
		})
	})
	clean := path.Clean(strings.TrimLeft(name, "/"))
	if digest := staticKeys[clean]; digest != "" {
		return digest
	}
	return "missing"
}

func staticAssetKey(data []byte) string {
	sum := sha256.Sum256(data)
	// Twelve hex characters are enough to make accidental collisions
	// vanishingly unlikely while keeping generated URLs compact.
	return hex.EncodeToString(sum[:])[:12]
}
