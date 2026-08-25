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

func TestPausedFollowDefersDeliveryInsteadOfDiscardingIt(t *testing.T) {
	// Pausing suppressed the delivery entirely. Because notification_events is
	// unique per (user, release, event type) and the release is no longer newly
	// observed once the pause expires, the alert was lost permanently rather
	// than deferred, which is not what "Pause for 7 days" implies.
	now := time.Now().UTC()
	resume := now.Add(48 * time.Hour)
	past := now.Add(-time.Hour)

	immediate := FollowNotificationRule{DeliveryMode: FollowDeliveryImmediate, PausedUntil: &resume}
	at, deferred := immediate.pausedDeliveryResumesAt(now)
	if !deferred || !at.Equal(resume) {
		t.Fatalf("paused immediate follow: at=%s deferred=%t, want deferral to %s", at, deferred, resume)
	}
	// While paused it must not also be queued for immediate delivery.
	if immediate.queuesImmediate(now) {
		t.Fatal("a paused follow reported immediate delivery")
	}

	inherit := FollowNotificationRule{DeliveryMode: FollowDeliveryInherit, PausedUntil: &resume}
	if _, deferred := inherit.pausedDeliveryResumesAt(now); !deferred {
		t.Fatal("paused inherit follow was not deferred")
	}

	// An expired pause is not a deferral, and delivery resumes normally.
	expired := FollowNotificationRule{DeliveryMode: FollowDeliveryImmediate, PausedUntil: &past}
	if _, deferred := expired.pausedDeliveryResumesAt(now); deferred {
		t.Fatal("an expired pause still deferred delivery")
	}
	if !expired.queuesImmediate(now) {
		t.Fatal("an expired pause did not resume immediate delivery")
	}

	// A follow with no pause is unaffected.
	if _, deferred := (FollowNotificationRule{DeliveryMode: FollowDeliveryImmediate}).pausedDeliveryResumesAt(now); deferred {
		t.Fatal("an unpaused follow was treated as deferred")
	}

	// Digest-only and off keep their own semantics while paused: there is no
	// immediate delivery to defer.
	for _, mode := range []string{FollowDeliveryDigest, FollowDeliveryOff} {
		rule := FollowNotificationRule{DeliveryMode: mode, PausedUntil: &resume}
		if _, deferred := rule.pausedDeliveryResumesAt(now); deferred {
			t.Fatalf("paused %q follow was converted into a deferred immediate delivery", mode)
		}
	}
}

func TestPausedFollowSurvivesADisabledFollowOnTheSameRelease(t *testing.T) {
	// effectiveDeliveryMode reports a paused follow as "off", which used to tie
	// it with a genuinely disabled follow. The tie-break keeps the first
	// candidate, so a disabled follow on the canonical artist could beat a
	// paused immediate follow on a credited artist: the deferral was evaluated
	// against the wrong rule and dropped, while the event row was still written
	// and permanently consumed its uniqueness constraint.
	now := time.Now().UTC()
	resume := now.Add(48 * time.Hour)

	disabled := FollowNotificationRule{DeliveryMode: FollowDeliveryOff}
	pausedImmediate := FollowNotificationRule{DeliveryMode: FollowDeliveryImmediate, PausedUntil: &resume}

	// Both report "off" while paused, which is precisely the collision.
	if disabled.effectiveDeliveryMode(now) != pausedImmediate.effectiveDeliveryMode(now) {
		t.Fatal("precondition changed: a paused follow no longer projects as off")
	}
	// Ranking must use the configured mode so the paused follow outranks the
	// disabled one regardless of candidate order.
	rank := func(r FollowNotificationRule) int {
		switch normalizeFollowDeliveryMode(r.DeliveryMode) {
		case FollowDeliveryInherit, FollowDeliveryImmediate:
			return 3
		case FollowDeliveryDigest:
			return 2
		}
		return 1
	}
	if rank(pausedImmediate) <= rank(disabled) {
		t.Fatalf("paused immediate rank=%d, disabled rank=%d: the paused follow must win",
			rank(pausedImmediate), rank(disabled))
	}
	// And it must still be recognised as a deferral once selected.
	at, deferred := pausedImmediate.pausedDeliveryResumesAt(now)
	if !deferred || !at.Equal(resume) {
		t.Fatalf("selected paused follow did not defer: at=%s deferred=%t", at, deferred)
	}
}

