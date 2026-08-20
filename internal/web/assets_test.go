package web

import (
	"io/fs"
	"strings"
	"testing"
)

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

func TestAppScriptRecoversLiveHealthAndClipboardOnRestore(t *testing.T) {
	script, err := fs.ReadFile(staticFiles, "app.js")
	if err != nil {
		t.Fatal(err)
	}
	content := string(script)
	for _, required := range []string{
		"window.addEventListener(\"pageshow\", startLiveRefresh)",
		"document.addEventListener(\"visibilitychange\"",
		"window.isSecureContext",
		"document.execCommand(\"copy\")",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("app.js missing resilience behavior %q", required)
		}
	}
}
