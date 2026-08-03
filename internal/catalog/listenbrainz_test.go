package catalog

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestListenBrainzPopularityParsesAndCaches(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/1/popularity/artist" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"artist_mbid":"ABC","total_listen_count":1234,"total_user_count":56}]`)
	}))
	defer server.Close()
	provider := NewListenBrainz()
	provider.baseURL = server.URL
	provider.requestEvery = 0
	provider.cacheTTL = time.Hour
	first, err := provider.Popularity(context.Background(), []string{"abc"})
	if err != nil || first["abc"].TotalListenCount != 1234 || first["abc"].TotalUserCount != 56 {
		t.Fatalf("first stats=%#v err=%v", first, err)
	}
	second, err := provider.Popularity(context.Background(), []string{"ABC"})
	if err != nil || second["abc"].TotalListenCount != 1234 || requests.Load() != 1 {
		t.Fatalf("cached stats=%#v requests=%d err=%v", second, requests.Load(), err)
	}
}
