package main

import (
	"database/sql"
	"log/slog"
	"net/http"
	"strings"

	"github.com/hibooboo2/gradio/db"
	"github.com/hibooboo2/gradio/models"
	"github.com/hibooboo2/gradio/views"
)

// handleStationsView renders the Radio Stations tab fragment listing every
// station in the radio_stations table with its current recording state.
func handleStationsView(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	stations, err := db.FetchRadioStations(r.Context(), q)
	if err != nil {
		slog.ErrorContext(r.Context(), "list radio stations", "err", err)
		http.Error(w, "failed to load radio stations", http.StatusInternalServerError)
		return
	}

	favs, err := db.FetchFavoriteUUIDs(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "list favorites", "err", err)
		http.Error(w, "failed to load favorites", http.StatusInternalServerError)
		return
	}

	rows := make([]models.StationViewRow, 0, len(stations))
	for _, s := range stations {
		_, faved := favs[s.StationUUID]
		domain, pos := recorderManager.QueueInfo(s.Name)
		rows = append(rows, models.StationViewRow{
			RadioStation: s,
			Recording:    recorderManager.IsRecording(s.Name),
			Favorited:    faved,
			Queued:       pos > 0,
			Domain:       domain,
			QueuePos:     pos,
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.StationsView(models.StationsViewData{Stations: rows, Query: q}).Render(r.Context(), w); err != nil {
		slog.ErrorContext(r.Context(), "render stations view", "err", err)
	}
}

// handleFavoritesView renders the Favorites tab fragment listing only the
// stations that have been favorited.
func handleFavoritesView(w http.ResponseWriter, r *http.Request) {
	stations, err := db.FetchFavoriteStations(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "list favorite stations", "err", err)
		http.Error(w, "failed to load favorite stations", http.StatusInternalServerError)
		return
	}

	rows := make([]models.StationViewRow, 0, len(stations))
	for _, s := range stations {
		domain, pos := recorderManager.QueueInfo(s.Name)
		rows = append(rows, models.StationViewRow{
			RadioStation: s,
			Recording:    recorderManager.IsRecording(s.Name),
			Favorited:    true,
			Queued:       pos > 0,
			Domain:       domain,
			QueuePos:     pos,
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.FavoritesView(models.StationsViewData{Stations: rows}).Render(r.Context(), w); err != nil {
		slog.ErrorContext(r.Context(), "render favorites view", "err", err)
	}
}

// handleToggleFavorite favorites or unfavorites a station. When the client
// asks for JSON (Accept: application/json or ?json=1) it returns a minimal
// payload so the JS favorite button can flip the star in place without
// re-rendering the whole list. Otherwise it re-renders the view the request
// came from (?view=stations or ?view=favorites) for non-JS fallback.
func handleToggleFavorite(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	if _, err := db.FetchRadioStationByUUID(r.Context(), uuid); err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "station not found", http.StatusNotFound)
			return
		}
		slog.ErrorContext(r.Context(), "fetch station", "err", err)
		http.Error(w, "failed to load station", http.StatusInternalServerError)
		return
	}

	favs, err := db.FetchFavoriteUUIDs(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "list favorites", "err", err)
		http.Error(w, "failed to load favorites", http.StatusInternalServerError)
		return
	}

	_, wasFavorited := favs[uuid]
	if wasFavorited {
		err = db.RemoveFavorite(r.Context(), uuid)
	} else {
		err = db.AddFavorite(r.Context(), uuid)
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "toggle favorite", "err", err, "uuid", uuid)
		http.Error(w, "failed to update favorite", http.StatusInternalServerError)
		return
	}

	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"favorited": !wasFavorited, "uuid": uuid})
		return
	}

	view := r.URL.Query().Get("view")
	if view == "favorites" {
		handleFavoritesView(w, r)
		return
	}
	handleStationsView(w, r)
}

