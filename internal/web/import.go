package web

import (
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

	"github.com/go-chi/chi/v5"

	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

const (
	maxArtistImportBytes = 1 << 20
	maxArtistImportRows  = 500
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
			SourceValue: csvField(record, columns["artist"]),
			DisplayName: csvField(record, columns["display_name"]),
			MBID:        strings.ToLower(csvField(record, columns["musicbrainz_id"])),
			MBURL:       csvField(record, columns["musicbrainz_url"]),
			SpotifyID:   csvField(record, columns["spotify_id"]),
			SpotifyURL:  csvField(record, columns["spotify_url"]),
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

func validateArtistImportInput(input store.ImportInput) string {
	if input.DisplayName == "" {
		return "display name is required"
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
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
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

func (a *App) importArtists(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	file, header, err := r.FormFile("file")
	if err != nil {
		a.renderImportError(w, r, "Select an ArtistTrackarr CSV file.")
		return
	}
	defer file.Close()
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
	job, err := a.store.CreateImportJob(r.Context(), session.User.ID)
	if err != nil {
		a.logger.Error("create artist import job failed", "user_id", session.User.ID, "error", err)
		http.Error(w, "could not create import job", http.StatusInternalServerError)
		return
	}
	for _, input := range inputs {
		if _, saveErr := a.store.SaveImportRow(r.Context(), session.User.ID, job.ID, input); saveErr != nil {
			a.logger.Error("save artist import row failed", "job_id", job.ID, "error", saveErr)
			// Keep the row visible even when one local write fails. This is an
			// invalid result, never a reason to discard the rest of the upload.
			input.Reason = "could not save this row"
			_, _ = a.store.SaveImportRow(r.Context(), session.User.ID, job.ID, input)
		}
	}
	http.Redirect(w, r, fmt.Sprintf("/artists/imports/%d", job.ID), http.StatusSeeOther)
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
