package main

import (
	"database/sql"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Playlist is one row in the playlists table: a user-created collection of
// split output files.
type Playlist struct {
	ID         int64
	Name       string
	CreatedAt  time.Time
	TrackCount int
}

// PlaylistSong is one row in the playlist_splits table: a split that has been
// added to a playlist, joined with the full split row for display and playback.
type PlaylistSong struct {
	PlaylistID int64
	SplitID    int64
	Position   int
	Split      Split
}

// createPlaylist inserts a new playlist and returns the created row.
func createPlaylist(name string) (Playlist, error) {
	if recordDB == nil {
		return Playlist{}, fmt.Errorf("nil db")
	}

	var p Playlist
	var createdAt string
	err := recordDB.QueryRow(
		`INSERT INTO playlists (name) VALUES ($1) RETURNING id, name, created_at`,
		name,
	).Scan(&p.ID, &p.Name, &createdAt)
	if err != nil {
		return Playlist{}, err
	}
	if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		p.CreatedAt = t
	}
	return p, nil
}

// deletePlaylist removes a playlist. Its songs are removed by the ON DELETE
// CASCADE foreign key.
func deletePlaylist(id int64) error {
	if recordDB == nil {
		return fmt.Errorf("nil db")
	}
	_, err := recordDB.Exec(`DELETE FROM playlists WHERE id = $1`, id)
	return err
}

// fetchPlaylist returns a single playlist by id, or an error when it does not
// exist.
func fetchPlaylist(id int64) (Playlist, error) {
	if recordDB == nil {
		return Playlist{}, fmt.Errorf("nil db")
	}

	var p Playlist
	var createdAt string
	err := recordDB.QueryRow(
		`SELECT id, name, created_at FROM playlists WHERE id = $1`,
		id,
	).Scan(&p.ID, &p.Name, &createdAt)
	if err != nil {
		return Playlist{}, err
	}
	if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		p.CreatedAt = t
	}
	return p, nil
}

