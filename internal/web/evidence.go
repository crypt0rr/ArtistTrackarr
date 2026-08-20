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

const evidenceIssuePageSize = 50

func (a *App) evidenceIssues(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	d := a.data(r, "Release Truth Desk")
	d.EvidenceIssueStatus = evidenceFilter(r.URL.Query().Get("status"), "open", "resolved")
	if d.EvidenceIssueStatus == "" {
		d.EvidenceIssueStatus = "open"
	}
	d.EvidenceIssueState = evidenceFilter(r.URL.Query().Get("state"), "unread", "all", "confirmed", "snoozed", "dismissed")
	if d.EvidenceIssueState == "" {
		d.EvidenceIssueState = "unread"
	}
	d.EvidenceIssueType = evidenceFilter(r.URL.Query().Get("type"), "date_conflict", "title_conflict", "type_conflict", "missing_canonical")
	d.EvidenceIssueSeverity = evidenceFilter(r.URL.Query().Get("severity"), "info", "warning", "critical")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	now := time.Now().UTC()
	pageFailed := false
	var err error
	d.EvidenceIssueUnreadCount, err = a.store.EvidenceIssueUnreadCount(r.Context(), session.User.ID, now)
	pageFailed = a.pageStoreError(r, &d, "Release Truth Desk", "unread evidence issue count", err) || pageFailed
	d.EvidenceIssueCount, err = a.store.EvidenceIssueCount(r.Context(), session.User.ID, d.EvidenceIssueStatus,
		d.EvidenceIssueState, d.EvidenceIssueType, d.EvidenceIssueSeverity, now)
	pageFailed = a.pageStoreError(r, &d, "Release Truth Desk", "evidence issue count", err) || pageFailed
	pages := (d.EvidenceIssueCount + evidenceIssuePageSize - 1) / evidenceIssuePageSize
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	d.EvidenceIssuePage, d.EvidenceIssuePages = page, pages
	d.EvidenceIssueURL = evidenceIssuePageURL(d.EvidenceIssueStatus, d.EvidenceIssueState, d.EvidenceIssueType,
		d.EvidenceIssueSeverity, page)
	if d.EvidenceIssueCount > 0 {
		d.EvidenceIssuePageStart = (page-1)*evidenceIssuePageSize + 1
		d.EvidenceIssuePageEnd = page * evidenceIssuePageSize
		if d.EvidenceIssuePageEnd > d.EvidenceIssueCount {
			d.EvidenceIssuePageEnd = d.EvidenceIssueCount
		}
	}
	if page > 1 {
		d.EvidenceIssuePrevURL = evidenceIssuePageURL(d.EvidenceIssueStatus, d.EvidenceIssueState,
			d.EvidenceIssueType, d.EvidenceIssueSeverity, page-1)
	}
	if page < pages {
		d.EvidenceIssueNextURL = evidenceIssuePageURL(d.EvidenceIssueStatus, d.EvidenceIssueState,
			d.EvidenceIssueType, d.EvidenceIssueSeverity, page+1)
	}
	if pages > 1 {
		d.EvidenceIssuePageLinks = make([]PaginationLink, 0, pages)
		for number := 1; number <= pages; number++ {
			d.EvidenceIssuePageLinks = append(d.EvidenceIssuePageLinks, PaginationLink{
				Number: number, URL: evidenceIssuePageURL(d.EvidenceIssueStatus, d.EvidenceIssueState,
					d.EvidenceIssueType, d.EvidenceIssueSeverity, number), Current: number == page,
			})
		}
	}
	d.EvidenceIssues, err = a.store.EvidenceIssues(r.Context(), session.User.ID, d.EvidenceIssueStatus,
		d.EvidenceIssueState, d.EvidenceIssueType, d.EvidenceIssueSeverity, evidenceIssuePageSize,
		(page-1)*evidenceIssuePageSize, now)
	pageFailed = a.pageStoreError(r, &d, "Release Truth Desk", "evidence issue list", err) || pageFailed
	status := http.StatusOK
	if pageFailed {
		status = http.StatusInternalServerError
	}
	a.render(w, "evidence_issues", d, status)
}

func evidenceFilter(value string, allowed ...string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return ""
}

func evidenceIssuePageURL(status, state, issueType, severity string, page int) string {
	values := url.Values{}
	if status != "" && status != "open" {
		values.Set("status", status)
	}
	if state != "" && state != "unread" {
		values.Set("state", state)
	}
	if issueType != "" {
		values.Set("type", issueType)
	}
	if severity != "" {
		values.Set("severity", severity)
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	if encoded := values.Encode(); encoded != "" {
		return "/coverage/issues?" + encoded
	}
	return "/coverage/issues"
}

func (a *App) evidenceIssueStateAction(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	action := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "action")))
	state := action
	var snoozedUntil *time.Time
	if action == "confirm" {
		state = "confirmed"
	}
	if action == "dismiss" {
		state = "dismissed"
	}
	if action == "restore" {
		state = "unread"
	}
	if action == "snooze" {
		var duration time.Duration
		switch strings.TrimSpace(r.FormValue("duration")) {
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
	if state != "confirmed" && state != "snoozed" && state != "dismissed" && state != "unread" {
		http.NotFound(w, r)
		return
	}
	if err := a.store.SetEvidenceIssueState(r.Context(), session.User.ID, id, state, snoozedUntil); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		a.logger.Error("release evidence issue state update failed", "path", r.URL.Path,
			"user_id", session.User.ID, "issue_id", id, "error", err)
		http.Error(w, "release evidence issue could not be updated", http.StatusInternalServerError)
		return
	}
	redirect := localReturnPath(r.FormValue("return"), "/coverage/issues", "/coverage/issues")
	separator := "?"
	if strings.Contains(redirect, "?") {
		separator = "&"
	}
	http.Redirect(w, r, redirect+separator+a.statusQuery("Evidence review updated"), http.StatusSeeOther)
}