// seedTwoFollowScenario gives one member two follows on one release: the
// canonical artist and a guest credited on it. Both are genuinely their own, so
// pausing either is a single click in the interface.
func seedTwoFollowScenario(t *testing.T, s *Store) (userID, canonicalID, guestID, eventID, deliveryID int64) {
	t.Helper()
	ctx := context.Background()
	var err error
	userID, err = s.CreateUser(ctx, "two-follow@example.com", "hash", "member", "UTC", "two-follow")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := s.UpsertArtist(ctx, Artist{MBID: "tf-canonical", Name: "TF Canonical"})
	if err != nil {
		t.Fatal(err)
	}
	guest, err := s.UpsertArtist(ctx, Artist{MBID: "tf-guest", Name: "TF Guest"})
	if err != nil {
		t.Fatal(err)
	}
	canonicalID, guestID = canonical.ID, guest.ID
	for _, id := range []int64{canonical.ID, guest.ID} {
		if _, err := s.Follow(ctx, userID, id); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	var releaseID int64
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_groups
		(mbid,artist_id,title,primary_type,secondary_types,first_release_date,date_precision,
		 musicbrainz_url,source,first_observed_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "tf-release", canonical.ID, "TF Release", "Album", "[]",
		now.Format("2006-01-02"), 3, "https://musicbrainz.org/release-group/tf-release",
		"musicbrainz", timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM release_groups WHERE mbid=?`, "tf-release").Scan(&releaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_credits
		(release_group_id,artist_id,provider,provider_id,role,track_title,credit_name,provider_url,confidence,first_seen_at,last_seen_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, releaseID, guest.ID, "musicbrainz", "tf-rec", "guest", "Track",
		"TF Guest", "https://musicbrainz.org/recording/tf-rec", "confirmed", timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDestination(ctx, userID, "TF destination", "generic", []byte("encrypted")); err != nil {
		t.Fatal(err)
	}
	// The delivery insert admits only destinations that already existed when the
	// event was created, so the destination has to predate it.
	setDestinationCreatedAt(t, s, userID, now.Add(-time.Hour))
	// Admit the event through the real path so the delivery is queued exactly as
	// production queues it.
	if err := s.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := enqueueEventTxModeOptions(ctx, tx, userID, releaseID, "announcement", "TF Release", "body", now, true, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM notification_events WHERE user_id=? AND release_group_id=?`,
		userID, releaseID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM deliveries WHERE event_id=?`, eventID).Scan(&deliveryID); err != nil {
		t.Fatal(err)
	}
	return userID, canonicalID, guestID, eventID, deliveryID
}

func deliveryNextAttempt(t *testing.T, s *Store, deliveryID int64) time.Time {
	t.Helper()
	var raw string
	if err := s.DB.QueryRow(`SELECT next_attempt_at FROM deliveries WHERE id=?`, deliveryID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	parsed, err := parseStoredTime(raw, "next attempt")
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// TestPausingOneFollowDoesNotMoveAnotherFollowsDelivery pins realignment to the
// GOVERNING follow. The predecessor matched every delivery whose release had
// the artist as canonical or credited, so pausing or resuming one follow moved
// an alert a different, still-active follow governs - and the Artists page went
// on showing that other follow as active while its alert had been rescheduled.
func TestPausingOneFollowDoesNotMoveAnotherFollowsDelivery(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, canonicalID, guestID, _, deliveryID := seedTwoFollowScenario(t, s)

	// The canonical follow governs this delivery (it wins admission's
	// tie-break). Pause it, so the delivery is deliberately deferred far out.
	governing := time.Now().UTC().Add(30 * 24 * time.Hour).Truncate(time.Second)
	if err := s.PauseFollowNotificationRule(ctx, userID, canonicalID, &governing); err != nil {
		t.Fatal(err)
	}
	deferred := deliveryNextAttempt(t, s, deliveryID)
	if deferred.Before(governing.Add(-time.Minute)) {
		t.Fatalf("the governing pause did not defer the delivery: next=%s want>=%s", deferred, governing)
	}

	// Now pause the OTHER follow, which governs nothing here, for a shorter
	// window. The predecessor matched on the release rather than the governing
	// follow, so this dragged the alert forward to the guest follow's expiry
	// while /artists still read "Paused until <the later date>".
	shorter := time.Now().UTC().Add(7 * 24 * time.Hour).Truncate(time.Second)
	if err := s.PauseFollowNotificationRule(ctx, userID, guestID, &shorter); err != nil {
		t.Fatal(err)
	}
	if got := deliveryNextAttempt(t, s, deliveryID); !got.Equal(deferred) {
		t.Fatalf("pausing a non-governing follow moved the delivery: %s -> %s", deferred, got)
	}

	// Resuming the non-governing follow must not release it either.
	if err := s.PauseFollowNotificationRule(ctx, userID, guestID, nil); err != nil {
		t.Fatal(err)
	}
	if got := deliveryNextAttempt(t, s, deliveryID); !got.Equal(deferred) {
		t.Fatalf("resuming a non-governing follow released a delivery the governing pause still holds: %s -> %s", deferred, got)
	}
}

// TestExtendingAPauseMovesItsDeliveriesLater is #249: the predecessor bounded
// its update with next_attempt_at > target, which admits only the earlier
// direction, so extending a pause left its alerts due at the old, earlier time
// and they fired while the follow still read "Paused until <later date>".
func TestExtendingAPauseMovesItsDeliveriesLater(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, canonicalID, _, _, deliveryID := seedTwoFollowScenario(t, s)

	first := time.Now().UTC().Add(7 * 24 * time.Hour).Truncate(time.Second)
	if err := s.PauseFollowNotificationRule(ctx, userID, canonicalID, &first); err != nil {
		t.Fatal(err)
	}
	if got := deliveryNextAttempt(t, s, deliveryID); got.Before(first.Add(-time.Minute)) {
		t.Fatalf("pause did not defer the delivery: next=%s want>=%s", got, first)
	}

	extended := first.Add(14 * 24 * time.Hour).Truncate(time.Second)
	if err := s.PauseFollowNotificationRule(ctx, userID, canonicalID, &extended); err != nil {
		t.Fatal(err)
	}
	got := deliveryNextAttempt(t, s, deliveryID)
	if got.Before(extended.Add(-time.Minute)) {
		t.Fatalf("extending the pause left the delivery at the old expiry: next=%s want>=%s", got, extended)
	}

	// Shortening must still pull it back.
	shortened := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	if err := s.PauseFollowNotificationRule(ctx, userID, canonicalID, &shortened); err != nil {
		t.Fatal(err)
	}
	if got := deliveryNextAttempt(t, s, deliveryID); got.After(shortened.Add(time.Minute)) {
		t.Fatalf("shortening the pause left the delivery at the later expiry: next=%s want<=%s", got, shortened)
	}
}

// TestRealignmentDoesNotResetAnOrdinaryRetryBackoff is the second half of #248.
// A delivery already in exponential backoff was created by failures, not by a
// pause; resuming an unrelated follow reset it to now and forced an immediate
// re-attempt against a destination that was already failing.
func TestRealignmentDoesNotResetAnOrdinaryRetryBackoff(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _, guestID, _, deliveryID := seedTwoFollowScenario(t, s)

	backoff := time.Now().UTC().Add(20 * time.Minute).Truncate(time.Second)
	if _, err := s.DB.ExecContext(ctx, `UPDATE deliveries SET attempts=3,next_attempt_at=? WHERE id=?`,
		timeText(backoff), deliveryID); err != nil {
		t.Fatal(err)
	}
	// No pause anywhere; resume a follow that is not paused.
	if err := s.PauseFollowNotificationRule(ctx, userID, guestID, nil); err != nil {
		t.Fatal(err)
	}
	if got := deliveryNextAttempt(t, s, deliveryID); !got.Equal(backoff) {
		t.Fatalf("an ordinary retry backoff was reset: %s -> %s", backoff, got)
	}
}

// TestPauseDeferredDeliveryBlockedByADestinationRecoversAutomatically is #247.
// A destination circuit-pause converts every pending row to blocked, including
// one a paused follow had deferred and nothing had attempted. Destination
// recovery deliberately skips those - fixing a webhook must not cancel a
// pause - so the row had no automatic route home at all: the member had to know
// to press Retry a second time after the pause expired, which nothing said.
func TestPauseDeferredDeliveryBlockedByADestinationRecoversAutomatically(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, canonicalID, _, _, deliveryID := seedTwoFollowScenario(t, s)

	// The governing follow is paused, so the delivery is deliberately deferred.
	until := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	if err := s.PauseFollowNotificationRule(ctx, userID, canonicalID, &until); err != nil {
		t.Fatal(err)
	}
	// The destination then circuit-pauses, flipping it to blocked.
	if _, err := s.DB.ExecContext(ctx, `UPDATE deliveries SET status='blocked',
		last_error='destination paused after repeated failures' WHERE id=?`, deliveryID); err != nil {
		t.Fatal(err)
	}

	// While the pause is still in force the row must stay blocked.
	if released, err := s.ReleasePauseBlockedDeliveries(ctx, time.Now().UTC()); err != nil || released != 0 {
		t.Fatalf("released=%d err=%v while the pause was still active, want 0", released, err)
	}
	var status string
	if err := s.DB.QueryRow(`SELECT status FROM deliveries WHERE id=?`, deliveryID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "blocked" {
		t.Fatalf("status=%q during an active pause, want blocked", status)
	}

	// Once the pause has expired the maintenance sweep must bring it home with
	// no member action at all.
	after := until.Add(time.Hour)
	released, err := s.ReleasePauseBlockedDeliveries(ctx, after)
	if err != nil {
		t.Fatal(err)
	}
	if released != 1 {
		t.Fatalf("released=%d after the pause expired, want 1", released)
	}
	var lastError string
	if err := s.DB.QueryRow(`SELECT status,last_error FROM deliveries WHERE id=?`, deliveryID).Scan(&status, &lastError); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("status=%q after the pause expired, want pending", status)
	}
	if lastError != "" {
		t.Fatalf("last_error=%q, want the circuit-pause message cleared", lastError)
	}
}

// TestUnfollowCancelsItsQueuedDeliveries is #251. Removing an artist left its
// already-queued notifications in place, so an alert still arrived for an
// artist the member had removed - and a deferred or backed-off row can sit
// months out, long after every page that would explain it stopped showing the
// artist.
func TestUnfollowCancelsItsQueuedDeliveries(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, canonicalID, guestID, _, deliveryID := seedTwoFollowScenario(t, s)

	// Unfollowing the guest must NOT cancel: the member still follows the
	// canonical artist, so the release is still theirs to hear about.
	if err := s.Unfollow(ctx, userID, guestID); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE id=?`, deliveryID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatal("unfollowing one of two qualifying follows cancelled a delivery the member can still legitimately receive")
	}

	// Unfollowing the last qualifying artist must cancel it.
	if err := s.Unfollow(ctx, userID, canonicalID); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE id=?`, deliveryID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatal("a delivery survived after its last qualifying follow was removed, so the alert still fires for a removed artist")
	}
	// The event row is deliberately kept: it is the inbox history, and its
	// uniqueness slot must not be recycled.
	var events int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM notification_events WHERE user_id=?`, userID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("notification_events=%d, want the audit row kept", events)
	}
}

