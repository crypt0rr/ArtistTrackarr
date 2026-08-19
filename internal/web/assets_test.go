package web

import "testing"

func TestStaticAssetKeyChangesWithContent(t *testing.T) {
	first := staticAssetKey([]byte("first asset"))
	second := staticAssetKey([]byte("updated asset"))
	if first == second {
		t.Fatalf("asset keys did not change after content changed: %q", first)
	}
	if len(first) != 12 || len(second) != 12 {
		t.Fatalf("asset key lengths = %d and %d, want 12", len(first), len(second))
	}
}

func TestStaticAssetVersionUsesEmbeddedContent(t *testing.T) {
	key := staticAssetVersion("theme.js")
	if key == "" || key == "missing" {
		t.Fatalf("theme.js cache key = %q, want embedded content digest", key)
	}
}
