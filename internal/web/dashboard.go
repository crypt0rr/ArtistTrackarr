package web

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	d := a.data(r, "Dashboard")
	pageFailed := false
	var err error
	d.FollowCount, err = a.store.FollowedArtistCount(r.Context(), session.User.ID)
	pageFailed = a.pageStoreError(r, &d, "Dashboard", "followed artist count", err) || pageFailed
	d.InboxUnreadCount, err = a.store.ReleaseInboxUnreadCount(r.Context(), session.User.ID, time.Now().UTC())
	pageFailed = a.pageStoreError(r, &d, "Dashboard", "release inbox count", err) || pageFailed
	d.EvidenceIssueUnreadCount, err = a.store.EvidenceIssueUnreadCount(r.Context(), session.User.ID, time.Now().UTC())
	pageFailed = a.pageStoreError(r, &d, "Dashboard", "release evidence issue count", err) || pageFailed
	d.CoverageSummary, err = a.store.CoverageSummary(r.Context(), session.User.ID)
	pageFailed = a.pageStoreError(r, &d, "Dashboard", "release coverage summary", err) || pageFailed
	location, err := time.LoadLocation(session.User.Timezone)
	if err != nil {
		location = time.UTC
	}
	today := time.Now().In(location).Format("2006-01-02")
	d.UpcomingReleases, d.RecentReleases, err = a.store.DashboardReleases(
		r.Context(), session.User.ID, today, 20,
	)
	pageFailed = a.pageStoreError(r, &d, "Dashboard", "release catalog", err) || pageFailed
	d.ReleaseCount = len(d.UpcomingReleases) + len(d.RecentReleases)
	d.History, err = a.store.DeliveryHistory(r.Context(), session.User.ID, 10)
	pageFailed = a.pageStoreError(r, &d, "Dashboard", "delivery history", err) || pageFailed
	d.Resolutions, err = a.store.ArtistResolutions(r.Context(), session.User.ID)
	pageFailed = a.pageStoreError(r, &d, "Dashboard", "artist resolutions", err) || pageFailed
	d.Preferences, err = a.store.NotificationPreferences(r.Context(), session.User.ID)
	pageFailed = a.pageStoreError(r, &d, "Dashboard", "notification preferences", err) || pageFailed
	d.NotificationHolds, err = a.store.NotificationHolds(r.Context(), session.User.ID, 20)
	pageFailed = a.pageStoreError(r, &d, "Dashboard", "notification holds", err) || pageFailed
	d.ListenBrainzArtists, err = a.store.TopListenBrainzArtists(r.Context(), session.User.ID, 5)
	pageFailed = a.pageStoreError(r, &d, "Dashboard", "ListenBrainz popularity", err) || pageFailed
	d.GenreBreakdown, err = a.store.FollowedBreakdown(r.Context(), session.User.ID, "genre")
	pageFailed = a.pageStoreError(r, &d, "Dashboard", "genre breakdown", err) || pageFailed
	d.CountryBreakdown, err = a.store.FollowedBreakdown(r.Context(), session.User.ID, "country")
	pageFailed = a.pageStoreError(r, &d, "Dashboard", "country breakdown", err) || pageFailed
	d.TypeBreakdown, err = a.store.FollowedBreakdown(r.Context(), session.User.ID, "type")
	pageFailed = a.pageStoreError(r, &d, "Dashboard", "artist type breakdown", err) || pageFailed
	status := http.StatusOK
	if pageFailed {
		status = http.StatusInternalServerError
	}
	a.render(w, "dashboard", d, status)
}
func (a *App) releaseDetail(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	d := a.data(r, "Release details")
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		a.logger.Debug("release detail unavailable", "release_id", chi.URLParam(r, "id"), "error", "invalid release ID")
		d.ReleaseUnavailable = true
		a.render(w, "release", d, http.StatusNotFound)
		return
	}
	detail, err := a.store.ReleaseDetail(r.Context(), session.User.ID, id)
	if err != nil {
		a.logger.Debug("release detail unavailable", "release_id", id, "error", err)
		d.ReleaseUnavailable = true
		if errors.Is(err, sql.ErrNoRows) {
			a.render(w, "release", d, http.StatusNotFound)
		} else {
			a.pageStoreError(r, &d, "Release details", "release lookup", err)
			a.render(w, "release", d, http.StatusInternalServerError)
		}
		return
	}
	d.ReleaseDetail = &detail
	d.ReleaseEvidenceIssues, err = a.store.EvidenceIssuesForRelease(r.Context(), session.User.ID, id, time.Now().UTC())
	if a.pageStoreError(r, &d, "Release details", "release evidence issues", err) {
		a.render(w, "release", d, http.StatusInternalServerError)
		return
	}
	d.ReleaseNotificationHolds, err = a.store.NotificationHoldsForRelease(r.Context(), session.User.ID, id)
	if a.pageStoreError(r, &d, "Release details", "release notification holds", err) {
		a.render(w, "release", d, http.StatusInternalServerError)
		return
	}
	a.render(w, "release", d, http.StatusOK)
}
