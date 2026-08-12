package version

import "testing"

func TestCurrentReleaseVersion(t *testing.T) {
	if Current != "dev" {
		t.Fatalf("Current=%q, want the local-build fallback dev", Current)
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