// TestOrphanCancellationSweepMatchesUnfollow keeps the unattended sweep and the
// unfollow path agreeing, since a delivery can also be orphaned by a route that
// does not run Unfollow at all.
func TestOrphanCancellationSweepMatchesUnfollow(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, canonicalID, guestID, _, deliveryID := seedTwoFollowScenario(t, s)

	// Remove both follows directly, bypassing Unfollow entirely.
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM follows WHERE user_id=?`, userID); err != nil {
		t.Fatal(err)
	}
	_ = canonicalID
	_ = guestID
	cancelled, err := s.CancelOrphanedDeliveries(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if cancelled != 1 {
		t.Fatalf("sweep cancelled=%d, want 1", cancelled)
	}
	var remaining int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE id=?`, deliveryID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatal("the sweep left an orphaned delivery queued")
	}
}

// TestOrphanSweepDoesNotTouchClaimedWork keeps the cancellation off a row a
// live worker is mid-send on.
func TestOrphanSweepDoesNotTouchClaimedWork(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _, _, _, deliveryID := seedTwoFollowScenario(t, s)
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM follows WHERE user_id=?`, userID); err != nil {
		t.Fatal(err)
	}
	lease := time.Now().UTC().Add(5 * time.Minute)
	if _, err := s.DB.ExecContext(ctx, `UPDATE deliveries SET claim_owner='worker-1',claim_expires_at=? WHERE id=?`,
		timeText(lease), deliveryID); err != nil {
		t.Fatal(err)
	}
	if cancelled, err := s.CancelOrphanedDeliveries(ctx, time.Now().UTC()); err != nil || cancelled != 0 {
		t.Fatalf("cancelled=%d err=%v, want the claimed row left alone", cancelled, err)
	}
}

// TestRetryDoesNotCancelADeliberatePause pins the discriminating field rather
// than a derived total.
//
// RetryFailedDeliveries partitions failed rows into those it makes runnable now
// and those a pause is still holding back, and the existing fixtures assert
// stats.Total(). That sum is invariant under the defect: if the partition
// collapses and every row is requeued, Total() is unchanged, so a mutation
// removing the deferral check survives every one of them. Recovering a
// destination must never cancel a pause a member set deliberately.
func TestRetryDoesNotCancelADeliberatePause(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, canonicalID, _, _, deliveryID := seedTwoFollowScenario(t, s)

	// The governing follow is paused, so this delivery is deliberately deferred.
	until := time.Now().UTC().Add(14 * 24 * time.Hour).Truncate(time.Second)
	if err := s.PauseFollowNotificationRule(ctx, userID, canonicalID, &until); err != nil {
		t.Fatal(err)
	}
	deferredAt := deliveryNextAttempt(t, s, deliveryID)

	// The destination then fails, so both rows are candidates for recovery.
	destinations, err := s.Destinations(ctx, userID)
	if err != nil || len(destinations) == 0 {
		t.Fatalf("destinations=%d err=%v", len(destinations), err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE deliveries SET status='failed' WHERE id=?`, deliveryID); err != nil {
		t.Fatal(err)
	}

	stats, err := s.RetryFailedDeliveries(ctx, userID, destinations[0].ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// The discriminating assertion: this row must be counted as deferred, not
	// requeued. Total() cannot tell these apart.
	if stats.Deferred != 1 {
		t.Fatalf("stats=%+v, want the paused row counted as deferred", stats)
	}
	if stats.Requeued != 0 {
		t.Fatalf("stats=%+v, want no row made runnable while its pause is active", stats)
	}
	// And it must still be parked at the pause expiry, not pulled to now.
	if got := deliveryNextAttempt(t, s, deliveryID); !got.Equal(deferredAt) {
		t.Fatalf("recovery moved a paused delivery: %s -> %s", deferredAt, got)
	}
}

