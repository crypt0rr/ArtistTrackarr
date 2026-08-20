package web

import (
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestCalendarFeedRouteGeneratesRotatesAndRevokesToken(t *testing.T) {
	_, server, client := authenticatedTestServer(t, nil, nil, nil)
	csrf := getCSRF(t, client, server.URL+"/settings")
	response := postForm(t, client, server.URL+"/settings/calendar-feed", url.Values{
		"_csrf": {csrf}, "action": {"generate"},
	})
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "Copy this URL now") {
		t.Fatalf("generate status/body=%d %q", response.StatusCode, body)
	}
	tokenPattern := regexp.MustCompile(`/calendar/feed/([A-Za-z0-9_-]+)`)
	match := tokenPattern.FindStringSubmatch(string(body))
	if len(match) != 2 {
		t.Fatalf("generated feed URL missing from settings: %q", body)
	}
	first := match[1]
	publicClient := &http.Client{}
	feed, err := publicClient.Get(server.URL + "/calendar/feed/" + first)
	if err != nil {
		t.Fatal(err)
	}
	feedBody, _ := io.ReadAll(feed.Body)
	_ = feed.Body.Close()
	if feed.StatusCode != http.StatusOK || !strings.Contains(feed.Header.Get("Content-Type"), "text/calendar") || !strings.Contains(string(feedBody), "BEGIN:VCALENDAR") {
		t.Fatalf("feed status/content=%d %q", feed.StatusCode, feedBody)
	}

	csrf = getCSRF(t, client, server.URL+"/settings")
	response = postForm(t, client, server.URL+"/settings/calendar-feed", url.Values{
		"_csrf": {csrf}, "action": {"rotate"},
	})
	body, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	match = tokenPattern.FindStringSubmatch(string(body))
	if response.StatusCode != http.StatusOK || len(match) != 2 || match[1] == first {
		t.Fatalf("rotate status/body=%d %q", response.StatusCode, body)
	}
	second := match[1]
	oldFeed, err := publicClient.Get(server.URL + "/calendar/feed/" + first)
	if err != nil {
		t.Fatal(err)
	}
	_ = oldFeed.Body.Close()
	if oldFeed.StatusCode != http.StatusNotFound {
		t.Fatalf("rotated old token status=%d, want 404", oldFeed.StatusCode)
	}

	csrf = getCSRF(t, client, server.URL+"/settings")
	response = postForm(t, client, server.URL+"/settings/calendar-feed", url.Values{
		"_csrf": {csrf}, "action": {"revoke"},
	})
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("revoke status=%d", response.StatusCode)
	}
	revoked, err := publicClient.Get(server.URL + "/calendar/feed/" + second)
	if err != nil {
		t.Fatal(err)
	}
	_ = revoked.Body.Close()
	if revoked.StatusCode != http.StatusNotFound {
		t.Fatalf("revoked token status=%d, want 404", revoked.StatusCode)
	}
}
