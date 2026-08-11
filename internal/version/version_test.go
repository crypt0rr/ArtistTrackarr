package version

import "testing"

func TestCurrentReleaseVersion(t *testing.T) {
	if Current != "0.35.0" {
		t.Fatalf("Current=%q, want 0.35.0", Current)
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
