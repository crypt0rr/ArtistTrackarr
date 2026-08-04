package version

import "testing"

func TestDevelopmentVersionFallback(t *testing.T) {
	if Current != "dev" {
		t.Fatalf("development Current=%q, want dev", Current)
	}
}

func TestUserAgentUsesBuildVersion(t *testing.T) {
	if Current == "" {
		t.Fatal("build version is empty")
	}
	want := "ArtistTrackarr/" + Current
	if UserAgent != want {
		t.Fatalf("UserAgent=%q, want %q", UserAgent, want)
	}
}
