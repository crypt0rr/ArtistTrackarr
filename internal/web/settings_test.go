package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// TestDestinationTestThrottleCannotBeResetByRotatingIds pins the limiter key.
// It was keyed on "userID|destinationID", but destinations.id is a plain
// INTEGER PRIMARY KEY with no AUTOINCREMENT, so deleting a destination and
// adding another mints a fresh id — and with it a fresh, empty bucket. Neither
// add nor delete is itself throttled, so the 5-per-15-minute cap on outbound
// test sends could be reset indefinitely.
func TestDestinationTestThrottleCannotBeResetByRotatingIds(t *testing.T) {
	database, server, client := authenticatedTestServer(t, nil, nil, nil)
	ctx := context.Background()
	user, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	// Two destinations the member genuinely owns, standing in for the pre- and
	// post-rotation ids.
	for _, name := range []string{"First", "Second"} {
		if err := database.AddDestination(ctx, user.ID, name, "generic", []byte("encrypted")); err != nil {
			t.Fatal(err)
		}
	}
	destinations, err := database.Destinations(ctx, user.ID)
	if err != nil || len(destinations) < 2 {
		t.Fatalf("destinations=%d err=%v", len(destinations), err)
	}

	post := func(id int64) int {
		csrf := getCSRF(t, client, server.URL+"/settings")
		response := postForm(t, client, fmt.Sprintf("%s/destinations/%d/test", server.URL, id), url.Values{"_csrf": {csrf}})
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode
	}

	// Exhaust the budget against the first destination.
	limited := false
	for i := 0; i < 8; i++ {
		if post(destinations[0].ID) == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("the destination test limiter never engaged")
	}
	// A different destination id must NOT hand the same member a fresh budget.
	if code := post(destinations[1].ID); code != http.StatusTooManyRequests {
		t.Fatalf("a second destination id reset the throttle: status=%d, want 429", code)
	}
}

// TestUnknownDestinationDoesNotConsumeTheTestBudget keeps the limiter behind
// the ownership check. An id the member does not own must 404 without spending
// their budget — and must never create a limiter bucket, since bogus ids were
// the lever for evicting other members' buckets from the shared limiter.
func TestUnknownDestinationDoesNotConsumeTheTestBudget(t *testing.T) {
	database, server, client := authenticatedTestServer(t, nil, nil, nil)
	ctx := context.Background()
	user, err := database.UserByEmail(ctx, "member@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.AddDestination(ctx, user.ID, "Real", "generic", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	destinations, err := database.Destinations(ctx, user.ID)
	if err != nil || len(destinations) == 0 {
		t.Fatalf("destinations=%d err=%v", len(destinations), err)
	}

	// Six requests naming a destination that does not exist.
	for i := 0; i < 6; i++ {
		csrf := getCSRF(t, client, server.URL+"/settings")
		response := postForm(t, client, fmt.Sprintf("%s/destinations/%d/test", server.URL, 999000+int64(i)), url.Values{"_csrf": {csrf}})
		code := response.StatusCode
		_ = response.Body.Close()
		if code != http.StatusNotFound {
			t.Fatalf("unknown destination status=%d, want 404", code)
		}
	}
	// The member's own budget must be untouched.
	csrf := getCSRF(t, client, server.URL+"/settings")
	response := postForm(t, client, fmt.Sprintf("%s/destinations/%d/test", server.URL, destinations[0].ID), url.Values{"_csrf": {csrf}})
	code := response.StatusCode
	_ = response.Body.Close()
	if code == http.StatusTooManyRequests {
		t.Fatal("requests for destinations that do not exist consumed the member's test budget")
	}
}