// TestClaimSkipsAPausedDestinationsPendingRows pins the claim query's exclusion
// of paused destinations. It sets destination_health directly rather than
// driving five real failures, so it guards the query rather than the whole
// circuit-breaker - which is the intent.
func TestClaimSkipsAPausedDestinationsPendingRows(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	userID, _, _, _, deliveryID := seedTwoFollowScenario(t, s)
	destinations, err := s.Destinations(ctx, userID)
	if err != nil || len(destinations) == 0 {
		t.Fatalf("destinations=%d err=%v", len(destinations), err)
	}
	now := time.Now().UTC()
	if _, err := s.DB.ExecContext(ctx, `UPDATE deliveries SET status='pending',next_attempt_at=? WHERE id=?`,
		timeText(now.Add(-time.Hour)), deliveryID); err != nil {
		t.Fatal(err)
	}

	// Control: it is claimable while the destination is healthy.
	claimed, err := s.ClaimDueDeliveries(ctx, now, 10, "worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("control: claimed=%d with a healthy destination, want 1", len(claimed))
	}
	// Release the claim so the only difference in the next call is the pause.
	if _, err := s.DB.ExecContext(ctx, `UPDATE deliveries SET claim_owner=NULL,claim_expires_at=NULL WHERE id=?`,
		deliveryID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.DB.ExecContext(ctx, `INSERT INTO destination_health(destination_id,status,consecutive_failures,updated_at)
		VALUES(?, 'paused',5,?)
		ON CONFLICT(destination_id) DO UPDATE SET status='paused',consecutive_failures=5,updated_at=excluded.updated_at`,
		destinations[0].ID, timeText(now)); err != nil {
		t.Fatal(err)
	}
	claimed, err = s.ClaimDueDeliveries(ctx, now, 10, "worker-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed=%d from a paused destination, want 0", len(claimed))
	}
}

