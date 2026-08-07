package web

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/store"
)

func TestProviderHealthPresentationHelpers(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name string
		p    store.ProviderHealth
		want string
	}{
		{name: "quota", p: store.ProviderHealth{LastFailureAt: &now, QuotaExceeded: true}, want: "quota limited"},
		{name: "rate", p: store.ProviderHealth{LastFailureAt: &now, RateLimited: true}, want: "rate limited"},
		{name: "degraded", p: store.ProviderHealth{LastFailureAt: &now}, want: "degraded"},
		{name: "healthy", p: store.ProviderHealth{LastSuccessAt: &now}, want: "healthy"},
		{name: "failure without success", p: store.ProviderHealth{LastFailureAt: &now, LastSuccessAt: nil}, want: "degraded"},
		{name: "new", p: store.ProviderHealth{}, want: "no success yet"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := providerHealthStatus(tc.p); got != tc.want {
				t.Fatalf("providerHealthStatus()=%q, want %q", got, tc.want)
			}
			class := providerHealthClass(tc.p)
			if tc.want == "healthy" && class != "sent" {
				t.Fatalf("healthy class=%q", class)
			}
			if tc.want != "healthy" && class == "sent" {
				t.Fatalf("non-healthy class=%q", class)
			}
		})
	}
	if got := providerDisplayLabel("spotify"); got != "Spotify" {
		t.Fatalf("Spotify label=%q", got)
	}
	if got := providerDisplayLabel("itunes"); got != "iTunes" {
		t.Fatalf("iTunes label=%q", got)
	}
	if got := providerDisplayLabel("musicbrainz"); got != "MusicBrainz" {
		t.Fatalf("MusicBrainz label=%q", got)
	}
	if got := providerHealthTime(nil); got != "" || providerHealthTimeAttr((*time.Time)(nil)) != "" {
		t.Fatalf("nil provider time should be empty: %q", got)
	}
	if got := providerHealthTime(now); !strings.Contains(got, "2026") && now.Year() == 2026 {
		t.Fatalf("provider time=%q", got)
	}
}

func TestProviderTimeValueAcceptsSupportedShapes(t *testing.T) {
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	if got, ok := providerTimeValue(now); !ok || !got.Equal(now) {
		t.Fatalf("time value=%v ok=%v", got, ok)
	}
	if got, ok := providerTimeValue(&now); !ok || !got.Equal(now) {
		t.Fatalf("pointer time value=%v ok=%v", got, ok)
	}
	var nilTime *time.Time
	if _, ok := providerTimeValue(nilTime); ok {
		t.Fatal("nil time pointer was accepted")
	}
	if _, ok := providerTimeValue("2026-08-07"); ok {
		t.Fatal("unsupported time value was accepted")
	}
}

func TestProviderHealthErrorHidesStaleRetryCountdown(t *testing.T) {
	quota := store.ProviderHealth{
		RateLimited: true,
		LastError:   "Spotify returned 429; retry after 20m",
	}
	if got := providerHealthError(quota); got != "Spotify returned 429" {
		t.Fatalf("quota error=%q", got)
	}
	plain := store.ProviderHealth{LastError: "provider unavailable"}
	if got := providerHealthError(plain); got != "provider unavailable" {
		t.Fatalf("plain error=%q", got)
	}
}

func TestPageFilterAndURLHelpers(t *testing.T) {
	for _, value := range []string{"album", "ep", "single", "read", "dismissed"} {
		if inboxFilter(value, "album", "ep", "single", "read", "dismissed") != value {
			t.Fatalf("inboxFilter(%q) rejected valid value", value)
		}
	}
	if inboxFilter("invalid", "album") != "" || evidenceFilter("INVALID", "open") != "" {
		t.Fatal("invalid filter was accepted")
	}
	if got := inboxPageURL("unread", "spotify", "album", 1); !strings.Contains(got, "source=spotify") || strings.Contains(got, "page=") {
		t.Fatalf("first inbox URL=%q", got)
	}
	if got := inboxPageURL("", "", "", 2); got != "/inbox?page=2" {
		t.Fatalf("paged inbox URL=%q", got)
	}
	if got := evidenceIssuePageURL("confirmed", "read", "date", "high", 2); !strings.Contains(got, "status=confirmed") || !strings.Contains(got, "page=2") {
		t.Fatalf("evidence URL=%q", got)
	}
	if got := coveragePageURL(3); got != "/coverage?page=3" {
		t.Fatalf("coverage URL=%q", got)
	}
	request := httptest.NewRequest("GET", "/artists?q=the+beatles&genre=rock", nil)
	if got := artistsPageURL(request, 2); !strings.Contains(got, "q=the+beatles") || !strings.Contains(got, "page=2") {
		t.Fatalf("artists URL=%q", got)
	}
	for _, value := range []string{"1", "123456", "000"} {
		if !validProviderID(value) {
			t.Fatalf("valid provider ID %q rejected", value)
		}
	}
	for _, value := range []string{"", "abc", "1.2", "-1"} {
		if validProviderID(value) {
			t.Fatalf("invalid provider ID %q accepted", value)
		}
	}
}

func TestSelectedValuesDeduplicatesAndBoundsBatchInputs(t *testing.T) {
	cases := []struct {
		name    string
		values  []string
		want    []string
		wantErr string
	}{
		{name: "deduplicates and trims", values: []string{" one ", "one", "", "two"}, want: []string{"one", "two"}},
		{name: "empty", values: nil, wantErr: "select at least one artist"},
		{name: "too many", values: []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"}, wantErr: "select no more than 10 artists"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/artists/follow/batch", nil)
			r.Form = url.Values{"mbids": tc.values}
			got, err := selectedValues(r, "mbids")
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("selectedValues error=%v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("selectedValues=%#v err=%v, want %#v", got, err, tc.want)
			}
		})
	}
}

func TestPageStoreErrorProvidesGenericMessage(t *testing.T) {
	app := &App{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	request := httptest.NewRequest("GET", "/artists", nil)
	data := PageData{}
	if !app.pageStoreError(request, &data, "Artists", "artist list", errors.New("database is locked")) {
		t.Fatal("pageStoreError did not report failure")
	}
	if data.Error == "" || strings.Contains(data.Error, "database") {
		t.Fatalf("page error exposed internal details: %q", data.Error)
	}
	if app.pageStoreError(request, &data, "Artists", "artist list", nil) {
		t.Fatal("nil page error reported failure")
	}
}
