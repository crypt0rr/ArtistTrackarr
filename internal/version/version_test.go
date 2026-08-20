package version

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCurrentReleaseVersion(t *testing.T) {
	if Current != "0.54.15" {
		t.Fatalf("Current=%q, want the source-controlled release version 0.54.15", Current)
	}
}

func TestUserAgentUsesSourceVersion(t *testing.T) {
	if Current == "" {
		t.Fatal("build version is empty")
	}
	want := "ArtistTrackarr/" + Current
	if UserAgent != want {
		t.Fatalf("UserAgent=%q, want %q", UserAgent, want)
	}
}

func TestBuildFilesDoNotOverrideVersion(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate version test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	checks := map[string][]string{
		"Dockerfile": {
			"APP_VERSION",
			"internal/version.Current=",
		},
		filepath.Join(".github", "workflows", "docker.yml"): {
			"APP_VERSION",
			"dev-${GITHUB_SHA",
			"internal/version.Current",
		},
	}
	for relativePath, forbidden := range checks {
		content, err := os.ReadFile(filepath.Join(root, relativePath))
		if err != nil {
			t.Fatalf("read %s: %v", relativePath, err)
		}
		for _, value := range forbidden {
			if strings.Contains(string(content), value) {
				t.Errorf("%s still contains build-time version override %q", relativePath, value)
			}
		}
	}
}
