package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFollowNotificationRulesDefaultAndOwnerScope(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, err := s.CreateUser(ctx, "rules-owner@example.com", "hash", "member", "UTC", "rules-owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateUser(ctx, "rules-other@example.com", "hash", "member", "UTC", "rules-other")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "rules-artist", Name: "Rules Artist"})
	if err != nil {
		t.Fatal(err)
	}
	added, err := s.Follow(ctx, owner, artist.ID)
	if err != nil || !added {
		t.Fatalf("follow added=%v err=%v", added, err)
	}
	rule, err := s.FollowNotificationRule(ctx, owner, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rule.DeliveryMode != FollowDeliveryInherit || !rule.IncludePrimary || !rule.IncludeFeatured || !rule.Albums || !rule.Compilations {
		t.Fatalf("default rule=%#v", rule)
	}
	custom := rule
	custom.DeliveryMode = FollowDeliveryDigest
	custom.IncludeFeatured = false
	custom.Singles = false
	if err := s.UpdateFollowNotificationRule(ctx, owner, artist.ID, custom); err != nil {
		t.Fatal(err)
	}
	stored, err := s.FollowNotificationRule(ctx, owner, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DeliveryMode != FollowDeliveryDigest || stored.IncludeFeatured || stored.Singles {
		t.Fatalf("stored rule=%#v", stored)
	}
	if err := s.UpdateFollowNotificationRule(ctx, other, artist.ID, custom); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-user update error=%v, want sql.ErrNoRows", err)
	}
	if _, err := s.SetFollowNotificationDeliveryMode(ctx, owner, []int64{artist.ID, artist.ID}, FollowDeliveryImmediate); err != nil {
		t.Fatal(err)
	}
	stored, err = s.FollowNotificationRule(ctx, owner, artist.ID)
	if err != nil || stored.DeliveryMode != FollowDeliveryImmediate || stored.IncludeFeatured || stored.Singles {
		t.Fatalf("bulk mode changed unrelated fields: rule=%#v err=%v", stored, err)
	}
	until := time.Now().UTC().Add(time.Hour)
	if err := s.PauseFollowNotificationRule(ctx, owner, artist.ID, &until); err != nil {
		t.Fatal(err)
	}
	paused, err := s.FollowNotificationRule(ctx, owner, artist.ID)
	if err != nil || paused.PausedUntil == nil {
		t.Fatalf("paused rule=%#v err=%v", paused, err)
	}
	if paused.queuesImmediate(time.Now().UTC()) || paused.belongsInDigest(time.Now().UTC()) {
		t.Fatal("future pause still allowed delivery")
	}
	if err := s.PauseFollowNotificationRule(ctx, owner, artist.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Unfollow(ctx, owner, artist.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FollowNotificationRule(ctx, owner, artist.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("follow notification rule survived unfollow: %v", err)
	}
	if err := s.Unfollow(ctx, owner, artist.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second unfollow error=%v, want sql.ErrNoRows", err)
	}
}

func TestFollowNotificationRulesFilterEventsAndQueueModes(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "rules-events@example.com", "hash", "member", "UTC", "rules-events")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "rules-events-artist", Name: "Rules Events"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	baselineAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	baseline := Release{SpotifyID: "rules-old", SpotifyURL: "https://open.spotify.com/album/rules-old", Title: "Old Release", PrimaryType: "Album", FirstReleaseDate: "2020-01-01", DatePrecision: 3}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{baseline}}}, baselineAt); err != nil {
		t.Fatal(err)
	}
	// Keep the onboarding event out of the delivery assertion below; the
	// follow rule applies to releases observed after the baseline.
	if err := s.AddDestination(ctx, userID, "Rules destination", "generic", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateFollowNotificationRule(ctx, userID, artist.ID, FollowNotificationRule{
		DeliveryMode: FollowDeliveryDigest, IncludePrimary: true, IncludeFeatured: false,
		Albums: true, EPs: true, Singles: true, Compilations: true, Announcements: true, ReleaseDay: true,
	}); err != nil {
		t.Fatal(err)
	}
	future := Release{SpotifyID: "rules-future", SpotifyURL: "https://open.spotify.com/album/rules-future", Title: "Future Release", PrimaryType: "Album", FirstReleaseDate: "2026-09-01", DatePrecision: 3}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{baseline, future}}}, baselineAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 2)
	var deliveries int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE event_id IN (SELECT id FROM notification_events WHERE user_id=?)`, userID).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("digest-only rule queued %d immediate deliveries", deliveries)
	}
	// A featured release is suppressed while the credit filter is disabled.
	featured := Release{SpotifyID: "rules-featured", SpotifyURL: "https://open.spotify.com/album/rules-featured", Title: "Featured Release", PrimaryType: "Single", FirstReleaseDate: "2026-09-02", DatePrecision: 3, ArtistCreditRole: "featured"}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{baseline, future, featured}}}, baselineAt.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 2)
	// Turning the credit filter on does not change delivery mode, so the next
	// featured event remains visible in history but still belongs in a digest.
	rule, err := s.FollowNotificationRule(ctx, userID, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	rule.IncludeFeatured = true
	if err := s.UpdateFollowNotificationRule(ctx, userID, artist.ID, rule); err != nil {
		t.Fatal(err)
	}
	featuredNext := Release{SpotifyID: "rules-featured-next", SpotifyURL: "https://open.spotify.com/album/rules-featured-next", Title: "Featured Next", PrimaryType: "Single", FirstReleaseDate: "2026-09-03", DatePrecision: 3, ArtistCreditRole: "featured"}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{baseline, future, featured, featuredNext}}}, baselineAt.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 3)
	if err := s.UpdateFollowNotificationRule(ctx, userID, artist.ID, FollowNotificationRule{
		DeliveryMode: FollowDeliveryOff, IncludePrimary: true, IncludeFeatured: true,
		Albums: true, EPs: true, Singles: true, Compilations: true, Announcements: true, ReleaseDay: true,
	}); err != nil {
		t.Fatal(err)
	}
	offRelease := Release{SpotifyID: "rules-off", SpotifyURL: "https://open.spotify.com/album/rules-off", Title: "Off Release", PrimaryType: "Album", FirstReleaseDate: "2026-09-04", DatePrecision: 3}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{baseline, future, featured, featuredNext, offRelease}}}, baselineAt.Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 4)
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE event_id IN (SELECT id FROM notification_events WHERE user_id=?)`, userID).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 0 {
		t.Fatalf("off rule queued deliveries after %d events", deliveries)
	}
	// Content and event-moment filters are independent from delivery mode.
	if err := s.UpdateFollowNotificationRule(ctx, userID, artist.ID, FollowNotificationRule{
		DeliveryMode: FollowDeliveryImmediate, IncludePrimary: true, IncludeFeatured: true,
		Albums: true, EPs: true, Singles: false, Compilations: true, Announcements: true, ReleaseDay: false,
	}); err != nil {
		t.Fatal(err)
	}
	single := Release{SpotifyID: "rules-single-filtered", SpotifyURL: "https://open.spotify.com/album/rules-single-filtered", Title: "Filtered Single", PrimaryType: "Single", FirstReleaseDate: "2026-09-05", DatePrecision: 3}
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{baseline, future, featured, featuredNext, offRelease, single}}}, baselineAt.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 4)
}

