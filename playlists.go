package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hibooboo2/gradio/db"
	"github.com/hibooboo2/gradio/models"
	"github.com/hibooboo2/gradio/views"
	"github.com/jackc/pgx/v5"
)

// derivedSongTitle produces the default human-friendly label for a split's
// output file, e.g. "Slotex · gradio-2026-08-19_09-47-01 #3".
func derivedSongTitle(s models.Split) string {
	base := strings.TrimSuffix(filepath.Base(s.SourcePath), filepath.Ext(s.SourcePath))
	radio := views.RadioFromPath(context.Background(), s.SourcePath)
	return fmt.Sprintf("%s · %s #%d", radio, base, s.Index+1)
}

// songTitle produces the display label for a split. A user-set custom title
// (a display-only rename) wins; otherwise the title is derived from the source
// file name and stream position.
func songTitle(s models.Split) string {
	if s.CustomTitle != "" {
		return s.CustomTitle
	}
	return derivedSongTitle(s)
}

// musicURL returns the web URL that serves a split's output file, based on the
// output_path stored on the split row.
func musicURL(outputPath string) string {
	rel := strings.TrimPrefix(filepath.ToSlash(outputPath), "split_music/")
	return "/music/" + rel
}

// radioQueueSize is how many random splits are loaded into a radio's queue.
const radioQueueSize = 100

// shuffleBatchSize is how many splits are loaded per batch of the global
// shuffle. When a batch finishes playing, the client fetches the next batch of
// songs that have not been played yet in that session.
const shuffleBatchSize = 5

// parseSplitIDs parses a comma-separated list of split ids, ignoring any
// non-numeric entries.
func parseSplitIDs(raw string) []int64 {
	var ids []int64
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if id, err := strconv.ParseInt(p, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// renderPlaylistsView executes the play lists fragment into w. The playlist
// with id expand (if any) has its songs loaded and rendered inline.
func renderPlaylistsView(w http.ResponseWriter, r *http.Request, expand int64) {
	playlists, err := db.FetchAllPlaylists(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "list playlists", "err", err)
		http.Error(w, "failed to load playlists", http.StatusInternalServerError)
		return
	}

	items := make([]models.PlaylistViewItem, 0, len(playlists))
	var expandedSongs []models.PlaylistSong
	for _, p := range playlists {
		item := models.PlaylistViewItem{Playlist: p}
		if p.ID == expand {
			songs, err := db.FetchPlaylistSongs(r.Context(), p.ID)
			if err != nil {
				slog.ErrorContext(r.Context(), "list playlist songs", "err", err, "playlist", p.ID)
				http.Error(w, "failed to load playlist songs", http.StatusInternalServerError)
				return
			}
			expandedSongs = songs
			item.Songs = songs
		}
		items = append(items, item)
	}

	allSongs, err := db.FetchAllSongs(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "list songs", "err", err)
		http.Error(w, "failed to load songs", http.StatusInternalServerError)
		return
	}

	_ = expandedSongs
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.PlaylistsView(models.PlaylistsViewData{
		Playlists: items,
		AllSongs:  allSongs,
		Expanded:  expand,
	}).Render(r.Context(), w); err != nil {
		slog.ErrorContext(r.Context(), "render playlists view", "err", err)
	}
}

// handlePlaylistsView renders the play lists tab fragment. An optional
// ?expand=<id> query param expands that playlist to show its songs.
func handlePlaylistsView(w http.ResponseWriter, r *http.Request) {
	var expand int64
	if v := r.URL.Query().Get("expand"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			expand = id
		}
	}
	renderPlaylistsView(w, r, expand)
}

// handleCreatePlaylist creates a playlist from the submitted name and re-renders
// the play lists fragment, expanding the new playlist.
func handleCreatePlaylist(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "playlist name is required", http.StatusBadRequest)
		return
	}

	p, err := db.CreatePlaylist(r.Context(), name)
	if err != nil {
		slog.ErrorContext(r.Context(), "create playlist", "err", err)
		http.Error(w, "failed to create playlist", http.StatusInternalServerError)
		return
	}

	renderPlaylistsView(w, r, p.ID)
}

// handleDeletePlaylist deletes a playlist and re-renders the play lists fragment.
func handleDeletePlaylist(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid playlist id", http.StatusBadRequest)
		return
	}

	if err := db.DeletePlaylist(r.Context(), id); err != nil {
		slog.ErrorContext(r.Context(), "delete playlist", "err", err, "id", id)
		http.Error(w, "failed to delete playlist", http.StatusInternalServerError)
		return
	}

	renderPlaylistsView(w, r, 0)
}

