package web

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const inboxPageSize = 50

func (a *App) inbox(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	d := a.data(r, "Release inbox")
	d.InboxState = inboxFilter(r.URL.Query().Get("state"), "unread", "read", "snoozed", "dismissed")
	d.InboxSource = inboxFilter(r.URL.Query().Get("source"), "musicbrainz", "spotify", "itunes", "both")
	d.InboxType = inboxFilter(r.URL.Query().Get("type"), "album", "ep", "single")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	now := time.Now().UTC()
	pageFailed := false
	var err error
	d.InboxUnreadCount, err = a.store.ReleaseInboxUnreadCount(r.Context(), session.User.ID, now)
	pageFailed = a.pageStoreError(r, &d, "Release inbox", "unread release count", err) || pageFailed
	d.EvidenceIssueUnreadCount, err = a.store.EvidenceIssueUnreadCount(r.Context(), session.User.ID, now)
	pageFailed = a.pageStoreError(r, &d, "Release inbox", "unread evidence issue count", err) || pageFailed
	d.InboxCount, err = a.store.ReleaseInboxCount(r.Context(), session.User.ID, d.InboxState, d.InboxSource, d.InboxType, now)
	pageFailed = a.pageStoreError(r, &d, "Release inbox", "release inbox count", err) || pageFailed
	pages := (d.InboxCount + inboxPageSize - 1) / inboxPageSize
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	d.InboxPage, d.InboxPages = page, pages
	d.InboxURL = inboxPageURL(d.InboxState, d.InboxSource, d.InboxType, page)
	if d.InboxCount > 0 {
		d.InboxPageStart = (page-1)*inboxPageSize + 1
		d.InboxPageEnd = page * inboxPageSize
		if d.InboxPageEnd > d.InboxCount {
			d.InboxPageEnd = d.InboxCount
		}
	}
	if page > 1 {
		d.InboxPrevURL = inboxPageURL(d.InboxState, d.InboxSource, d.InboxType, page-1)
	}
	if page < pages {
		d.InboxNextURL = inboxPageURL(d.InboxState, d.InboxSource, d.InboxType, page+1)
	}
	if pages > 1 {
		d.InboxPageLinks = make([]PaginationLink, 0, pages)
		for number := 1; number <= pages; number++ {
			d.InboxPageLinks = append(d.InboxPageLinks, PaginationLink{
				Number: number, URL: inboxPageURL(d.InboxState, d.InboxSource, d.InboxType, number), Current: number == page,
			})
		}
	}
	d.InboxItems, err = a.store.ReleaseInbox(r.Context(), session.User.ID, d.InboxState, d.InboxSource, d.InboxType,
		inboxPageSize, (page-1)*inboxPageSize, now)
	pageFailed = a.pageStoreError(r, &d, "Release inbox", "release inbox items", err) || pageFailed
	status := http.StatusOK
	if pageFailed {
		status = http.StatusInternalServerError
	}
	a.render(w, "inbox", d, status)
}

func inboxFilter(value string, allowed ...string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return ""
}

func inboxPageURL(state, source, primaryType string, page int) string {
	values := url.Values{}
	if state != "" {
		values.Set("state", state)
	}
	if source != "" {
		values.Set("source", source)
	}
	if primaryType != "" {
		values.Set("type", primaryType)
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	if encoded := values.Encode(); encoded != "" {
		return "/inbox?" + encoded
	}
	return "/inbox"
}

func (a *App) inboxStateAction(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	action := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "action")))
	if action == "dismiss" {
		action = "dismissed"
	}
	state := action
	var snoozedUntil *time.Time
	if action == "snooze" {
		var duration time.Duration
		switch strings.TrimSpace(r.FormValue("duration")) {
		case "1h":
			duration = time.Hour
		case "1d":
			duration = 24 * time.Hour
		case "7d":
			duration = 7 * 24 * time.Hour
		default:
			http.Error(w, "invalid snooze duration", http.StatusBadRequest)
			return
		}
		until := time.Now().UTC().Add(duration)
		snoozedUntil = &until
		state = "snoozed"
	}
	if state != "read" && state != "unread" && state != "snoozed" && state != "dismissed" {
		http.NotFound(w, r)
		return
	}
	if err := a.store.SetReleaseInboxState(r.Context(), session.User.ID, id, state, snoozedUntil); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		a.logger.Error("release inbox state update failed", "path", r.URL.Path,
			"user_id", session.User.ID, "release_id", id, "error", err)
		http.Error(w, "release inbox could not be updated", http.StatusInternalServerError)
		return
	}
	redirect := localReturnPath(r.FormValue("return"), "/inbox", "/inbox")
	if state == "read" {
		redirect = inboxReadRedirect(redirect)
	}
	separator := "?"
	if strings.Contains(redirect, "?") {
		separator = "&"
	}
	http.Redirect(w, r, redirect+separator+a.statusQuery("Release inbox updated"), http.StatusSeeOther)
}

// inboxReadRedirect returns the first page while preserving any explicit
// source/type/state filters. This keeps the next unread item at the top after
// a member marks the current item read.
func inboxReadRedirect(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Path != "/inbox" {
		return "/inbox"
	}
	query := parsed.Query()
	query.Del("page")
	parsed.RawQuery = query.Encode()
	parsed.Fragment = ""
	if parsed.RawQuery == "" {
		return parsed.Path
	}
	return parsed.Path + "?" + parsed.RawQuery
}
