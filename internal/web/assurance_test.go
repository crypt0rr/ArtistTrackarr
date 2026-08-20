package web

import (
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
	}, jobs.RunnerStatus{Running: true, LastActivityAt: &now}, "UTC")
	for _, want := range []string{"ArtistTrackarr release assurance report", "Timezone: UTC", "Database: healthy", "spotify", "Scheduler: healthy"} {
		if !strings.Contains(report, want) {
			t.Fatalf("diagnostic report missing %q: %q", want, report)
		}
	}
	if strings.Contains(report, "secret-token") || strings.Contains(report, "example.test") {
		t.Fatalf("diagnostic report leaked provider error: %q", report)
	}
}
