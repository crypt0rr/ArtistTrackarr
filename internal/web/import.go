package web

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

const (
	maxArtistImportBytes = 1 << 20
	maxArtistImportRows  = 500
	maxConcurrentImports = 2
)

var requiredArtistImportColumns = [...]string{
	"artist", "display_name", "musicbrainz_id", "musicbrainz_url", "spotify_id", "spotify_url",
}

// parseArtistTrackarrCSV accepts the exact schema emitted by exportArtists.
// Unknown columns are ignored so future exports remain backwards compatible;
// required columns may appear in any order.
func parseArtistTrackarrCSV(reader io.Reader) ([]store.ImportInput, error) {
	csvReader := csv.NewReader(reader)
	csvReader.FieldsPerRecord = -1
	csvReader.TrimLeadingSpace = true
	header, err := csvReader.Read()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the CSV file is empty")
		}
		return nil, fmt.Errorf("read CSV header: %w", err)
	}
	if len(header) > 0 {
		header[0] = strings.TrimPrefix(header[0], "\ufeff")
	}
	columns := make(map[string]int, len(header))
	for index, value := range header {
		name := strings.ToLower(strings.TrimSpace(value))
		if name == "" {
			continue
		}
		if _, exists := columns[name]; exists {
			return nil, fmt.Errorf("duplicate CSV column %q", name)
		}
		columns[name] = index
	}
	for _, required := range requiredArtistImportColumns {
		if _, ok := columns[required]; !ok {
			return nil, fmt.Errorf("CSV is missing required column %q", required)
		}
	}

	var result []store.ImportInput
	for rowNumber := 2; ; rowNumber++ {
		record, readErr := csvReader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read CSV row %d: %w", rowNumber, readErr)
		}
		if len(result) >= maxArtistImportRows {
			return nil, fmt.Errorf("CSV contains more than %d data rows", maxArtistImportRows)
		}
		input := store.ImportInput{
			SourceValue:     csvField(record, columns["artist"]),
			DisplayName:     csvField(record, columns["display_name"]),
			SortName:        csvOptionalField(record, columns, "sort_name"),
			ArtistType:      csvOptionalField(record, columns, "artist_type"),
			Country:         csvOptionalField(record, columns, "country"),
			Disambiguation:  csvOptionalField(record, columns, "disambiguation"),
			MBID:            strings.ToLower(csvField(record, columns["musicbrainz_id"])),
			MBURL:           csvField(record, columns["musicbrainz_url"]),
			SpotifyID:       csvField(record, columns["spotify_id"]),
			SpotifyURL:      csvField(record, columns["spotify_url"]),
			SpotifyImageURL: csvOptionalField(record, columns, "spotify_image_url"),
		}
		input.Reason = validateArtistImportInput(input)
		if input.Reason == "" && input.SpotifyID != "" {
			input.SpotifyID, _ = catalog.SpotifyID(input.SpotifyID)
		}
		result = append(result, input)
	}
	if len(result) == 0 {
		return nil, errors.New("CSV contains no data rows")
	}
	return result, nil
}

func csvField(record []string, index int) string {
	if index < 0 || index >= len(record) {
		return ""
	}
	return strings.TrimSpace(record[index])
}

func csvOptionalField(record []string, columns map[string]int, name string) string {
	index, ok := columns[name]
	if !ok {
		return ""
	}
	return csvField(record, index)
}

func validateArtistImportInput(input store.ImportInput) string {
	if input.DisplayName == "" {
		return "display name is required"
	}
	if utf8.RuneCountInString(input.DisplayName) > 256 {
		return "display name is too long (maximum 256 characters)"
	}
	for _, field := range []struct {
		label string
		value string
	}{
		{label: "sort name", value: input.SortName},
		{label: "artist type", value: input.ArtistType},
		{label: "country", value: input.Country},
		{label: "disambiguation", value: input.Disambiguation},
	} {
		if utf8.RuneCountInString(field.value) > 256 {
			return field.label + " is too long (maximum 256 characters)"
		}
	}
	if input.SourceValue == "" {
		return "artist source value is required"
	}
	mbid := strings.ToLower(strings.TrimSpace(input.MBID))
	if !validMBID(mbid) {
		return "invalid MusicBrainz ID"
	}
	mbURL, ok := validMusicBrainzArtistURL(input.MBURL)
	if !ok || !strings.EqualFold(path.Base(strings.TrimSuffix(mbURL.Path, "/")), mbid) {
		return "invalid MusicBrainz artist URL"
	}
	if !strings.EqualFold(input.SourceValue, mbid) && !strings.EqualFold(input.SourceValue, mbURL.String()) {
		return "artist source does not match the MusicBrainz identity"
	}
	if input.SpotifyID == "" && input.SpotifyURL != "" {
		return "Spotify ID is required when a Spotify URL is provided"
	}
	if input.SpotifyID != "" {
		spotifyID, valid := catalog.SpotifyID(input.SpotifyID)
		if !valid {
			return "invalid Spotify artist ID"
		}
		if input.SpotifyURL == "" {
			return "Spotify URL is required when a Spotify ID is provided"
		}
		spotifyURL, valid := validSpotifyArtistURL(input.SpotifyURL)
		if !valid || !strings.EqualFold(path.Base(strings.TrimSuffix(spotifyURL.Path, "/")), spotifyID) {
			return "invalid Spotify artist URL"
		}
		input.SpotifyID = spotifyID
	}
	if input.SpotifyImageURL != "" && !validSpotifyImageURL(input.SpotifyImageURL) {
		return "invalid Spotify image URL"
	}
	return ""
}