// wantsJSON reports whether the request asks for a JSON response, either via
// the ?json=1 query parameter or an Accept: application/json header.
func wantsJSON(r *http.Request) bool {
	if r.URL.Query().Get("json") != "" {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// handleStationRecord starts recording the given station (if it is not already
// being recorded) and returns its queue so the client can start playback.
//
// When the client asks for JSON (Accept: application/json or ?json=1), the
// response is a models.StationRecordResponse with the queue to load into the
// persistent mini player — the current tab is never swapped or navigated away
// from. Otherwise it renders the player for the songs recorded so far, or a
// friendly message when the station has no recorded songs yet.
func handleStationRecord(w http.ResponseWriter, r *http.Request) {
	station, err := db.FetchRadioStationByUUID(r.Context(), r.PathValue("uuid"))
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "station not found", http.StatusNotFound)
			return
		}
		slog.ErrorContext(r.Context(), "fetch station", "err", err)
		http.Error(w, "failed to load station", http.StatusInternalServerError)
		return
	}

	if station.URLResolved == "" {
		http.Error(w, "station has no resolvable stream url", http.StatusBadRequest)
		return
	}

	res := recorderManager.Start(station.Name, station.URLResolved)

	// The station was queued behind another station on the same domain. Tell
	// the client without starting a recorder or loading a queue; the recorder
	// will start automatically when the active station finishes.
	if res.Queued {
		if wantsJSON(r) {
			writeJSON(w, http.StatusOK, models.StationRecordResponse{
				StationName:   station.Name,
				QueueKey:      "radio:" + station.Name,
				Source:        "Radio · " + station.Name,
				Tracks:        []models.ShuffleTrack{},
				Queued:        true,
				Domain:        res.Domain,
				QueuePosition: res.QueuePosition,
			})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := views.QueuedStation(station.Name, res.Domain, res.QueuePosition).Render(r.Context(), w); err != nil {
			slog.ErrorContext(r.Context(), "render queued station", "err", err)
		}
		return
	}

	splits, err := db.FetchRadioSplits(r.Context(), station.Name, radioQueueSize)
	if err != nil {
		slog.ErrorContext(r.Context(), "load radio splits", "err", err, "radio", station.Name)
		http.Error(w, "failed to load radio", http.StatusInternalServerError)
		return
	}

	// The JS Record & Play flow asks for JSON so it can load the queue into the
	// persistent mini player without navigating away from the current tab.
	if wantsJSON(r) {
		resp := models.StationRecordResponse{
			StationName: station.Name,
			QueueKey:    "radio:" + station.Name,
			Source:      "Radio · " + station.Name,
			Tracks:      make([]models.ShuffleTrack, 0, len(splits)),
		}
		for _, s := range splits {
			resp.Tracks = append(resp.Tracks, models.ShuffleTrack{
				ID:             s.ID,
				Title:          songTitle(s),
				DerivedTitle:   derivedSongTitle(s),
				CustomTitle:    s.CustomTitle,
				Src:            musicURL(s.OutputPath),
				Start:          s.Start,
				End:            s.End,
				Classification: s.Classification,
				Plays:          s.Plays,
				Rating:         s.Rating,
			})
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	if len(splits) == 0 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := views.StationNoSongs(station.Name).Render(r.Context(), w); err != nil {
			slog.ErrorContext(r.Context(), "render no songs", "err", err)
		}
		return
	}

	songs := make([]models.PlaylistSong, 0, len(splits))
	for i, s := range splits {
		songs = append(songs, models.PlaylistSong{
			SplitID:  s.ID,
			Position: i,
			Split:    s,
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.PlayerView(models.PlayerViewData{
		Playlist: models.Playlist{Name: station.Name},
		Songs:    songs,
		Subtitle: "Radio · " + station.Name,
		QueueKey: "radio:" + station.Name,
	}).Render(r.Context(), w); err != nil {
		slog.ErrorContext(r.Context(), "render player view", "err", err)
	}
}