// handleAddSong adds a split to a playlist from the submitted split_id and
// re-renders the play lists fragment with the playlist expanded.
func handleAddSong(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid playlist id", http.StatusBadRequest)
		return
	}

	splitID, err := strconv.ParseInt(r.FormValue("split_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid split id", http.StatusBadRequest)
		return
	}

	if err := db.AddSongToPlaylist(r.Context(), id, splitID); err != nil {
		slog.ErrorContext(r.Context(), "add song to playlist", "err", err, "playlist", id, "split", splitID)
		http.Error(w, "failed to add song", http.StatusInternalServerError)
		return
	}

	renderPlaylistsView(w, r, id)
}

// handleRemoveSong removes a split from a playlist and re-renders the play
// lists fragment with the playlist expanded.
func handleRemoveSong(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid playlist id", http.StatusBadRequest)
		return
	}

	splitID, err := strconv.ParseInt(r.PathValue("split_id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid split id", http.StatusBadRequest)
		return
	}

	if err := db.RemoveSongFromPlaylist(r.Context(), id, splitID); err != nil {
		slog.ErrorContext(r.Context(), "remove song from playlist", "err", err, "playlist", id, "split", splitID)
		http.Error(w, "failed to remove song", http.StatusInternalServerError)
		return
	}

	renderPlaylistsView(w, r, id)
}