// fetchAllPlaylists returns every playlist with its track count, ordered by
// name.
func fetchAllPlaylists() ([]Playlist, error) {
	if recordDB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := recordDB.Query(
		`SELECT p.id, p.name, p.created_at, COUNT(ps.id)
		 FROM playlists p
		 LEFT JOIN playlist_splits ps ON ps.playlist_id = p.id
		 GROUP BY p.id
		 ORDER BY p.name ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var playlists []Playlist
	for rows.Next() {
		var p Playlist
		var createdAt string
		if err := rows.Scan(&p.ID, &p.Name, &createdAt, &p.TrackCount); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
			p.CreatedAt = t
		}
		playlists = append(playlists, p)
	}
	return playlists, rows.Err()
}

// fetchPlaylistSongs returns the splits in a playlist in playlist order.
func fetchPlaylistSongs(playlistID int64) ([]PlaylistSong, error) {
	if recordDB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := recordDB.Query(
		`SELECT ps.playlist_id, ps.split_id, ps.position,
		        s.id, s.recording_id, s.source_path, s.position, s.start_seconds, s.end_seconds, s.output_path, s.classification
		 FROM playlist_splits ps
		 JOIN splits s ON s.id = ps.split_id
		 WHERE ps.playlist_id = $1
		 ORDER BY ps.position ASC`,
		playlistID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var songs []PlaylistSong
	for rows.Next() {
		var song PlaylistSong
		if err := rows.Scan(
			&song.PlaylistID, &song.SplitID, &song.Position,
			&song.Split.ID, &song.Split.RecordingID, &song.Split.SourcePath, &song.Split.Index,
			&song.Split.Start, &song.Split.End, &song.Split.OutputPath, &song.Split.Classification,
		); err != nil {
			return nil, err
		}
		songs = append(songs, song)
	}
	return songs, rows.Err()
}

// addSongToPlaylist appends a split to the end of a playlist. Adding the same
// split twice is a no-op.
func addSongToPlaylist(playlistID, splitID int64) error {
	if recordDB == nil {
		return fmt.Errorf("nil db")
	}

	_, err := recordDB.Exec(
		`INSERT INTO playlist_splits (playlist_id, split_id, position)
		 SELECT $1, $2, COALESCE(MAX(position) + 1, 0)
		 FROM playlist_splits WHERE playlist_id = $1
		 ON CONFLICT (playlist_id, split_id) DO NOTHING`,
		playlistID, splitID,
	)
	return err
}

// removeSongFromPlaylist removes a split from a playlist.
func removeSongFromPlaylist(playlistID, splitID int64) error {
	if recordDB == nil {
		return fmt.Errorf("nil db")
	}

	_, err := recordDB.Exec(
		`DELETE FROM playlist_splits WHERE playlist_id = $1 AND split_id = $2`,
		playlistID, splitID,
	)
	return err
}

// fetchAllSongs returns every split that can be added to a playlist, newest
// recording first.
func fetchAllSongs() ([]Split, error) {
	return fetchAllSplits()
}

// songTitle produces a human-friendly label for a split's output file, e.g.
// "Slotex · gradio-2026-08-19_09-47-01 #3".
func songTitle(s Split) string {
	base := strings.TrimSuffix(filepath.Base(s.SourcePath), filepath.Ext(s.SourcePath))
	radio := radioFromPath(s.SourcePath)
	return fmt.Sprintf("%s · %s #%d", radio, base, s.Index+1)
}

// musicURL returns the web URL that serves a split's output file, based on the
// output_path stored on the split row.
func musicURL(outputPath string) string {
	rel := strings.TrimPrefix(filepath.ToSlash(outputPath), "split_music/")
	return "/music/" + rel
}

// playlistsViewData is the data model for the play lists tab fragment.
type playlistsViewData struct {
	Playlists []playlistViewItem
	AllSongs  []Split
	Expanded  int64
}

// playlistViewItem pairs a playlist with the songs shown when it is expanded.
type playlistViewItem struct {
	Playlist
	Songs []PlaylistSong
}

// playerViewData is the data model for the player tab fragment.
type playerViewData struct {
	Playlist  Playlist
	Songs     []PlaylistSong
	StartSong int64
}

var viewFuncs = template.FuncMap{
	"musicURL":  musicURL,
	"songTitle": songTitle,
	"timeStr": func(seconds float64) string {
		if seconds < 0 {
			return "0:00"
		}
		total := int(seconds)
		m := total / 60
		s := total % 60
		return fmt.Sprintf("%d:%02d", m, s)
	},
}

var playlistsViewTemplate = template.Must(template.New("playlists").Funcs(viewFuncs).Parse(`
<div class="view-header">
	<h2>Playlists</h2>
	<p>{{len .Playlists}} playlist{{if ne (len .Playlists) 1}}s{{end}} &mdash; built from the mp3 files in your splits</p>
</div>

<form class="create-form" hx-post="/playlists/create" hx-target="#content" hx-swap="innerHTML">
	<input type="text" name="name" placeholder="New playlist name" required maxlength="200">
	<button type="submit">Create</button>
</form>

{{if .Playlists}}
<ul class="playlist-list">
	{{range .Playlists}}
	{{$pl := .}}
	<li class="surface playlist-item">
		<div class="playlist-head"
			hx-get="/playlists/view?expand={{.ID}}" hx-target="#content" hx-swap="innerHTML">
			<div class="playlist-icon">&#9835;</div>
			<div class="playlist-info">
				<span class="playlist-name">{{.Name}}</span>
				<span class="playlist-meta">{{.TrackCount}} track{{if ne .TrackCount 1}}s{{end}}</span>
			</div>
			<span class="playlist-chevron">{{if eq .ID $.Expanded}}&#9660;{{else}}&#9654;{{end}}</span>
		</div>

		{{if eq .ID $.Expanded}}
		<div class="playlist-detail">
			<div class="playlist-actions">
				<button class="btn-play"
					hx-get="/player/view?playlist={{.ID}}" hx-target="#content" hx-swap="innerHTML"
					hx-push-url="/player" hx-on:click="selectTab('player', false)">&#9654; Play</button>
				<button class="btn-danger"
					hx-post="/playlists/{{.ID}}/delete" hx-target="#content" hx-swap="innerHTML"
					hx-confirm="Delete playlist &quot;{{.Name}}&quot;? The mp3 files are kept.">&#128465; Delete</button>
			</div>

			<h3>Songs</h3>
			{{if .Songs}}
			<ul class="song-list">
				{{range .Songs}}
				<li>
					<a class="song-play" href="/player?playlist={{$pl.ID}}&song={{.Split.ID}}"
						title="Play"
						hx-get="/player/view?playlist={{$pl.ID}}&song={{.Split.ID}}"
						hx-target="#content" hx-swap="innerHTML" hx-push-url="/player"
						hx-on:click="selectTab('player', false)">&#9654;</a>
					<div class="song-info">
						<span class="song-title">{{songTitle .Split}}</span>
						<span class="song-sub">{{timeStr .Split.Start}} &ndash; {{timeStr .Split.End}} &middot; {{.Split.Classification}}</span>
					</div>
					<button class="btn-remove" title="Remove from playlist"
						hx-post="/playlists/{{$pl.ID}}/songs/{{.Split.ID}}/delete"
						hx-target="#content" hx-swap="innerHTML"
						hx-vals='{"expand": "{{$pl.ID}}"}'>&times;</button>
				</li>
				{{end}}
			</ul>
			{{else}}
			<p class="empty">No songs yet. Add some below.</p>
			{{end}}

			<form class="add-song-form" hx-post="/playlists/{{.ID}}/songs" hx-target="#content" hx-swap="innerHTML"
				hx-vals='{"expand": "{{.ID}}"}'>
				<select name="split_id" required>
					<option value="" disabled selected>Add an mp3 from the splits&hellip;</option>
					{{range $.AllSongs}}
					<option value="{{.ID}}">{{songTitle .}}</option>
					{{end}}
				</select>
				<button type="submit">Add</button>
			</form>
		</div>
		{{end}}
	</li>
	{{end}}
</ul>
{{else}}
<p class="empty">No playlists yet. Create one above using the mp3 files in your splits.</p>
{{end}}
`))

var playerViewTemplate = template.Must(template.New("player").Funcs(viewFuncs).Parse(`
<div class="player-wrap" data-player-wrap>
	<section class="surface player-card">
		<div class="player-cover" data-eq>
			<span></span><span></span><span></span>
		</div>
		<p class="player-title" data-player-title>Nothing playing</p>
		<p class="player-subtitle" data-player-subtitle>Playlist &middot; {{.Playlist.Name}}</p>

		<audio data-audio preload="metadata"></audio>

		<div class="progress-row">
			<span data-time>0:00</span>
			<div class="progress-bar" data-progress-bar>
				<div class="progress-fill" data-progress-fill></div>
			</div>
			<span data-duration>0:00</span>
		</div>

		<div class="transport">
			<button type="button" class="icon-btn" data-player-shuffle title="Shuffle">&#128256;</button>
			<button type="button" class="icon-btn" data-player-prev title="Previous">&#9198;&#65039;</button>
			<button type="button" class="play-btn" data-player-toggle title="Play/Pause">&#9654;&#65039;</button>
			<button type="button" class="icon-btn" data-player-next title="Next">&#9197;&#65039;</button>
			<button type="button" class="icon-btn" data-player-repeat title="Repeat">&#128257;&#65039;</button>
		</div>

		<div class="volume-row">
			<button type="button" class="icon-btn" data-player-mute title="Mute">&#128266;&#65039;</button>
			<input type="range" data-player-volume min="0" max="1" step="0.01">
		</div>
	</section>

	<aside class="surface queue">
		<div class="queue-header">
			<h2>Queue</h2>
			<span>{{len .Songs}} track{{if ne (len .Songs) 1}}s{{end}}</span>
		</div>
		{{if .Songs}}
		<ul data-player-queue>
			{{range .Songs}}
			<li class="queue-item {{if eq .Split.ID $.StartSong}}active{{end}}"
				data-player-track
				data-src="{{musicURL .Split.OutputPath}}"
				data-title="{{songTitle .Split}}"
				data-split="{{.Split.ID}}">
				<span class="queue-num">{{.Position | printf "%d"}}</span>
				<div class="queue-info">
					<span class="queue-title">{{songTitle .Split}}</span>
					<span class="queue-sub">{{timeStr .Split.Start}} &ndash; {{timeStr .Split.End}} &middot; {{.Split.Classification}}</span>
				</div>
			</li>
			{{end}}
		</ul>
		{{else}}
		<p class="empty">This playlist has no songs.</p>
		{{end}}
	</aside>
</div>
`))

var playerEmptyTemplate = template.Must(template.New("playerEmpty").Parse(`
<div class="player-empty surface">
	<p class="empty-icon">&#9835;</p>
	<p class="empty">Nothing is playing.</p>
	<p class="empty-sub">Pick a playlist from the Play Lists tab to start listening.</p>
	<button
		hx-get="/playlists/view" hx-target="#content" hx-swap="innerHTML"
		hx-push-url="/playlists" hx-on:click="selectTab('playlists', false)">Go to Play Lists</button>
</div>
`))

// renderPlaylistsView executes the play lists fragment into w. The playlist
// with id expand (if any) has its songs loaded and rendered inline.
func renderPlaylistsView(w http.ResponseWriter, r *http.Request, expand int64) {
	playlists, err := fetchAllPlaylists()
	if err != nil {
		slog.ErrorContext(r.Context(), "list playlists", "err", err)
		http.Error(w, "failed to load playlists", http.StatusInternalServerError)
		return
	}

	items := make([]playlistViewItem, 0, len(playlists))
	var expandedSongs []PlaylistSong
	for _, p := range playlists {
		item := playlistViewItem{Playlist: p}
		if p.ID == expand {
			songs, err := fetchPlaylistSongs(p.ID)
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

	allSongs, err := fetchAllSongs()
	if err != nil {
		slog.ErrorContext(r.Context(), "list songs", "err", err)
		http.Error(w, "failed to load songs", http.StatusInternalServerError)
		return
	}

	_ = expandedSongs
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := playlistsViewTemplate.Execute(w, playlistsViewData{
		Playlists: items,
		AllSongs:  allSongs,
		Expanded:  expand,
	}); err != nil {
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

	p, err := createPlaylist(name)
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

	if err := deletePlaylist(id); err != nil {
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

	if err := addSongToPlaylist(id, splitID); err != nil {
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

	if err := removeSongFromPlaylist(id, splitID); err != nil {
		slog.ErrorContext(r.Context(), "remove song from playlist", "err", err, "playlist", id, "split", splitID)
		http.Error(w, "failed to remove song", http.StatusInternalServerError)
		return
	}

	renderPlaylistsView(w, r, id)
}

// handlePlayerView renders the player tab fragment for the playlist named by
// the ?playlist=<id> query param. The optional ?song=<split id> query param
// selects which track starts playing.
func handlePlayerView(w http.ResponseWriter, r *http.Request) {
	playlistID, err := strconv.ParseInt(r.URL.Query().Get("playlist"), 10, 64)
	if err != nil || playlistID == 0 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := playerEmptyTemplate.Execute(w, nil); err != nil {
			slog.ErrorContext(r.Context(), "render player empty", "err", err)
		}
		return
	}

	playlist, err := fetchPlaylist(playlistID)
	if err != nil {
		if err == sql.ErrNoRows {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err := playerEmptyTemplate.Execute(w, nil); err != nil {
				slog.ErrorContext(r.Context(), "render player empty", "err", err)
			}
			return
		}
		slog.ErrorContext(r.Context(), "fetch playlist", "err", err, "playlist", playlistID)
		http.Error(w, "failed to load playlist", http.StatusInternalServerError)
		return
	}

	songs, err := fetchPlaylistSongs(playlistID)
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
	if err := playerViewTemplate.Execute(w, playerViewData{
		Playlist:  playlist,
		Songs:     songs,
		StartSong: startSplit,
	}); err != nil {
		slog.ErrorContext(r.Context(), "render player view", "err", err)
	}
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

	w.Header().Set("Content-Type", "audio/mpeg")
	http.ServeFile(w, r, full)
}

// handleListPlaylistsJSON returns every playlist as JSON.
func handleListPlaylistsJSON(w http.ResponseWriter, r *http.Request) {
	playlists, err := fetchAllPlaylists()
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

	playlist, err := fetchPlaylist(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "playlist not found")
		return
	}

	songs, err := fetchPlaylistSongs(id)
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
	songs, err := fetchAllSongs()
	if err != nil {
		slog.ErrorContext(r.Context(), "list songs", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, songs)
}
