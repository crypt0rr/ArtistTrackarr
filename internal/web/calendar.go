package web

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/crypt0rr/artist-tracker/internal/store"
)

const (
	calendarExportPageSize = 500
	calendarExportMax      = 5000
	// calendarMonthLimit bounds the month grid. The grid is a browsable view
	// rather than an export, so it stays bounded and says so instead of paging
	// an unusually large watchlist into a single page.
	calendarMonthLimit = 300
)

var errCalendarExportTooLarge = errors.New("calendar export exceeds the safety limit")

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
	fromTime, toTime := calendarMonthBounds(month, location)
	from := fromTime.Format("2006-01-02")
	to := toTime.Format("2006-01-02")
	// Over-fetch by one so a truncated month is detectable. The grid only needs
	// to know that more exists, so this is cheaper than the export path's
	// separate probe request.
	releases, err := a.store.CalendarReleasesPage(r.Context(), session.User.ID, from, to, calendarMonthLimit+1, 0)
	pageFailed := a.pageStoreError(r, &d, "Release calendar", "calendar releases", err)
	if len(releases) > calendarMonthLimit {
		releases = releases[:calendarMonthLimit]
		d.CalendarNotice = fmt.Sprintf(
			"This month has more than %d dated releases. Showing the first %d — subscribe to the calendar feed or download the ICS file for the complete list.",
			calendarMonthLimit, calendarMonthLimit)
	}
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

// calendarMonthBounds returns the first and last local dates in month. Date
// arithmetic is intentionally used instead of subtracting a fixed duration:
// a spring-forward transition can make the elapsed time between two local
// midnights shorter than 24 hours, which would otherwise move the upper bound
// onto the wrong calendar date. Constructing the final day from date parts
// also keeps this safe for zones that change their offset at midnight.
func calendarMonthBounds(month time.Time, location *time.Location) (time.Time, time.Time) {
	if location == nil {
		location = time.UTC
	}
	from := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, location)
	next := time.Date(month.Year(), month.Month()+1, 1, 0, 0, 0, 0, location)
	last := time.Date(next.Year(), next.Month(), 0, 0, 0, 0, 0, location)
	return from, last
}

func (a *App) calendarICS(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	a.writeCalendarICS(w, r, session.User)
}

// calendarFeed serves the owner-scoped ICS export without requiring a browser
// session. The opaque token is revocable and expires automatically; invalid
// credentials intentionally look identical to a missing feed.
func (a *App) calendarFeed(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(chi.URLParam(r, "token"))
	if a.calendarFeedLimiter != nil && !a.calendarFeedLimiter.Allow(raw) {
		rateLimited(w, 60, "calendar feed requests are temporarily rate limited; try again later")
		return
	}
	userID, err := a.store.UserIDByCalendarFeedToken(r.Context(), raw)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		if a.logger != nil {
			a.logger.Error("calendar feed token lookup failed", "path", r.URL.Path, "error", err)
		}
		http.Error(w, "calendar feed unavailable", http.StatusInternalServerError)
		return
	}
	user, err := a.store.UserByID(r.Context(), userID)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		if a.logger != nil {
			a.logger.Error("calendar feed owner lookup failed", "user_id", userID, "error", err)
		}
		http.Error(w, "calendar feed unavailable", http.StatusInternalServerError)
		return
	}
	a.writeCalendarICS(w, r, user)
}

func (a *App) writeCalendarICS(w http.ResponseWriter, r *http.Request, user store.User) {
	location, err := time.LoadLocation(user.Timezone)
	if err != nil {
		location = time.UTC
	}
	now := time.Now().In(location)
	from := now.Format("2006-01-02")
	to := now.AddDate(1, 0, 0).Format("2006-01-02")
	releases, err := a.calendarExportReleases(r, user.ID, from, to)
	if err != nil {
		if a.logger != nil {
			a.logger.Error("calendar export failed", "operation", "calendar releases", "path", r.URL.Path, "error", err)
		}
		if errors.Is(err, errCalendarExportTooLarge) {
			http.Error(w, "calendar export is too large; narrow the date range", http.StatusUnprocessableEntity)
		} else {
			http.Error(w, "calendar export unavailable", http.StatusInternalServerError)
		}
		return
	}
	publicURL := a.cfg.PublicURL
	var builder strings.Builder
	writeICSLine(&builder, "BEGIN:VCALENDAR")
	writeICSLine(&builder, "VERSION:2.0")
	writeICSLine(&builder, "PRODID:-//ArtistTrackarr//Release Calendar//EN")
	writeICSLine(&builder, "METHOD:PUBLISH")
	writeICSLine(&builder, "CALSCALE:GREGORIAN")
	writeICSLine(&builder, "X-WR-CALNAME:ArtistTrackarr releases")
	writeICSLine(&builder, "X-WR-TIMEZONE:"+icsEscape(location.String()))
	stamp := time.Now().UTC().Format("20060102T150405Z")
	for _, release := range releases {
		writeICSLine(&builder, "BEGIN:VEVENT")
		writeICSLine(&builder, "UID:release-"+strconv.FormatInt(release.ID, 10)+"@artisttrackarr")
		writeICSLine(&builder, "DTSTAMP:"+stamp)
		writeICSLine(&builder, "DTSTART;VALUE=DATE:"+strings.ReplaceAll(release.CalendarDate, "-", ""))
		writeICSLine(&builder, "SUMMARY:"+icsEscape(release.Title+" — "+release.ArtistName))
		description := fmt.Sprintf("%s · %s · %s", release.PrimaryType, release.Source, calendarReleaseStatus(release))
		if link := releaseExternalLink(release); link != "" {
			description += "\n" + link
		}
		writeICSLine(&builder, "DESCRIPTION:"+icsEscape(description))
		if publicURL != nil {
			link := publicURL.ResolveReference(&url.URL{Path: "/releases/" + strconv.FormatInt(release.ID, 10)}).String()
			writeICSLine(&builder, "URL:"+icsEscapeURI(link))
		}
		writeICSLine(&builder, "END:VEVENT")
	}
	writeICSLine(&builder, "END:VCALENDAR")
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="artisttrackarr-releases.ics"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(builder.String()))
}