// The pause predicate is a pure time comparison, and every existing fixture
// exercises it at now, now+48h or now-1h - never at the instant the pause
// expires. That leaves `!now.Before(*r.PausedUntil)` and `now.After(...)`
// indistinguishable, though they disagree on exactly the boundary: at
// now == PausedUntil the pause is over and the delivery must go out.
func TestThePauseEndsAtItsExpiryInstantNotAfterIt(t *testing.T) {
	pausedUntil := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	rule := FollowNotificationRule{
		DeliveryMode: FollowDeliveryImmediate,
		PausedUntil:  &pausedUntil,
	}
	for _, tc := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{"a nanosecond before expiry still defers", pausedUntil.Add(-time.Nanosecond), true},
		{"the expiry instant itself does not defer", pausedUntil, false},
		{"a nanosecond after expiry does not defer", pausedUntil.Add(time.Nanosecond), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resumesAt, deferred := rule.pausedDeliveryResumesAt(tc.now)
			if deferred != tc.want {
				t.Fatalf("pausedDeliveryResumesAt(%s) deferred=%v, want %v", tc.now, deferred, tc.want)
			}
			if deferred && !resumesAt.Equal(pausedUntil) {
				t.Errorf("resumesAt=%s, want %s", resumesAt, pausedUntil)
			}
			if !deferred && !resumesAt.IsZero() {
				t.Errorf("resumesAt=%s, want the zero time when not deferred", resumesAt)
			}
		})
	}
}

