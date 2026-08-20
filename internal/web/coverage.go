package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const coveragePageSize = 50

func (a *App) coverage(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	d := a.data(r, "Release Trust Center")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	pageFailed := false
	var err error
	d.CoverageSummary, err = a.store.CoverageSummary(r.Context(), session.User.ID)
	pageFailed = a.pageStoreError(r, &d, "Release Trust Center", "coverage summary", err) || pageFailed
	d.EvidenceIssueUnreadCount, err = a.store.EvidenceIssueUnreadCount(r.Context(), session.User.ID, time.Now().UTC())
	pageFailed = a.pageStoreError(r, &d, "Release Trust Center", "release evidence issue count", err) || pageFailed
	pages := (d.CoverageSummary.Artists + coveragePageSize - 1) / coveragePageSize
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	d.CoveragePage, d.CoveragePages = page, pages
	if page > 1 {
		d.CoveragePrevPage = page - 1
		d.CoveragePrevURL = coveragePageURL(page - 1)
	}
	if page < pages {
		d.CoverageNextPage = page + 1
		d.CoverageNextURL = coveragePageURL(page + 1)
	}
	if d.CoverageSummary.Artists > 0 {
		d.CoveragePageStart = (page-1)*coveragePageSize + 1
		d.CoveragePageEnd = page * coveragePageSize
		if d.CoveragePageEnd > d.CoverageSummary.Artists {
			d.CoveragePageEnd = d.CoverageSummary.Artists
		}
	}
	if pages > 1 {
		d.CoveragePageLinks = make([]PaginationLink, 0, pages)
		for number := 1; number <= pages; number++ {
			d.CoveragePageLinks = append(d.CoveragePageLinks, PaginationLink{
				Number: number, URL: coveragePageURL(number), Current: number == page,
			})
		}
	}
	d.CoverageArtists, err = a.store.FollowedArtistCoveragePage(r.Context(), session.User.ID,
		coveragePageSize, (page-1)*coveragePageSize)
	pageFailed = a.pageStoreError(r, &d, "Release Trust Center", "coverage artist list", err) || pageFailed
	status := http.StatusOK
	if pageFailed {
		status = http.StatusInternalServerError
	}
	a.render(w, "coverage", d, status)
}

func coveragePageURL(page int) string {
	return "/coverage?page=" + url.QueryEscape(strconv.Itoa(page))
}

func (a *App) queueCoverageSync(w http.ResponseWriter, r *http.Request) {
	if !a.allowProviderAction(w, r) {
		return
	}
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	following, err := a.store.IsFollowing(r.Context(), session.User.ID, id)
	if err != nil {
		a.logger.Error("coverage artist lookup failed", "path", r.URL.Path, "user_id", session.User.ID,
			"artist_id", id, "error", err)
		http.Error(w, "could not load this artist", http.StatusInternalServerError)
		return
	}
	if !following {
		http.NotFound(w, r)
		return
	}
	if _, err := a.store.CreateManualSyncRequest(r.Context(), session.User.ID, "artist", &id); err != nil {
		a.logger.Error("coverage sync queue failed", "path", r.URL.Path, "user_id", session.User.ID,
			"artist_id", id, "error", err)
		http.Redirect(w, r, "/coverage?"+a.statusQuery("Synchronization could not be queued"), http.StatusSeeOther)
		return
	}
	if a.jobs != nil {
		a.jobs.Wake()
	}
	page := strings.TrimSpace(r.FormValue("page"))
	redirect := "/coverage?" + a.statusQuery("Synchronization queued")
	if page != "" {
		redirect += "&page=" + url.QueryEscape(page)
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}