func TestHeldNotificationsReevaluateCurrentRulesOnApproval(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name          string
		configureRule func(t *testing.T, s *Store, userID, artistID int64)
		wantEvent     bool
		wantDelivery  bool
	}{
		{
			name: "digest-only",
			configureRule: func(t *testing.T, s *Store, userID, artistID int64) {
				rule, err := s.FollowNotificationRule(ctx, userID, artistID)
				if err != nil {
					t.Fatal(err)
				}
				rule.DeliveryMode = FollowDeliveryDigest
				if err := s.UpdateFollowNotificationRule(ctx, userID, artistID, rule); err != nil {
					t.Fatal(err)
				}
			},
			wantEvent: true,
		},
		{
			name: "content-disabled",
			configureRule: func(t *testing.T, s *Store, userID, artistID int64) {
				rule, err := s.FollowNotificationRule(ctx, userID, artistID)
				if err != nil {
					t.Fatal(err)
				}
				rule.Announcements = false
				if err := s.UpdateFollowNotificationRule(ctx, userID, artistID, rule); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "paused",
			configureRule: func(t *testing.T, s *Store, userID, artistID int64) {
				until := time.Now().UTC().Add(time.Hour)
				if err := s.PauseFollowNotificationRule(ctx, userID, artistID, &until); err != nil {
					t.Fatal(err)
				}
			},
			wantEvent: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			userID, err := s.CreateUser(ctx, "held-rules-"+tc.name+"@example.com", "hash", "member", "UTC", "held-rules-"+tc.name)
			if err != nil {
				t.Fatal(err)
			}
			artist, err := s.UpsertArtist(ctx, Artist{MBID: "held-rules-" + tc.name, Name: "Held Rules " + tc.name})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
				t.Fatal(err)
			}
			preferences, err := s.NotificationPreferences(ctx, userID)
			if err != nil {
				t.Fatal(err)
			}
			preferences.HoldConflictingNotifications = true
			if err := s.UpdateNotificationPreferences(ctx, preferences); err != nil {
				t.Fatal(err)
			}
			result, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
				(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
				 musicbrainz_url,spotify_id,spotify_url,source,first_observed_at,updated_at)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, "held-rules-release-"+tc.name, artist.ID, "Held Rules Release", "Album", "[]",
				"2099-01-01", 3, "", "held-rules-spotify-"+tc.name,
				"https://open.spotify.com/album/held-rules-spotify-"+tc.name, "spotify", nowText(), nowText())
			if err != nil {
				t.Fatal(err)
			}
			releaseID, err := result.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_evidence_issues
				(release_group_id,issue_type,severity,fingerprint,summary,status,first_seen_at,last_seen_at)
				VALUES(?,?,?,?,?,'open',?,?)`, releaseID, "title_conflict", "warning", "held-rules-fingerprint-"+tc.name,
				"Providers disagree", nowText(), nowText()); err != nil {
				t.Fatal(err)
			}
			if err := s.EnqueueEvent(ctx, userID, releaseID, "announcement", "Held rules release", "Review this release", time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			holds, err := s.NotificationHoldsForRelease(ctx, userID, releaseID)
			if err != nil || len(holds) != 1 {
				t.Fatalf("initial holds=%#v err=%v", holds, err)
			}
			tc.configureRule(t, s, userID, artist.ID)
			if err := s.SetReleaseTruthDecision(ctx, userID, releaseID, "spotify", "reviewed"); err != nil {
				t.Fatal(err)
			}
			var events, deliveries int
			if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM notification_events WHERE user_id=? AND release_group_id=?`, userID, releaseID).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE event_id IN (SELECT id FROM notification_events WHERE user_id=? AND release_group_id=?)`, userID, releaseID).Scan(&deliveries); err != nil {
				t.Fatal(err)
			}
			if (events > 0) != tc.wantEvent {
				t.Fatalf("events=%d, want event=%v", events, tc.wantEvent)
			}
			if (deliveries > 0) != tc.wantDelivery {
				t.Fatalf("deliveries=%d, want delivery=%v", deliveries, tc.wantDelivery)
			}
			allHolds, err := s.NotificationHoldsForReleaseIncludingDiscarded(ctx, userID, releaseID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantEvent {
				if len(allHolds) != 0 {
					t.Fatalf("released hold still visible: %#v", allHolds)
				}
				var body string
				if err := s.DB.QueryRowContext(ctx, `SELECT body FROM notification_events WHERE user_id=? AND release_group_id=?`, userID, releaseID).Scan(&body); err != nil {
					t.Fatal(err)
				}
				if strings.Count(body, "Followed artist association(s):") > 1 {
					t.Fatalf("hold body was decorated repeatedly: %q", body)
				}
			} else if len(allHolds) != 1 || allHolds[0].Status != "held" {
				t.Fatalf("blocked hold projection=%#v", allHolds)
			}
		})
	}
}

func TestAccountNotificationMomentPreferencesGateInheritedRules(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "account-preferences@example.com", "hash", "member", "UTC", "account-preferences")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "account-preferences-artist", Name: "Account Preferences"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNotificationPreferences(ctx, NotificationPreferences{
		UserID: userID, Albums: true, EPs: true, Singles: true,
		Announcements: false, ReleaseDay: false,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	baseline := Release{MBID: "account-old", Title: "Account Old", PrimaryType: "Album", FirstReleaseDate: "2020-01-01", DatePrecision: 3}
	if err := s.ApplyReleaseSync(ctx, artist, []Release{baseline}, now); err != nil {
		t.Fatal(err)
	}
	announcement := baseline
	announcement.MBID = "account-announcement"
	announcement.Title = "Account Announcement"
	announcement.FirstReleaseDate = "2026-08-20"
	if err := s.ApplyReleaseSync(ctx, artist, []Release{baseline, announcement}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 0)

	// An explicit per-follow mode is an override of the account-wide moment
	// preference. Its content filters still apply normally.
	rule, err := s.FollowNotificationRule(ctx, userID, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	rule.DeliveryMode = FollowDeliveryImmediate
	if err := s.UpdateFollowNotificationRule(ctx, userID, artist.ID, rule); err != nil {
		t.Fatal(err)
	}
	override := announcement
	override.MBID = "account-override"
	override.Title = "Account Override"
	override.FirstReleaseDate = "2026-08-21"
	if err := s.ApplyReleaseSync(ctx, artist, []Release{baseline, announcement, override}, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 1)

	// Release-day queueing uses the same admission path, so the inherited
	// account preference also suppresses reminders until the explicit rule is
	// selected.
	today := baseline
	today.MBID = "account-today"
	today.Title = "Account Today"
	today.FirstReleaseDate = now.Format("2006-01-02")
	if err := s.ApplyReleaseSync(ctx, artist, []Release{baseline, announcement, override, today}, now.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateFollowNotificationRule(ctx, userID, artist.ID, FollowNotificationRule{
		DeliveryMode: FollowDeliveryInherit, IncludePrimary: true, IncludeFeatured: true,
		Albums: true, EPs: true, Singles: true, Compilations: true, Announcements: true, ReleaseDay: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDueReleaseDays(ctx, now); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "release_day", 0)
	rule, err = s.FollowNotificationRule(ctx, userID, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	rule.DeliveryMode = FollowDeliveryImmediate
	if err := s.UpdateFollowNotificationRule(ctx, userID, artist.ID, rule); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDueReleaseDays(ctx, now); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "release_day", 1)
}

func TestCompilationFilterUsesSecondaryTypeAndPreservesAlbumFilter(t *testing.T) {
	now := time.Now().UTC()
	rule := defaultFollowNotificationRule(1, 1, now)
	rule.Albums = false
	rule.Compilations = false
	if rule.AllowsRelease("Album", []string{"Compilation"}, "primary", "announcement", now) {
		t.Fatal("compilation was admitted while Compilations is disabled")
	}
	rule.Compilations = true
	if !rule.AllowsRelease("Album", []string{"Compilation"}, "primary", "announcement", now) {
		t.Fatal("compilation was not admitted while Compilations is enabled")
	}
	if rule.AllowsRelease("Album", nil, "primary", "announcement", now) {
		t.Fatal("ordinary album was admitted while Albums is disabled")
	}
	rule.Albums = true
	if !rule.AllowsRelease("Album", []string{"Live"}, "primary", "announcement", now) {
		t.Fatal("live album was not controlled by Albums")
	}
}

func TestCompilationFilterAppliesAcrossReleaseProviders(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "compilation-providers@example.com", "hash", "member", "UTC", "compilation-providers")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "compilation-providers-artist", Name: "Compilation Providers"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	rule, err := s.FollowNotificationRule(ctx, userID, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	rule.Compilations = false
	if err := s.UpdateFollowNotificationRule(ctx, userID, artist.ID, rule); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 12, 12, 0, 0, 0, time.UTC)
	providers := []string{"musicbrainz", "spotify", "itunes"}
	for index, provider := range providers {
		old := compilationProviderRelease(provider, "old", "2020-01-01")
		if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: provider, Releases: []Release{old}}}, now.Add(time.Duration(index)*time.Hour)); err != nil {
			t.Fatal(err)
		}
		newRelease := compilationProviderRelease(provider, "filtered", "2026-09-01")
		if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: provider, Releases: []Release{old, newRelease}}}, now.Add(time.Duration(index+3)*time.Hour)); err != nil {
			t.Fatal(err)
		}
		assertEventCount(t, s, userID, "announcement", index)

		rule, err = s.FollowNotificationRule(ctx, userID, artist.ID)
		if err != nil {
			t.Fatal(err)
		}
		rule.Compilations = true
		if err := s.UpdateFollowNotificationRule(ctx, userID, artist.ID, rule); err != nil {
			t.Fatal(err)
		}
		admitted := compilationProviderRelease(provider, "admitted", "2026-09-02")
		if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: provider, Releases: []Release{old, newRelease, admitted}}}, now.Add(time.Duration(index+6)*time.Hour)); err != nil {
			t.Fatal(err)
		}
		assertEventCount(t, s, userID, "announcement", index+1)

		rule.Compilations = false
		if err := s.UpdateFollowNotificationRule(ctx, userID, artist.ID, rule); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompilationFollowRuleStillAppliesWhenAccountAlbumsDisabled(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, err := s.CreateUser(ctx, "compilation-account@example.com", "hash", "member", "UTC", "compilation-account")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNotificationPreferences(ctx, NotificationPreferences{UserID: userID, Albums: false, EPs: true, Singles: true, Announcements: true, ReleaseDay: true}); err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "compilation-account-artist", Name: "Compilation Account"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Follow(ctx, userID, artist.ID); err != nil {
		t.Fatal(err)
	}
	rule, err := s.FollowNotificationRule(ctx, userID, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	rule.Albums = false
	rule.Compilations = true
	if err := s.UpdateFollowNotificationRule(ctx, userID, artist.ID, rule); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	release := compilationProviderRelease("spotify", "account-albums-disabled", "2026-09-01")
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{release}}}, now); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 1)

	rule.Compilations = false
	if err := s.UpdateFollowNotificationRule(ctx, userID, artist.ID, rule); err != nil {
		t.Fatal(err)
	}
	filtered := compilationProviderRelease("spotify", "account-compilation-disabled", "2026-09-02")
	if err := s.ApplyReleaseBatches(ctx, artist, []ReleaseBatch{{Provider: "spotify", Releases: []Release{release, filtered}}}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, s, userID, "announcement", 1)
}

func compilationProviderRelease(provider, suffix, date string) Release {
	release := Release{
		Title: "Compilation " + provider + " " + suffix, PrimaryType: "Album",
		SecondaryTypes: []string{"Compilation"}, FirstReleaseDate: date, DatePrecision: 3,
		Source: provider,
	}
	switch provider {
	case "musicbrainz":
		release.MBID = "compilation:" + provider + ":" + suffix
	case "spotify":
		release.MBID = "spotify:compilation:" + suffix
		release.SpotifyID = "compilation-spotify-" + suffix
		release.SpotifyURL = "https://open.spotify.com/album/compilation-spotify-" + suffix
	case "itunes":
		release.MBID = "itunes:compilation:" + suffix
		release.ITunesID = "compilation-itunes-" + suffix
		release.ITunesURL = "https://music.apple.com/us/album/compilation-itunes-" + suffix
	}
	return release
}

func TestAllowsReleaseTreatsGuestCreditsAsAppearances(t *testing.T) {
	// release_credits.role is CHECK(role IN ('primary','featured','guest')) and
	// both iTunes and MusicBrainz emit "guest". The README states that follow
	// rules including featured appearances also include guest credits, so
	// "guest" must follow IncludeFeatured. Gating it by IncludePrimary inverted
	// that: unchecking "Featured & guest appearances" still delivered every
	// guest credit, and unchecking Primary silently dropped them all.
	base := FollowNotificationRule{
		Albums: true, EPs: true, Singles: true, Compilations: true,
		Announcements: true, ReleaseDay: true, DeliveryMode: FollowDeliveryImmediate,
	}
	now := time.Now().UTC()
	for _, test := range []struct {
		name            string
		role            string
		includePrimary  bool
		includeFeatured bool
		want            bool
	}{
		{name: "guest with appearances on", role: "guest", includePrimary: false, includeFeatured: true, want: true},
		{name: "guest with appearances off", role: "guest", includePrimary: true, includeFeatured: false, want: false},
		{name: "featured with appearances on", role: "featured", includePrimary: false, includeFeatured: true, want: true},
		{name: "featured with appearances off", role: "featured", includePrimary: true, includeFeatured: false, want: false},
		{name: "primary with primary on", role: "primary", includePrimary: true, includeFeatured: false, want: true},
		{name: "primary with primary off", role: "primary", includePrimary: false, includeFeatured: true, want: false},
		{name: "unknown role follows primary", role: "", includePrimary: true, includeFeatured: false, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			rule := base
			rule.IncludePrimary, rule.IncludeFeatured = test.includePrimary, test.includeFeatured
			if got := rule.AllowsRelease("Album", nil, test.role, "announcement", now); got != test.want {
				t.Fatalf("AllowsRelease(role=%q, primary=%t, featured=%t)=%t, want %t",
					test.role, test.includePrimary, test.includeFeatured, got, test.want)
			}
		})
	}
}
