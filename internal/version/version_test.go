package version

import "testing"

func TestCurrentReleaseVersion(t *testing.T) {
	if Current != "0.28.2" {
		t.Fatalf("Current=%q, want 0.28.2", Current)
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
