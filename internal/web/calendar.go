package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	// Build the last calendar day by date rather than subtracting 24 hours
	// from the next month. The latter can land on the wrong local date around
	// DST transitions.
	toTime := time.Date(month.Year(), month.Month()+1, 1, 0, 0, 0, 0, location).AddDate(0, 0, -1)
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
	for len(data) > 75 {
		cut := 75
		for cut > 0 && cut < len(data) && data[cut]&0xc0 == 0x80 {
			cut--
		}
		if cut == 0 {
			cut = 75
		}
		builder.Write(data[:cut])
		builder.WriteString("\r\n ")
		data = data[cut:]
	}
	builder.Write(data)
	builder.WriteString("\r\n")
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
