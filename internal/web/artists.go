package web

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/crypt0rr/artist-tracker/internal/catalog"
	"github.com/crypt0rr/artist-tracker/internal/store"
)

func (a *App) artists(w http.ResponseWriter, r *http.Request) {
	d := a.data(r, "Artists")
	pageFailed := a.loadArtistsData(r, &d)
	status := http.StatusOK
	if pageFailed {
		status = http.StatusInternalServerError
	}
	a.render(w, "artists", d, status)
}
func (a *App) syncArtist(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	if !a.allowProviderAction(w, r) {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	following, err := a.store.IsFollowing(r.Context(), session.User.ID, id)
	if err != nil {
		a.logger.Error("artist follow lookup failed", "page", "Artists", "path", r.URL.Path, "user_id", session.User.ID, "artist_id", id, "error", err)
		http.Error(w, "could not load this artist", http.StatusInternalServerError)
		return
	}
	if !following {
		http.NotFound(w, r)
		return
	}
	if _, err := a.store.CreateManualSyncRequest(r.Context(), session.User.ID, "artist", &id); err != nil {
		http.Redirect(w, r, "/artists?"+a.statusQuery(safeActionMessage(err, "Synchronization could not be queued. Please try again.")), http.StatusSeeOther)
		return
	}
	if a.jobs != nil {
		a.jobs.Wake()
	}
	http.Redirect(w, r, "/artists?"+a.statusQuery("Synchronization queued"), http.StatusSeeOther)
}

func parseFollowArtistID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		return 0, errors.New("invalid artist")
	}
	return id, nil
}

func followRuleFromForm(r *http.Request, rule store.FollowNotificationRule) store.FollowNotificationRule {
	rule.DeliveryMode = strings.TrimSpace(r.FormValue("delivery_mode"))
	rule.IncludePrimary = r.FormValue("include_primary") == "on"
	rule.IncludeFeatured = r.FormValue("include_featured") == "on"
	rule.Albums = r.FormValue("albums") == "on"
	rule.EPs = r.FormValue("eps") == "on"
	rule.Singles = r.FormValue("singles") == "on"
	rule.Compilations = r.FormValue("compilations") == "on"
	rule.Announcements = r.FormValue("announcements") == "on"
	rule.ReleaseDay = r.FormValue("release_day") == "on"
	return rule
}

func (a *App) updateArtistNotificationRule(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := parseFollowArtistID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rule, err := a.store.FollowNotificationRule(r.Context(), session.User.ID, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		a.logger.Error("load artist notification rule failed", "user_id", session.User.ID, "artist_id", id, "error", err)
		http.Error(w, "artist notification rule could not be loaded", http.StatusInternalServerError)
		return
	}
	rule = followRuleFromForm(r, rule)
	if err := a.store.UpdateFollowNotificationRule(r.Context(), session.User.ID, id, rule); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/artists?"+a.statusQuery(safeActionMessage(err, "The notification rule could not be saved. Please try again.")), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/artists?"+a.statusQuery("Notification rule updated"), http.StatusSeeOther)
}

