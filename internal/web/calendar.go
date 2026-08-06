package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crypt0rr/artist-tracker/internal/store"
)

func (a *App) calendar(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	d := a.data(r, "Release calendar")
	location, err := time.LoadLocation(session.User.Timezone)
	if err != nil {
		location = time.UTC
	}
	now := time.Now().In(location)
	month := now
	if value := strings.TrimSpace(r.URL.Query().Get("month")); value != "" {
		if parsed, parseErr := time.ParseInLocation("2006-01", value, location); parseErr == nil {
			month = parsed
		}
	}
	fromTime := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, location)
	toTime := fromTime.AddDate(0, 1, 0).Add(-24 * time.Hour)
	from := fromTime.Format("2006-01-02")
	to := toTime.Format("2006-01-02")
	releases, err := a.store.CalendarReleases(r.Context(), session.User.ID, from, to, 300)
	pageFailed := a.pageStoreError(r, &d, "Release calendar", "calendar releases", err)
	d.CalendarMonth = fromTime.Format("January 2006")
	d.CalendarPrevMonth = fromTime.AddDate(0, -1, 0).Format("2006-01")
	d.CalendarNextMonth = fromTime.AddDate(0, 1, 0).Format("2006-01")
	d.CalendarICSURL = "/calendar.ics"
	byDate := make(map[string][]store.CalendarRelease)
	for _, release := range releases {
		byDate[release.CalendarDate] = append(byDate[release.CalendarDate], release)
	}
	for date := fromTime; !date.After(toTime); date = date.AddDate(0, 0, 1) {
		key := date.Format("2006-01-02")
		if items := byDate[key]; len(items) > 0 {
			d.CalendarDays = append(d.CalendarDays, CalendarDay{
				Date: key, Label: date.Format("Mon, Jan 2"),
				Today: key == now.Format("2006-01-02"), Releases: items,
			})
		}
	}
	status := http.StatusOK
	if pageFailed {
		status = http.StatusInternalServerError
	}
	a.render(w, "calendar", d, status)
}

func (a *App) calendarICS(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	location, err := time.LoadLocation(session.User.Timezone)
	if err != nil {
		location = time.UTC
	}
	now := time.Now().In(location)
	from := now.Format("2006-01-02")
	to := now.AddDate(1, 0, 0).Format("2006-01-02")
	releases, err := a.store.CalendarReleases(r.Context(), session.User.ID, from, to, 500)
	if err != nil {
		a.logger.Error("calendar export failed", "operation", "calendar releases", "path", r.URL.Path, "error", err)
		http.Error(w, "calendar export unavailable", http.StatusInternalServerError)
		return
	}
	publicURL := a.cfg.PublicURL
	var builder strings.Builder
	builder.WriteString("BEGIN:VCALENDAR\r\n")
	builder.WriteString("VERSION:2.0\r\n")
	builder.WriteString("PRODID:-//ArtistTrackarr//Release Calendar//EN\r\n")
	builder.WriteString("METHOD:PUBLISH\r\n")
	builder.WriteString("CALSCALE:GREGORIAN\r\n")
	builder.WriteString("X-WR-CALNAME:ArtistTrackarr releases\r\n")
	builder.WriteString("X-WR-TIMEZONE:" + icsEscape(location.String()) + "\r\n")
	stamp := time.Now().UTC().Format("20060102T150405Z")
	for _, release := range releases {
		builder.WriteString("BEGIN:VEVENT\r\n")
		builder.WriteString("UID:release-" + strconv.FormatInt(release.ID, 10) + "@artisttrackarr\r\n")
		builder.WriteString("DTSTAMP:" + stamp + "\r\n")
		builder.WriteString("DTSTART;VALUE=DATE:" + strings.ReplaceAll(release.CalendarDate, "-", "") + "\r\n")
		builder.WriteString("SUMMARY:" + icsEscape(release.Title+" — "+release.ArtistName) + "\r\n")
		description := fmt.Sprintf("%s · %s · %s", release.PrimaryType, release.Source, calendarReleaseStatus(release))
		if link := releaseExternalLink(release); link != "" {
			description += "\n" + link
		}
		builder.WriteString("DESCRIPTION:" + icsEscape(description) + "\r\n")
		if publicURL != nil {
			link := publicURL.ResolveReference(&url.URL{Path: "/releases/" + strconv.FormatInt(release.ID, 10)}).String()
			builder.WriteString("URL:" + icsEscape(link) + "\r\n")
		}
		builder.WriteString("END:VEVENT\r\n")
	}
	builder.WriteString("END:VCALENDAR\r\n")
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="artisttrackarr-releases.ics"`)
	w.Header().Set("Cache-Control", "private, max-age=300")
	_, _ = w.Write([]byte(builder.String()))
}

func icsEscape(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, ";", `\;`)
	value = strings.ReplaceAll(value, ",", `\,`)
	value = strings.ReplaceAll(value, "\r\n", "\\n")
	value = strings.ReplaceAll(value, "\n", "\\n")
	return value
}

func releaseExternalLink(release store.CalendarRelease) string {
	if release.SpotifyURL != "" {
		return release.SpotifyURL
	}
	if release.ITunesURL != "" {
		return release.ITunesURL
	}
	return release.MusicBrainzURL
}

func calendarReleaseStatus(release store.CalendarRelease) string {
	if release.Held {
		return "held for review"
	}
	if release.TruthIssueCount > 0 {
		return "review required"
	}
	if release.Confidence == "confirmed" || release.SourceCount > 1 {
		return "confirmed"
	}
	return "single source"
}