// handlePlayerView renders the player tab fragment. It supports four modes:
//
//   - ?shuffle=1: a global shuffle of every song, least played first; an
//     optional ?exclude=<id,id,...> skips splits already played this session
//   - ?radio=<name>: play a queue of random splits from that radio (a hashed
//     recordings dir is accepted and resolved to the display name)
//   - ?playlist=<id>: play a saved playlist, with optional ?song=<split id>
//   - no params: the empty state, which lists available radios to start
func handlePlayerView(w http.ResponseWriter, r *http.Request) {
	if shuffle := r.URL.Query().Get("shuffle"); shuffle != "" {
		var exclude []int64
		if ex := r.URL.Query().Get("exclude"); ex != "" {
			exclude = parseSplitIDs(ex)
		}

		splits, err := db.FetchGlobalShuffleBatch(r.Context(), shuffleBatchSize, exclude)
		if err != nil {
			slog.ErrorContext(r.Context(), "load global shuffle", "err", err)
			http.Error(w, "failed to load shuffle", http.StatusInternalServerError)
			return
		}

		songs := make([]models.PlaylistSong, 0, len(splits))
		for i, s := range splits {
			songs = append(songs, models.PlaylistSong{
				SplitID:  s.ID,
				Position: i,
				Split:    s,
				Plays:    s.Plays,
				Rating:   s.Rating,
			})
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := views.PlayerView(models.PlayerViewData{
			Playlist: models.Playlist{Name: "All Music"},
			Songs:    songs,
			Subtitle: "Global Shuffle",
			QueueKey: "shuffle",
		}).Render(r.Context(), w); err != nil {
			slog.ErrorContext(r.Context(), "render player view", "err", err)
		}
		return
	}

	if radio := r.URL.Query().Get("radio"); radio != "" {
		// Resolve a stale ?radio=<hash> link back to the display name so the
		// playlist name, subtitle, queue key and split lookup all use the
		// station name. Plain names pass through unchanged.
		radio = db.RadioDisplayName(r.Context(), radio)

		splits, err := db.FetchRadioSplits(r.Context(), radio, radioQueueSize)
		if err != nil {
			slog.ErrorContext(r.Context(), "load radio splits", "err", err, "radio", radio)
			http.Error(w, "failed to load radio", http.StatusInternalServerError)
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
			Playlist: models.Playlist{Name: radio},
			Songs:    songs,
			Subtitle: "Radio · " + radio,
			QueueKey: "radio:" + radio,
		}).Render(r.Context(), w); err != nil {
			slog.ErrorContext(r.Context(), "render player view", "err", err)
		}
		return
	}

	playlistID, err := strconv.ParseInt(r.URL.Query().Get("playlist"), 10, 64)
	if err != nil || playlistID == 0 {
		radios, rerr := db.FetchRadios(r.Context())
		if rerr != nil {
			slog.ErrorContext(r.Context(), "list radios", "err", rerr)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := views.PlayerEmpty(models.PlayerEmptyData{Radios: radios}).Render(r.Context(), w); err != nil {
			slog.ErrorContext(r.Context(), "render player empty", "err", err)
		}
		return
	}

	playlist, err := db.FetchPlaylist(r.Context(), playlistID)
	if err != nil {
		if err == pgx.ErrNoRows {
			radios, rerr := db.FetchRadios(r.Context())
			if rerr != nil {
				slog.ErrorContext(r.Context(), "list radios", "err", rerr)
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err := views.PlayerEmpty(models.PlayerEmptyData{Radios: radios}).Render(r.Context(), w); err != nil {
				slog.ErrorContext(r.Context(), "render player empty", "err", err)
			}
			return
		}
		slog.ErrorContext(r.Context(), "fetch playlist", "err", err, "playlist", playlistID)
		http.Error(w, "failed to load playlist", http.StatusInternalServerError)
		return
	}

	songs, err := db.FetchPlaylistSongs(r.Context(), playlistID)
	if err != nil {
		slog.ErrorContext(r.Context(), "list playlist songs", "err", err, "playlist", playlistID)
		http.Error(w, "failed to load playlist songs", http.StatusInternalServerError)
		return
	}

	var startSplit int64
	if v := r.URL.Query().Get("song"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			startSplit = id
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.PlayerView(models.PlayerViewData{
		Playlist:  playlist,
		Songs:     songs,
		StartSong: startSplit,
		QueueKey:  "playlist:" + strconv.FormatInt(playlistID, 10),
	}).Render(r.Context(), w); err != nil {
		slog.ErrorContext(r.Context(), "render player view", "err", err)
	}
}

// handleShuffleJSON returns the next batch of the global shuffle as JSON so
// the player can seamlessly continue after the current batch finishes. ?exclude
// lists split ids already played in the session; the batch is ordered least
// played first with a random tiebreak.
func handleShuffleJSON(w http.ResponseWriter, r *http.Request) {
	var exclude []int64
	if ex := r.URL.Query().Get("exclude"); ex != "" {
		exclude = parseSplitIDs(ex)
	}

	splits, err := db.FetchGlobalShuffleBatch(r.Context(), shuffleBatchSize, exclude)
	if err != nil {
		slog.ErrorContext(r.Context(), "global shuffle json", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	tracks := make([]models.ShuffleTrack, 0, len(splits))
	for _, s := range splits {
		tracks = append(tracks, models.ShuffleTrack{
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
	writeJSON(w, http.StatusOK, tracks)
}

// handleMusic serves a split output file from split_music/ for the player's
// <audio> element. The URL path is the output_path from the splits table with
// the leading split_music/ segment removed, so range requests and seeking work
// natively.
func handleMusic(w http.ResponseWriter, r *http.Request) {
	rel := filepath.FromSlash(r.PathValue("path"))
	if rel == "" || rel == "." {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}

	clean := filepath.Clean(rel)
	if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	full := filepath.Join("split_music", clean)
	info, err := os.Stat(full)
	if err != nil {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	if info.IsDir() {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}

	// Log the split behind the served output file at debug level so playback
	// from the /music/ endpoint can be correlated with the split row. The DB
	// stores output_path relative to the working directory, so strip any
	// leading separator before the lookup.
	lookupPath := strings.TrimPrefix(filepath.ToSlash(full), "/")
	if s, err := db.FetchSplitByOutputPath(r.Context(), lookupPath); err == nil {
		slog.DebugContext(r.Context(), "serve split music",
			"split", s.ID,
			"song", songTitle(s),
			"source", s.SourcePath,
		)
	} else {
		slog.DebugContext(r.Context(), "serve split music", "output", full)
	}

	w.Header().Set("Content-Type", "audio/mpeg")
	http.ServeFile(w, r, full)
}

// handleListPlaylistsJSON returns every playlist as JSON.
func handleListPlaylistsJSON(w http.ResponseWriter, r *http.Request) {
	playlists, err := db.FetchAllPlaylists(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "list playlists", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, playlists)
}

// handleGetPlaylistJSON returns a single playlist with its songs as JSON.
func handleGetPlaylistJSON(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid playlist id")
		return
	}

	playlist, err := db.FetchPlaylist(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "playlist not found")
		return
	}

	songs, err := db.FetchPlaylistSongs(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"playlist": playlist,
		"songs":    songs,
	})
}

// handleListSongsJSON returns every split available for playlists as JSON.
func handleListSongsJSON(w http.ResponseWriter, r *http.Request) {
	songs, err := db.FetchAllSongs(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "list songs", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, songs)
}

// handleListRadiosJSON returns the distinct radios with playable splits.
func handleListRadiosJSON(w http.ResponseWriter, r *http.Request) {
	radios, err := db.FetchRadios(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "list radios", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, radios)
}