// calendarExportReleases paginates the full one-year export and fails loudly
// at a generous safety cap instead of silently omitting releases.
func (a *App) calendarExportReleases(r *http.Request, userID int64, from, to string) ([]store.CalendarRelease, error) {
	all := make([]store.CalendarRelease, 0, calendarExportPageSize)
	offset := 0
	for {
		remaining := calendarExportMax - len(all)
		if remaining <= 0 {
			probe, err := a.store.CalendarReleasesPage(r.Context(), userID, from, to, 1, offset)
			if err != nil {
				return nil, err
			}
			if len(probe) > 0 {
				return nil, errCalendarExportTooLarge
			}
			return all, nil
		}
		limit := calendarExportPageSize
		if limit > remaining {
			limit = remaining
		}
		page, err := a.store.CalendarReleasesPage(r.Context(), userID, from, to, limit, offset)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < limit {
			return all, nil
		}
		offset += len(page)
	}
}

func icsEscape(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, ";", `\;`)
	value = strings.ReplaceAll(value, ",", `\,`)
	value = strings.ReplaceAll(value, "\r\n", "\\n")
	value = strings.ReplaceAll(value, "\r", "\\n")
	value = strings.ReplaceAll(value, "\n", "\\n")
	return value
}

// icsEscapeURI returns a URI-safe value for the RFC 5545 URL property. URI
// punctuation remains intact, while whitespace, non-ASCII query bytes, and
// malformed percent escapes are normalized. Control characters are removed
// before parsing so a provider-controlled value can never inject another
// calendar property.
func icsEscapeURI(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	parsed, err := url.Parse(value)
	if err != nil {
		return escapeURIQuery(value)
	}
	parsed.RawQuery = escapeURIQuery(parsed.RawQuery)
	return parsed.String()
}

// escapeURIQuery escapes bytes that cannot appear literally in a URI query,
// preserving delimiters such as '&' and '='. Existing valid percent escapes
// are retained; malformed '%' bytes become %25 instead of producing an
// invalid or ambiguous URL property.
func escapeURIQuery(value string) string {
	var builder strings.Builder
	for index := 0; index < len(value); {
		character := value[index]
		if character == '%' && index+2 < len(value) && isHexByte(value[index+1]) && isHexByte(value[index+2]) {
			builder.WriteString(value[index : index+3])
			index += 3
			continue
		}
		if character >= 0x21 && character <= 0x7e && character != '%' {
			builder.WriteByte(character)
			index++
			continue
		}
		_, size := utf8.DecodeRuneInString(value[index:])
		if size == 0 {
			size = 1
		}
		for offset := 0; offset < size && index+offset < len(value); offset++ {
			fmt.Fprintf(&builder, "%%%02X", value[index+offset])
		}
		index += size
	}
	return builder.String()
}

func isHexByte(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'f') || (value >= 'A' && value <= 'F')
}

// writeICSLine applies RFC 5545 line folding at 75 octets. It avoids cutting
// through a UTF-8 continuation byte and prefixes each continuation line with
// the required single space.
func writeICSLine(builder *strings.Builder, line string) {
	data := []byte(line)
	first := true
	for len(data) > 0 {
		// A continuation line consumes one octet for its required leading
		// space, so its payload is at most 74 octets. The previous implementation
		// allowed 75 payload octets on continuation lines, producing 76-octet
		// physical lines for ASCII-heavy titles.
		width := 75
		if !first {
			builder.WriteByte(' ')
			width = 74
		}
		cut := width
		if cut > len(data) {
			cut = len(data)
		}
		for cut > 0 && cut < len(data) && data[cut]&0xc0 == 0x80 {
			cut--
		}
		if cut == 0 {
			// A valid UTF-8 string cannot have a code point wider than the
			// folding width. Keep a defensive fallback for malformed input.
			cut = width
			if cut > len(data) {
				cut = len(data)
			}
		}
		builder.Write(data[:cut])
		builder.WriteString("\r\n")
		data = data[cut:]
		first = false
	}
	if first {
		// Empty properties still need their terminating line ending.
		builder.WriteString("\r\n")
	}
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