func (a *App) updateArtistNotificationRuleBatch(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	seen := make(map[int64]struct{})
	var artistIDs []int64
	for _, raw := range r.Form["artist_ids"] {
		id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || id < 1 {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		artistIDs = append(artistIDs, id)
	}
	if len(artistIDs) == 0 {
		http.Redirect(w, r, "/artists?"+a.statusQuery("Select at least one artist"), http.StatusSeeOther)
		return
	}
	if len(artistIDs) > 50 {
		http.Redirect(w, r, "/artists?"+a.statusQuery("Select no more than 50 artists"), http.StatusSeeOther)
		return
	}
	changed, err := a.store.SetFollowNotificationDeliveryMode(r.Context(), session.User.ID, artistIDs, r.FormValue("delivery_mode"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/artists?"+a.statusQuery(safeActionMessage(err, "The notification mode could not be saved. Please try again.")), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/artists?"+a.statusQuery(fmt.Sprintf("Notification mode updated for %d artists", changed)), http.StatusSeeOther)
}

func (a *App) pauseArtistNotifications(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := parseFollowArtistID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	days := 7
	if value, parseErr := strconv.Atoi(r.FormValue("days")); parseErr == nil && value >= 1 && value <= 365 {
		days = value
	}
	until := time.Now().UTC().AddDate(0, 0, days)
	if err := a.store.PauseFollowNotificationRule(r.Context(), session.User.ID, id, &until); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/artists?"+a.statusQuery(safeActionMessage(err, "Notifications could not be paused. Please try again.")), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/artists?"+a.statusQuery(fmt.Sprintf("Notifications paused for %d days", days)), http.StatusSeeOther)
}

func (a *App) resumeArtistNotifications(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := parseFollowArtistID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := a.store.PauseFollowNotificationRule(r.Context(), session.User.ID, id, nil); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/artists?"+a.statusQuery(safeActionMessage(err, "Notifications could not be resumed. Please try again.")), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/artists?"+a.statusQuery("Notifications resumed"), http.StatusSeeOther)
}
func (a *App) search(w http.ResponseWriter, r *http.Request) {
	target := "/artists"
	if query := strings.TrimSpace(r.URL.Query().Get("q")); query != "" {
		target += "?q=" + url.QueryEscape(query)
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// populateSearch keeps provider discovery in one place now that the Artists
// page is the single entry point for both discovery and the followed list.
func (a *App) populateSearch(ctx context.Context, d *PageData) {
	if d.Query == "" {
		return
	}
	if a.spotify != nil {
		results, err := a.spotify.SearchArtists(ctx, d.Query)
		if err == nil && len(results) > 0 {
			d.SpotifyResults = results
			return
		}
		if err != nil {
			a.logger.Warn("Spotify artist search failed", "query_length", utf8.RuneCountInString(d.Query), "error", "provider request failed")
			if a.itunes == nil {
				d.ProviderNotice = "Spotify is temporarily unavailable; showing MusicBrainz results."
			} else {
				d.ProviderNotice = "Spotify is unavailable; trying Apple/iTunes discovery."
			}
		} else if a.itunes == nil {
			d.ProviderNotice = "No Spotify matches were found; showing MusicBrainz results."
		} else {
			d.ProviderNotice = "No Spotify matches were found; trying Apple/iTunes discovery."
		}
	}
	if a.itunes != nil {
		results, err := a.itunes.SearchArtists(ctx, d.Query)
		if err == nil && len(results) > 0 {
			d.ITunesResults = results
			return
		}
		if err != nil {
			a.logger.Warn("iTunes artist search failed", "query_length", utf8.RuneCountInString(d.Query), "error", "provider request failed")
			d.ProviderNotice = "Spotify and Apple/iTunes discovery are unavailable; showing MusicBrainz results."
		} else {
			d.ProviderNotice = "No Spotify or Apple/iTunes matches were found; showing MusicBrainz results."
		}
	}
	results, err := a.mb.SearchArtists(ctx, d.Query, 10)
	if err != nil {
		a.logger.Warn("artist search failed", "query_length", utf8.RuneCountInString(d.Query), "error", "provider request failed")
		d.Error = "MusicBrainz is temporarily unavailable. Please try your search again in a moment."
	} else {
		d.Results = results
	}
}
func (a *App) followITunes(w http.ResponseWriter, r *http.Request) {
	if !a.allowProviderAction(w, r) {
		return
	}
	session, _ := currentSession(r)
	if a.itunes == nil {
		http.Error(w, "iTunes is unavailable", http.StatusBadRequest)
		return
	}
	itunesID := strings.TrimSpace(r.FormValue("itunes_id"))
	if !validProviderID(itunesID) {
		http.Error(w, "invalid iTunes artist ID", http.StatusBadRequest)
		return
	}
	artist, err := a.itunes.Artist(r.Context(), itunesID)
	if err != nil {
		a.logger.Warn("iTunes artist lookup failed", "itunes_id", itunesID, "error", err)
		http.Error(w, "iTunes artist could not be verified", http.StatusBadGateway)
		return
	}
	_, created, err := a.store.CreateArtistResolution(
		r.Context(), session.User.ID, "itunes", artist.ID, artist.Name, artist.URL, "",
	)
	if err != nil {
		http.Error(w, "artist could not be queued", http.StatusInternalServerError)
		return
	}
	if created && a.jobs != nil {
		a.jobs.Wake()
	}
	message := "Artist queued for identification"
	if !created {
		message = "Artist is already queued"
	}
	http.Redirect(w, r, "/?"+a.statusQuery(message), http.StatusSeeOther)
}
func (a *App) followITunesBatch(w http.ResponseWriter, r *http.Request) {
	if !a.allowProviderAction(w, r) {
		return
	}
	if a.itunes == nil {
		http.Error(w, "iTunes is unavailable", http.StatusBadRequest)
		return
	}
	session, _ := currentSession(r)
	values, err := selectedValues(r, "itunes_ids")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var queued, existing, failed int
	for _, value := range values {
		if !validProviderID(value) {
			failed++
			continue
		}
		artist, lookupErr := a.itunes.Artist(r.Context(), value)
		if lookupErr != nil {
			failed++
			continue
		}
		_, created, createErr := a.store.CreateArtistResolution(
			r.Context(), session.User.ID, "itunes", artist.ID, artist.Name, artist.URL, "",
		)
		if createErr != nil {
			failed++
			continue
		}
		if created {
			queued++
		} else {
			existing++
		}
	}
	if queued > 0 && a.jobs != nil {
		a.jobs.Wake()
	}
	message := fmt.Sprintf("%d queued, %d already queued, %d failed", queued, existing, failed)
	http.Redirect(w, r, "/?"+a.statusQuery(message), http.StatusSeeOther)
}
func validProviderID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
func (a *App) followSpotify(w http.ResponseWriter, r *http.Request) {
	if !a.allowProviderAction(w, r) {
		return
	}
	session, _ := currentSession(r)
	if a.spotify == nil {
		http.Error(w, "Spotify is not configured", http.StatusBadRequest)
		return
	}
	spotifyID, ok := catalog.SpotifyID(r.FormValue("spotify_id"))
	if !ok {
		http.Error(w, "invalid Spotify artist ID", http.StatusBadRequest)
		return
	}
	spotifyArtist, err := a.spotify.Artist(r.Context(), spotifyID)
	if err != nil {
		a.logger.Warn("Spotify artist lookup failed", "spotify_id", spotifyID, "error", err)
		http.Error(w, "Spotify artist could not be verified", http.StatusBadGateway)
		return
	}
	_, created, err := a.store.CreateArtistResolution(
		r.Context(), session.User.ID, "spotify", spotifyArtist.ID, spotifyArtist.Name, spotifyArtist.URL, spotifyArtist.ImageURL,
	)
	if err != nil {
		http.Error(w, "artist could not be queued", http.StatusInternalServerError)
		return
	}
	if created && a.jobs != nil {
		a.jobs.Wake()
	}
	message := "Artist queued for identification"
	if !created {
		message = "Artist is already queued"
	}
	http.Redirect(w, r, "/?"+a.statusQuery(message), http.StatusSeeOther)
}
func (a *App) followSpotifyBatch(w http.ResponseWriter, r *http.Request) {
	if !a.allowProviderAction(w, r) {
		return
	}
	if a.spotify == nil {
		http.Error(w, "Spotify is not configured", http.StatusBadRequest)
		return
	}
	session, _ := currentSession(r)
	values, err := selectedValues(r, "spotify_ids")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var queued, existing, failed int
	var spotifyByID map[string]catalog.SpotifyArtist
	batchLookupFailed := false
	if batchProvider, ok := a.spotify.(catalog.SpotifyBatchArtistProvider); ok {
		ids := make([]string, 0, len(values))
		seen := make(map[string]bool, len(values))
		for _, value := range values {
			spotifyID, valid := catalog.SpotifyID(value)
			if valid && !seen[spotifyID] {
				seen[spotifyID] = true
				ids = append(ids, spotifyID)
			}
		}
		if len(ids) > 0 {
			spotifyByID = make(map[string]catalog.SpotifyArtist, len(ids))
			artists, lookupErr := batchProvider.Artists(r.Context(), ids)
			if lookupErr != nil {
				batchLookupFailed = true
				a.logger.Warn("Spotify batch artist lookup failed", "count", len(ids), "error", lookupErr)
			} else {
				for _, artist := range artists {
					spotifyByID[artist.ID] = artist
				}
			}
		}
	}
	for _, value := range values {
		spotifyID, ok := catalog.SpotifyID(value)
		if !ok {
			failed++
			continue
		}
		var spotifyArtist catalog.SpotifyArtist
		if spotifyByID != nil {
			var found bool
			spotifyArtist, found = spotifyByID[spotifyID]
			if batchLookupFailed || !found {
				failed++
				continue
			}
		} else {
			var lookupErr error
			spotifyArtist, lookupErr = a.spotify.Artist(r.Context(), spotifyID)
			if lookupErr != nil {
				a.logger.Warn("Spotify batch artist lookup failed", "spotify_id", spotifyID, "error", lookupErr)
				failed++
				continue
			}
		}
		_, created, createErr := a.store.CreateArtistResolution(
			r.Context(), session.User.ID, "spotify", spotifyArtist.ID, spotifyArtist.Name, spotifyArtist.URL, spotifyArtist.ImageURL,
		)
		if createErr != nil {
			failed++
			continue
		}
		if created {
			queued++
		} else {
			existing++
		}
	}
	if queued > 0 && a.jobs != nil {
		a.jobs.Wake()
	}
	message := fmt.Sprintf("%d queued, %d already queued, %d failed", queued, existing, failed)
	http.Redirect(w, r, "/?"+a.statusQuery(message), http.StatusSeeOther)
}
func (a *App) follow(w http.ResponseWriter, r *http.Request) {
	if !a.allowProviderAction(w, r) {
		return
	}
	session, _ := currentSession(r)
	result, err := a.mb.ResolveArtist(r.Context(), r.FormValue("mbid"))
	if err != nil {
		http.Error(w, "artist could not be resolved", http.StatusBadRequest)
		return
	}
	if a.spotify != nil {
		candidates, _ := a.spotify.SearchArtists(r.Context(), result.Name)
		results := []catalog.ArtistResult{result}
		catalog.Enrich(results, candidates)
		result = results[0]
	}
	artist, err := a.store.UpsertArtist(r.Context(), result.StoreArtist())
	var added bool
	if err == nil {
		added, err = a.store.Follow(r.Context(), session.User.ID, artist.ID)
	}
	if err != nil {
		http.Error(w, "could not follow artist", http.StatusInternalServerError)
		return
	}
	if added && a.jobs != nil {
		a.jobs.Wake()
	}
	message := "Artist added"
	if !added {
		message = "Artist already followed"
	}
	http.Redirect(w, r, "/?"+a.statusQuery(message), http.StatusSeeOther)
}
func (a *App) followBatch(w http.ResponseWriter, r *http.Request) {
	if !a.allowProviderAction(w, r) {
		return
	}
	session, _ := currentSession(r)
	values, err := selectedValues(r, "mbids")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var added, existing, failed int
	for _, mbid := range values {
		result, resolveErr := a.mb.ResolveArtist(r.Context(), mbid)
		if resolveErr != nil {
			failed++
			continue
		}
		if a.spotify != nil {
			candidates, _ := a.spotify.SearchArtists(r.Context(), result.Name)
			results := []catalog.ArtistResult{result}
			catalog.Enrich(results, candidates)
			result = results[0]
		}
		artist, storeErr := a.store.UpsertArtist(r.Context(), result.StoreArtist())
		if storeErr != nil {
			failed++
			continue
		}
		created, followErr := a.store.Follow(r.Context(), session.User.ID, artist.ID)
		if followErr != nil {
			failed++
			continue
		}
		if created {
			added++
		} else {
			existing++
		}
	}
	if added > 0 && a.jobs != nil {
		a.jobs.Wake()
	}
	message := fmt.Sprintf("%d added, %d already followed, %d failed", added, existing, failed)
	http.Redirect(w, r, "/?"+a.statusQuery(message), http.StatusSeeOther)
}
func selectedValues(r *http.Request, name string) ([]string, error) {
	seen := make(map[string]bool)
	var result []string
	for _, raw := range r.Form[name] {
		value := strings.TrimSpace(raw)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	if len(result) == 0 {
		return nil, errors.New("select at least one artist")
	}
	if len(result) > 10 {
		return nil, errors.New("select no more than 10 artists")
	}
	return result, nil
}
func (a *App) unfollow(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	if err := a.store.Unfollow(r.Context(), session.User.ID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		a.logger.Error("unfollow artist failed", "user_id", session.User.ID, "artist_id", id, "error", err)
		http.Error(w, "artist could not be removed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/artists?"+a.statusQuery("Artist removed"), http.StatusSeeOther)
}
func (a *App) artistResolution(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	resolution, err := a.store.ArtistResolution(r.Context(), session.User.ID, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		d := a.data(r, "Review artist")
		a.pageStoreError(r, &d, "Review artist", "artist resolution", err)
		a.render(w, "resolution", d, http.StatusInternalServerError)
		return
	}
	d := a.data(r, "Review artist")
	d.Resolution = &resolution
	a.render(w, "resolution", d, http.StatusOK)
}
func (a *App) selectArtistResolution(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	resolution, err := a.store.ArtistResolution(r.Context(), session.User.ID, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		a.logger.Error("artist resolution lookup failed", "page", "Review artist", "path", r.URL.Path, "resolution_id", id, "error", err)
		http.Error(w, "could not load this resolution", http.StatusInternalServerError)
		return
	}
	var selected *store.ResolutionCandidate
	for i := range resolution.Candidates {
		if resolution.Candidates[i].MBID == r.FormValue("mbid") {
			selected = &resolution.Candidates[i]
			break
		}
	}
	if resolution.Status != "review" || selected == nil {
		http.Error(w, "select one of the reviewed MusicBrainz artists", http.StatusBadRequest)
		return
	}
	if _, err := a.jobs.QueueSelectedArtistResolution(r.Context(), resolution, *selected); err != nil {
		http.Error(w, "artist could not be followed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/?"+a.statusQuery("Artist followed"), http.StatusSeeOther)
}
func (a *App) cancelArtistResolution(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := a.store.CancelArtistResolution(r.Context(), session.User.ID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		a.logger.Error("cancel artist resolution failed", "path", r.URL.Path, "user_id", session.User.ID, "resolution_id", id, "error", err)
		http.Error(w, "could not cancel this resolution", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/?"+a.statusQuery("Pending artist cancelled"), http.StatusSeeOther)
}
func (a *App) exportArtists(w http.ResponseWriter, r *http.Request) {
	session, _ := currentSession(r)
	filename := "artist-trackarr-watched-artists-" + time.Now().UTC().Format("2006-01-02") + ".csv"
	payload, err := buildBufferedCSV(func(writer *csv.Writer) error {
		if err := writer.Write([]string{
			"artist", "display_name", "sort_name", "artist_type", "country", "disambiguation",
			"musicbrainz_id", "musicbrainz_url", "spotify_id", "spotify_url", "spotify_image_url",
		}); err != nil {
			return err
		}
		const pageSize = 500
		var cursor *store.ArtistExportCursor
		for {
			artists, next, err := a.store.FollowedArtistsExportPage(r.Context(), session.User.ID, pageSize, cursor)
			if err != nil {
				return fmt.Errorf("artist export page lookup: %w", err)
			}
			for _, artist := range artists {
				musicBrainzURL := "https://musicbrainz.org/artist/" + artist.MBID
				if err := writer.Write(neutralizeCSVRow([]string{
					musicBrainzURL, artist.Name, artist.SortName, artist.Type, artist.Country,
					artist.Disambiguation, artist.MBID, musicBrainzURL, artist.SpotifyID,
					artist.SpotifyURL, artist.SpotifyImageURL,
				})); err != nil {
					return err
				}
			}
			if len(artists) < pageSize || next == nil {
				break
			}
			cursor = next
		}
		return nil
	})
	if err != nil {
		a.logger.Error("artist export failed", "page", "Artists", "path", r.URL.Path, "user_id", session.User.ID, "error", err)
		http.Error(w, "could not export followed artists", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	if _, err := w.Write(payload); err != nil {
		a.logger.Debug("artist export response interrupted", "user_id", session.User.ID, "error", err)
	}
}

// neutralizeCSVCell prevents spreadsheet applications from interpreting
// provider-controlled values as formulas when a household backup is opened.
// The leading apostrophe is part of the exported value and is understood by
// common spreadsheet programs as an explicit text marker.
func neutralizeCSVCell(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n")
	if trimmed == "" {
		return value
	}
	switch trimmed[0] {
	case '=', '+', '-', '@':
		return "'" + value
	default:
		return value
	}
}

func neutralizeCSVRow(row []string) []string {
	result := make([]string, len(row))
	for index, value := range row {
		result[index] = neutralizeCSVCell(value)
	}
	return result
}