func validMBID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !isHexDigit(character) {
			return false
		}
	}
	return true
}

func isHexDigit(character rune) bool {
	return (character >= '0' && character <= '9') ||
		(character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')
}

func validMusicBrainzArtistURL(value string) (*url.URL, bool) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.Host != "musicbrainz.org" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 2 || parts[0] != "artist" || !validMBID(parts[1]) {
		return nil, false
	}
	return parsed, true
}

func validSpotifyArtistURL(value string) (*url.URL, bool) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.Host != "open.spotify.com" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 2 || parts[0] != "artist" {
		return nil, false
	}
	if _, ok := catalog.SpotifyID(parts[1]); !ok {
		return nil, false
	}
	return parsed, true
}

func validSpotifyImageURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	return host == "i.scdn.co" || host == "scdn.co" || strings.HasSuffix(host, ".spotifycdn.com")
}

func (a *App) importArtists(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	if !a.acquireImportSlot(w) {
		return
	}
	defer a.releaseImportSlot()
	key := strconv.FormatInt(session.User.ID, 10) + "|" + a.clientIP(r)
	if a.importLimiter != nil && !a.importLimiter.Allow(key) {
		rateLimited(w, 3600, "artist imports are temporarily rate limited; try again later")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		a.renderImportError(w, r, "Select an ArtistTrackarr CSV file.")
		return
	}
	defer func() { _ = file.Close() }()
	if header.Size > maxArtistImportBytes {
		a.renderImportError(w, r, "CSV files must be 1 MiB or smaller.")
		return
	}
	limited := io.LimitReader(file, maxArtistImportBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		a.renderImportError(w, r, "The CSV file could not be read.")
		return
	}
	if len(data) > maxArtistImportBytes {
		a.renderImportError(w, r, "CSV files must be 1 MiB or smaller.")
		return
	}
	inputs, err := parseArtistTrackarrCSV(strings.NewReader(string(data)))
	if err != nil {
		a.renderImportError(w, r, err.Error())
		return
	}
	job, err := a.runArtistImport(r.Context(), session.User.ID, inputs, data)
	if err != nil {
		a.logger.Error("create artist import job failed", "user_id", session.User.ID, "error", err)
		http.Error(w, "could not create import job", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/artists/imports/%d", job.ID), http.StatusSeeOther)
}

// importBookkeepingContext bounds a durable write that must survive the request
// it belongs to. An import runs on the HTTP request context under a request
// timeout; once that fires, recording what happened is exactly the work that
// must still complete.
func importBookkeepingContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	return context.WithTimeout(base, 5*time.Second)
}

// runArtistImport owns the durable per-row loop shared by fresh and resumed
// uploads. Every row is independent, so a local write failure leaves the rest
// of the upload useful while the terminal status tells the user it is
// incomplete. The original payload is retained only within the store's
// bounded import-job limit.
func (a *App) runArtistImport(ctx context.Context, userID int64, inputs []store.ImportInput, payload []byte) (store.ImportJob, error) {
	job, err := a.store.CreateImportJobWithPayload(ctx, userID, payload)
	if err != nil {
		return store.ImportJob{}, err
	}
	importStatus := "complete"
	for _, input := range inputs {
		// A large upload can outlive the request deadline: every row is its own
		// write transaction on the single writer, shared with the scheduler.
		// Stop at the first sign of that rather than failing every remaining row
		// against a dead context, which produced one error log per row and no
		// usable record of any of them.
		if ctxErr := ctx.Err(); ctxErr != nil {
			importStatus = "interrupted"
			a.logger.Warn("artist import interrupted before completing",
				"job_id", job.ID, "remaining", len(inputs), "error", ctxErr)
			break
		}
		if _, saveErr := a.store.SaveImportRow(ctx, userID, job.ID, input); saveErr != nil {
			if errors.Is(saveErr, context.Canceled) || errors.Is(saveErr, context.DeadlineExceeded) {
				importStatus = "interrupted"
				a.logger.Warn("artist import interrupted before completing",
					"job_id", job.ID, "error", saveErr)
				break
			}
			importStatus = "failed"
			a.logger.Error("save artist import row failed", "job_id", job.ID, "error", saveErr)
			// Keep the row visible even when one local write fails. This is an
			// invalid result, never a reason to discard the rest of the upload.
			// The recovery write gets its own budget: reusing a context that has
			// just expired discarded the row and swallowed the error, so the
			// failure never appeared in the results table.
			input.Reason = "could not save this row"
			rowCtx, cancelRow := importBookkeepingContext(ctx)
			_, _ = a.store.SaveImportRow(rowCtx, userID, job.ID, input)
			cancelRow()
		}
	}
	// Finishing must not depend on the context that just expired either: a job
	// left in "processing" is not resumable, so the member saw a partially
	// applied import with the Resume action hidden until the hourly sweep
	// recovered it.
	finishCtx, cancelFinish := importBookkeepingContext(ctx)
	defer cancelFinish()
	if err := a.store.FinishImportJob(finishCtx, userID, job.ID, importStatus); err != nil {
		a.logger.Error("finish artist import job failed", "job_id", job.ID, "error", err)
	}
	job.Status = importStatus
	return job, nil
}

func (a *App) acquireImportSlot(w http.ResponseWriter) bool {
	if a.importSlots == nil {
		return true
	}
	select {
	case a.importSlots <- struct{}{}:
		return true
	default:
		// Imports perform up to 500 independent writes. Refuse excess work
		// immediately instead of queueing unbounded requests behind SQLite's
		// single writer connection.
		rateLimited(w, 30, "artist imports are busy; try again shortly")
		return false
	}
}

func (a *App) releaseImportSlot() {
	if a.importSlots == nil {
		return
	}
	<-a.importSlots
}

func (a *App) renderImportError(w http.ResponseWriter, r *http.Request, message string) {
	d := a.data(r, "Artists")
	pageFailed := a.loadArtistsData(r, &d)
	d.Error = message
	status := http.StatusBadRequest
	if pageFailed {
		status = http.StatusInternalServerError
	}
	a.render(w, "artists", d, status)
}

func (a *App) artistImport(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	job, err := a.store.ImportJob(r.Context(), session.User.ID, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		d := a.data(r, "Artist import results")
		a.pageStoreError(r, &d, "Artist import results", "import job", err)
		a.render(w, "import", d, http.StatusInternalServerError)
		return
	}
	d := a.data(r, "Artist import results")
	d.Import = &job
	a.render(w, "import", d, http.StatusOK)
}

// resumeArtistImport replays the retained source payload into a fresh job.
// Replaying is intentionally idempotent: existing follows are reported as
// already followed, while rows that were never persisted during the original
// attempt are applied normally. The original interrupted/failed job remains
// as an audit record.
func (a *App) resumeArtistImport(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	if !a.acquireImportSlot(w) {
		return
	}
	defer a.releaseImportSlot()
	key := strconv.FormatInt(session.User.ID, 10) + "|" + a.clientIP(r)
	if a.importLimiter != nil && !a.importLimiter.Allow(key) {
		rateLimited(w, 3600, "artist imports are temporarily rate limited; try again later")
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	payload, err := a.store.ImportJobPayload(r.Context(), session.User.ID, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, store.ErrImportNotResumable) {
			a.renderImportResumeError(w, r, id, "This import is no longer resumable; upload the CSV again to start a new import.", http.StatusBadRequest)
			return
		}
		a.logger.Error("load artist import payload failed", "job_id", id, "error", err)
		a.renderImportResumeError(w, r, id, "The saved import could not be resumed.", http.StatusInternalServerError)
		return
	}
	if len(payload) > maxArtistImportBytes {
		a.renderImportResumeError(w, r, id, "The saved import is larger than the supported upload limit.", http.StatusBadRequest)
		return
	}
	inputs, err := parseArtistTrackarrCSV(strings.NewReader(string(payload)))
	if err != nil {
		a.renderImportResumeError(w, r, id, "The saved import is invalid; upload the CSV again to start a new import.", http.StatusBadRequest)
		return
	}
	job, err := a.runArtistImport(r.Context(), session.User.ID, inputs, payload)
	if err != nil {
		a.logger.Error("resume artist import failed", "source_job_id", id, "error", err)
		http.Error(w, "could not resume import", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/artists/imports/%d", job.ID), http.StatusSeeOther)
}

func (a *App) renderImportResumeError(w http.ResponseWriter, r *http.Request, id int64, message string, status int) {
	session, _ := currentSession(r)
	job, err := a.store.ImportJob(r.Context(), session.User.ID, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "import results are temporarily unavailable", http.StatusInternalServerError)
		return
	}
	d := a.data(r, "Artist import results")
	d.Import = &job
	d.Error = message
	a.render(w, "import", d, status)
}
