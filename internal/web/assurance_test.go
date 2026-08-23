package web

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/jobs"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

func TestAssurancePresentationHelpers(t *testing.T) {
	for _, test := range []struct {
		status string
		label  string
		class  string
	}{
		{status: "healthy", label: "Healthy", class: "sent"},
		{status: "delayed", label: "Delayed", class: "ambiguous"},
		{status: "degraded", label: "Degraded", class: "failed"},
		{status: "pending", label: "Pending", class: "pending"},
	} {
		if got := assuranceStatusLabel(test.status); got != test.label {
			t.Fatalf("label(%q)=%q, want %q", test.status, got, test.label)
		}
		if got := assuranceStatusClass(test.status); got != test.class {
			t.Fatalf("class(%q)=%q, want %q", test.status, got, test.class)
		}
	}
}

func TestDiagnosticReportExcludesSensitiveProviderDetails(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	report := diagnosticReport(store.DiagnosticsSnapshot{
		CheckedAt: now, DatabaseHealthy: true, SchemaVersion: 24, FollowedArtists: 3,
		Providers: []store.DiagnosticsProvider{{Provider: "spotify", Status: "healthy", NextCheckAt: &now}},
	}, jobs.RunnerStatus{Running: true, LastActivityAt: &now}, "UTC", LogSinkHealth{})
	for _, want := range []string{"ArtistTrackarr release assurance report", "Timezone: UTC", "Database: healthy", "spotify", "Scheduler: healthy"} {
		if !strings.Contains(report, want) {
			t.Fatalf("diagnostic report missing %q: %q", want, report)
		}
	}
	if strings.Contains(report, "secret-token") || strings.Contains(report, "example.test") {
		t.Fatalf("diagnostic report leaked provider error: %q", report)
	}
}

// TestLogLossIsVisibleWhileTheProcessRuns pins the application-log loss
// counters to a runtime surface. Both were previously read only in the
// clean-shutdown path, strictly after an early return that fires whenever the
// drain is unclean - so a SIGKILL, a panic or a stalled drain reported nothing.
// Meanwhile the admin panel reads log history from SQLite and falls back to the
// in-memory ring only when SQLite returns zero rows, so a partial loss looked
// exactly like a quiet period. The queue fills during precisely the incidents
// an operator is reading that history to understand.
func TestLogLossIsVisibleWhileTheProcessRuns(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	snapshot := store.DiagnosticsSnapshot{
		CheckedAt: now, DatabaseHealthy: true, SchemaVersion: 24, RecentLogEntries: 12,
	}
	runner := jobs.RunnerStatus{Running: true, LastActivityAt: &now}

	clean := diagnosticReport(snapshot, runner, "UTC", LogSinkHealth{})
	if strings.Contains(clean, "Application log loss") {
		t.Fatalf("a healthy sink reported loss: %q", clean)
	}
	if strings.Contains(clean, "application log loss") {
		t.Fatalf("a healthy sink contributed a degraded reason: %q", clean)
	}

	lossy := diagnosticReport(snapshot, runner, "UTC", LogSinkHealth{Dropped: 7, Errors: 3})
	if !strings.Contains(lossy, "Application log loss: 7 dropped (queue full), 3 failed to persist") {
		t.Fatalf("the report does not surface log loss: %q", lossy)
	}
	// The count above it must not be left looking authoritative.
	if !strings.Contains(lossy, "the log history above is incomplete") {
		t.Fatalf("the report does not qualify the event count: %q", lossy)
	}
	if !strings.Contains(lossy, "application log loss") {
		t.Fatalf("log loss did not reach the report's operational reasons: %q", lossy)
	}
	// And it has to reach operational status, not just the prose.
	status, reasons := store.OperationalStatus(store.DiagnosticsSnapshot{
		CheckedAt: now, DatabaseHealthy: true, DroppedLogEntries: 7,
	}, "running", now)
	if status != "degraded" {
		t.Fatalf("operational status=%q with dropped log records, want degraded", status)
	}
	if !slices.Contains(reasons, "application log loss") {
		t.Fatalf("operational reasons=%v, want an application log loss reason", reasons)
	}
}