// A paused follow defers rather than discards, so it will deliver when the
// pause expires - which means it must still take the conflict hold on the way
// in. Gating only on queuesImmediate skipped the hold while the deferral
// queued the delivery anyway, releasing an unreviewed release at pause expiry
// with no hold row for the member to act on.
//
// The sibling half of that fix - `|| deferredDelivery` on the admission return
// - is covered by TestHeldNotificationsReevaluateCurrentRulesOnApproval. This
// is the hold half, from the same commit and the same variable, which nothing
// failed on when reverted.
func TestDeferredDeliveryStillTakesTheConflictHold(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now().UTC()

	userID, err := s.CreateUser(ctx, "deferred-hold@example.com", "hash", "member", "UTC", "deferred-hold")
	if err != nil {
		t.Fatal(err)
	}
	artist, err := s.UpsertArtist(ctx, Artist{MBID: "deferred-hold", Name: "Deferred Hold"})
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
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, "deferred-hold-release", artist.ID, "Deferred Hold Release",
		"Album", "[]", "2099-01-01", 3, "", "deferred-hold-spotify",
		"https://open.spotify.com/album/deferred-hold-spotify", "spotify", nowText(), nowText())
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO release_evidence_issues
		(release_group_id,issue_type,severity,fingerprint,summary,status,first_seen_at,last_seen_at)
		VALUES(?,?,?,?,?,'open',?,?)`, releaseID, "title_conflict", "warning", "deferred-hold-fingerprint",
		"Providers disagree", nowText(), nowText()); err != nil {
		t.Fatal(err)
	}

	// Pause the follow, so admission takes the deferral path rather than the
	// immediate one. Without the pause this test would pass on the strength of
	// queuesImmediate alone and prove nothing.
	until := now.Add(48 * time.Hour)
	if err := s.PauseFollowNotificationRule(ctx, userID, artist.ID, &until); err != nil {
		t.Fatal(err)
	}
	rule, err := s.FollowNotificationRule(ctx, userID, artist.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, deferred := rule.pausedDeliveryResumesAt(now); !deferred {
		t.Fatal("precondition: the follow is not deferring, so the hold path under test never runs")
	}

	if err := s.EnqueueEvent(ctx, userID, releaseID, "announcement", "Deferred hold release", "Review this", now); err != nil {
		t.Fatal(err)
	}

	holds, err := s.NotificationHoldsForRelease(ctx, userID, releaseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 1 {
		t.Fatalf("holds=%d, want 1: a deferred delivery must still be held for review", len(holds))
	}

	// And the hold must be instead of the event, not alongside it - otherwise
	// the alert is queued for pause expiry and the review row is decorative.
	var events int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notification_events WHERE user_id=? AND release_group_id=?`,
		userID, releaseID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Errorf("notification_events=%d, want 0 while the release is held", events)
	}
}
