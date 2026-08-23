package main

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
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
	Plays      int
	Rating     int
}

// createPlaylist inserts a new playlist and returns the created row.
func createPlaylist(ctx context.Context, name string) (Playlist, error) {
	if recordDB == nil {
		return Playlist{}, fmt.Errorf("nil db")
	}

	var p Playlist
	var createdAt string
	err := recordDB.QueryRowContext(ctx, 
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
func deletePlaylist(ctx context.Context, id int64) error {
	if recordDB == nil {
		return fmt.Errorf("nil db")
	}
	_, err := recordDB.ExecContext(ctx, `DELETE FROM playlists WHERE id = $1`, id)
	return err
}

// fetchPlaylist returns a single playlist by id, or an error when it does not
// exist.
func fetchPlaylist(ctx context.Context, id int64) (Playlist, error) {
	if recordDB == nil {
		return Playlist{}, fmt.Errorf("nil db")
	}

	var p Playlist
	var createdAt string
	err := recordDB.QueryRowContext(ctx, 
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
func fetchAllPlaylists(ctx context.Context) ([]Playlist, error) {
	if recordDB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := recordDB.QueryContext(ctx, 
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
func fetchPlaylistSongs(ctx context.Context, playlistID int64) ([]PlaylistSong, error) {
	if recordDB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := recordDB.QueryContext(ctx, 
		`SELECT ps.playlist_id, ps.split_id, ps.position,
		        s.id, s.recording_id, s.source_path, s.position, s.start_seconds, s.end_seconds, s.output_path, s.classification, s.custom_title,
		        COALESCE(sp.plays, 0), COALESCE(sp.rating, 0)
		 FROM playlist_splits ps
		 JOIN splits s ON s.id = ps.split_id
		 LEFT JOIN song_plays sp ON sp.split_id = s.id
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
			&song.Split.Start, &song.Split.End, &song.Split.OutputPath, &song.Split.Classification, &song.Split.CustomTitle,
			&song.Plays, &song.Rating,
		); err != nil {
			return nil, err
		}
		songs = append(songs, song)
	}
	return songs, rows.Err()
}

// addSongToPlaylist appends a split to the end of a playlist. Adding the same
// split twice is a no-op.
func addSongToPlaylist(ctx context.Context, playlistID, splitID int64) error {
	if recordDB == nil {
		return fmt.Errorf("nil db")
	}

	_, err := recordDB.ExecContext(ctx, 
		`INSERT INTO playlist_splits (playlist_id, split_id, position)
		 SELECT $1, $2, COALESCE(MAX(position) + 1, 0)
		 FROM playlist_splits WHERE playlist_id = $1
		 ON CONFLICT (playlist_id, split_id) DO NOTHING`,
		playlistID, splitID,
	)
	return err
}

// removeSongFromPlaylist removes a split from a playlist.
func removeSongFromPlaylist(ctx context.Context, playlistID, splitID int64) error {
	if recordDB == nil {
		return fmt.Errorf("nil db")
	}

	_, err := recordDB.ExecContext(ctx, 
		`DELETE FROM playlist_splits WHERE playlist_id = $1 AND split_id = $2`,
		playlistID, splitID,
	)
	return err
}

// fetchAllSongs returns every split that can be added to a playlist, newest
// recording first.
func fetchAllSongs(ctx context.Context) ([]Split, error) {
	return fetchAllSplits(ctx)
}

// derivedSongTitle produces the default human-friendly label for a split's
// output file, e.g. "Slotex · gradio-2026-08-19_09-47-01 #3".
func derivedSongTitle(s Split) string {
	base := strings.TrimSuffix(filepath.Base(s.SourcePath), filepath.Ext(s.SourcePath))
	radio := radioFromPath(context.Background(), s.SourcePath)
	return fmt.Sprintf("%s · %s #%d", radio, base, s.Index+1)
}

// songTitle produces the display label for a split. A user-set custom title
// (a display-only rename) wins; otherwise the title is derived from the source
// file name and stream position.
func songTitle(s Split) string {
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
	// Subtitle overrides the default "Playlist · <name>" subtitle, e.g. for a
	// radio which shows "Radio · <name>".
	Subtitle string
	// QueueKey uniquely identifies the loaded queue (e.g. "radio:Slotex" or
	// "playlist:123") so the client can keep the currently playing queue when
	// the view is re-rendered instead of restarting it.
	QueueKey string
}

// radioQueueSize is how many random splits are loaded into a radio's queue.
const radioQueueSize = 100

// shuffleBatchSize is how many splits are loaded per batch of the global
// shuffle. When a batch finishes playing, the client fetches the next batch of
// songs that have not been played yet in that session.
const shuffleBatchSize = 5

var viewFuncs = template.FuncMap{
	"musicURL":              musicURL,
	"songTitle":             songTitle,
	"derivedSongTitle":      derivedSongTitle,
	"clsLabel":              clsLabel,
	"classificationOptions": func() []classificationOption { return classificationOptions },
	"urlq":                  url.QueryEscape,
	"timeStr": func(seconds float64) string {
		if seconds < 0 {
			return "0:00"
		}
		total := int(seconds)
		m := total / 60
		s := total % 60
		return fmt.Sprintf("%d:%02d", m, s)
	},
	"timeFmt": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Local().Format("2006-01-02 15:04:05")
	},
}

var playlistsViewTemplate = template.Must(template.New("playlists").Funcs(viewFuncs).Parse(`
<div class="view-header">
	<h2>Playlists</h2>
	<p>{{len .Playlists}} playlist{{if ne (len .Playlists) 1}}s{{end}} &mdash; built from the mp3 files in your splits</p>
</div>

<form class="create-form" hx-post="/playlists/create" hx-target="#content" hx-swap="innerHTML"
		hx-push-url="/playlists">
	<input type="text" name="name" placeholder="New playlist name" required maxlength="200">
	<button type="submit">Create</button>
</form>

{{if .Playlists}}
<ul class="playlist-list">
	{{range .Playlists}}
	{{$pl := .}}
	<li class="surface playlist-item">
		<div class="playlist-head"
			hx-get="/playlists/view?expand={{.ID}}" hx-target="#content" hx-swap="innerHTML"
			hx-push-url="/playlists?expand={{.ID}}">
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
					hx-push-url="/player?playlist={{.ID}}" hx-on:click="selectTab('player', false)">&#9654; Play</button>
				<button class="btn-danger"
					hx-post="/playlists/{{.ID}}/delete" hx-target="#content" hx-swap="innerHTML"
					hx-push-url="/playlists"
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
						hx-target="#content" hx-swap="innerHTML" hx-push-url="/player?playlist={{$pl.ID}}&song={{.Split.ID}}"
						hx-on:click="selectTab('player', false)">&#9654;</a>
					<div class="song-info">
						<span class="song-title">{{songTitle .Split}}</span>
						<span class="song-sub">{{timeStr .Split.Start}} &ndash; {{timeStr .Split.End}} &middot; {{clsLabel .Split.Classification}}</span>
					</div>
					<button class="btn-remove" title="Remove from playlist"
						hx-post="/playlists/{{$pl.ID}}/songs/{{.Split.ID}}/delete"
						hx-target="#content" hx-swap="innerHTML"
						hx-push-url="/playlists?expand={{$pl.ID}}"
						hx-vals='{"expand": "{{$pl.ID}}"}'>&times;</button>
				</li>
				{{end}}
			</ul>
			{{else}}
			<p class="empty">No songs yet. Add some below.</p>
			{{end}}

			<form class="add-song-form" hx-post="/playlists/{{.ID}}/songs" hx-target="#content" hx-swap="innerHTML"
				hx-push-url="/playlists?expand={{.ID}}"
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
<div class="player-wrap" data-player-wrap
	data-queue-key="{{.QueueKey}}"
	data-start-split="{{.StartSong}}">
	<section class="surface player-card">
		<div class="player-cover" data-eq>
			<span></span><span></span><span></span>
		</div>
		<p class="player-title" data-player-title>Nothing playing</p>
		<p class="player-subtitle" data-player-subtitle>{{if .Subtitle}}{{.Subtitle}}{{else}}Playlist &middot; {{.Playlist.Name}}{{end}}</p>

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

		<div class="rating-row">
			<button type="button" class="icon-btn" data-player-rate data-rating="like" title="Like this track">&#128077;</button>
			<button type="button" class="icon-btn" data-player-rate data-rating="dislike" title="Dislike this track">&#128078;</button>
		</div>

		<div class="sleep-row">
			<button type="button" class="icon-btn sleep-btn" data-player-sleep title="Sleep timer">&#127769;&#65039;</button>
			<span class="sleep-remaining" data-sleep-remaining></span>
			<div class="sleep-popup" data-sleep-popup hidden>
				<div class="sleep-popup-title">Sleep timer</div>
				<button type="button" data-sleep-min="5">5 min</button>
				<button type="button" data-sleep-min="10">10 min</button>
				<button type="button" data-sleep-min="15">15 min</button>
				<button type="button" data-sleep-min="30">30 min</button>
				<button type="button" data-sleep-min="60">60 min</button>
				<button type="button" class="sleep-off" data-sleep-off>Off</button>
			</div>
		</div>

		<div class="mark-row">
			<select data-classification-select disabled title="Classify the currently playing track">
				{{range classificationOptions}}
				<option value="{{.Value}}"{{if eq .Value "re_split"}} disabled{{end}}>{{.Label}}</option>
				{{end}}
			</select>
			<button type="button" data-player-merge-prev disabled title="This track started too soon: join the previous split on to it">&#9198;&#65039; Start Too Soon</button>
			<button type="button" data-player-merge-next disabled title="This track ended too soon: join the following split on to it">&#9197;&#65039; End Too Soon</button>
		</div>

		<p class="resplit-status" data-resplit-status hidden></p>

		<div class="shuffle-all-row">
			<button type="button" class="btn-play" data-player-shuffle-all
				title="Shuffle every song in your library, least played first">&#128257; Shuffle All Music</button>
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
				data-split="{{.Split.ID}}"
				data-start="{{.Split.Start}}"
				data-end="{{.Split.End}}"
				data-classification="{{.Split.Classification}}"
				data-derived-title="{{derivedSongTitle .Split}}"
				data-custom-title="{{.Split.CustomTitle}}">
				<span class="queue-num">{{.Position | printf "%d"}}</span>
				<div class="queue-info">
					<span class="queue-title">{{songTitle .Split}}</span>
					<span class="queue-sub">{{timeStr .Split.Start}} &ndash; {{timeStr .Split.End}} &middot; <span data-cls>{{clsLabel .Split.Classification}}</span>{{if .Plays}} &middot; &#9835; {{.Plays}} play{{if ne .Plays 1}}s{{end}}{{end}}</span>
				</div>
				<button type="button" class="title-edit" data-title-edit data-split="{{.Split.ID}}" title="Rename this track">&#9999;&#65039;</button>
			</li>
			{{end}}
		</ul>
		{{else}}
		<p class="empty">This playlist has no songs.</p>
		{{end}}
	</aside>
</div>
`))

var playerEmptyTemplate = template.Must(template.New("playerEmpty").Funcs(viewFuncs).Parse(`
<div class="player-empty surface">
	<p class="empty-icon">&#9835;</p>
	<p class="empty">Nothing is playing.</p>
	<p class="empty-sub">Pick a playlist from the Play Lists tab, start a radio below, or shuffle your whole library.</p>

	<button class="btn-play shuffle-all"
		hx-get="/player/view?shuffle=1" hx-target="#content" hx-swap="innerHTML"
		hx-push-url="/player?shuffle=1" hx-on:click="selectTab('player')"
		title="Shuffle every song in your library, least played first">&#128257; Shuffle All Music</button>

	{{if .Radios}}
	<h3 class="radio-section-title">Radios</h3>
	<ul class="radio-list">
		{{range .Radios}}
		<li class="radio-item">
			<button
				class="radio-play"
				hx-get="/player/view?radio={{urlq .Name}}" hx-target="#content" hx-swap="innerHTML"
				hx-push-url="/player?radio={{urlq .Name}}" hx-on:click="selectTab('player')"
				title="Play radio {{.Name}}">&#9654; Play {{.Name}}</button>
			<span class="radio-meta">{{.SplitCount}} split{{if ne .SplitCount 1}}s{{end}}</span>
		</li>
		{{end}}
	</ul>
	{{end}}

	<button class="go-playlists"
		hx-get="/playlists/view" hx-target="#content" hx-swap="innerHTML"
		hx-push-url="/playlists" hx-on:click="selectTab('playlists')">Go to Play Lists</button>
</div>
`))

// playerEmptyData is the data model for the player empty state.
type playerEmptyData struct {
	Radios []Radio
}

// shuffleTrack is one track in the global shuffle queue, as returned by the
// JSON continuation endpoint so the player can keep fetching fresh batches of
// least-played songs without a full view swap.
type shuffleTrack struct {
	ID             int64   `json:"id"`
	Title          string  `json:"title"`
	DerivedTitle   string  `json:"derived_title"`
	CustomTitle    string  `json:"custom_title"`
	Src            string  `json:"src"`
	Start          float64 `json:"start"`
	End            float64 `json:"end"`
	Classification string  `json:"classification"`
	Plays          int     `json:"plays"`
	Rating         int     `json:"rating"`
}

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
	playlists, err := fetchAllPlaylists(r.Context())
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
			songs, err := fetchPlaylistSongs(r.Context(), p.ID)
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

	allSongs, err := fetchAllSongs(r.Context())
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

	p, err := createPlaylist(r.Context(), name)
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

	if err := deletePlaylist(r.Context(), id); err != nil {
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

	if err := addSongToPlaylist(r.Context(), id, splitID); err != nil {
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

	if err := removeSongFromPlaylist(r.Context(), id, splitID); err != nil {
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
//   - ?radio=<name>: play a queue of random splits from that radio
//   - ?playlist=<id>: play a saved playlist, with optional ?song=<split id>
//   - no params: the empty state, which lists available radios to start
func handlePlayerView(w http.ResponseWriter, r *http.Request) {
	if shuffle := r.URL.Query().Get("shuffle"); shuffle != "" {
		var exclude []int64
		if ex := r.URL.Query().Get("exclude"); ex != "" {
			exclude = parseSplitIDs(ex)
		}

		splits, err := fetchGlobalShuffleBatch(r.Context(), shuffleBatchSize, exclude)
		if err != nil {
			slog.ErrorContext(r.Context(), "load global shuffle", "err", err)
			http.Error(w, "failed to load shuffle", http.StatusInternalServerError)
			return
		}

		songs := make([]PlaylistSong, 0, len(splits))
		for i, s := range splits {
			songs = append(songs, PlaylistSong{
				SplitID:  s.ID,
				Position: i,
				Split:    s,
				Plays:    s.Plays,
				Rating:   s.Rating,
			})
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := playerViewTemplate.Execute(w, playerViewData{
			Playlist: Playlist{Name: "All Music"},
			Songs:    songs,
			Subtitle: "Global Shuffle",
			QueueKey: "shuffle",
		}); err != nil {
			slog.ErrorContext(r.Context(), "render player view", "err", err)
		}
		return
	}

	if radio := r.URL.Query().Get("radio"); radio != "" {
		splits, err := fetchRadioSplits(r.Context(), radio, radioQueueSize)
		if err != nil {
			slog.ErrorContext(r.Context(), "load radio splits", "err", err, "radio", radio)
			http.Error(w, "failed to load radio", http.StatusInternalServerError)
			return
		}

		songs := make([]PlaylistSong, 0, len(splits))
		for i, s := range splits {
			songs = append(songs, PlaylistSong{
				SplitID:  s.ID,
				Position: i,
				Split:    s,
			})
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := playerViewTemplate.Execute(w, playerViewData{
			Playlist: Playlist{Name: radio},
			Songs:    songs,
			Subtitle: "Radio · " + radio,
			QueueKey: "radio:" + radio,
		}); err != nil {
			slog.ErrorContext(r.Context(), "render player view", "err", err)
		}
		return
	}

	playlistID, err := strconv.ParseInt(r.URL.Query().Get("playlist"), 10, 64)
	if err != nil || playlistID == 0 {
		radios, rerr := fetchRadios(r.Context())
		if rerr != nil {
			slog.ErrorContext(r.Context(), "list radios", "err", rerr)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := playerEmptyTemplate.Execute(w, playerEmptyData{Radios: radios}); err != nil {
			slog.ErrorContext(r.Context(), "render player empty", "err", err)
		}
		return
	}

	playlist, err := fetchPlaylist(r.Context(), playlistID)
	if err != nil {
		if err == sql.ErrNoRows {
			radios, rerr := fetchRadios(r.Context())
			if rerr != nil {
				slog.ErrorContext(r.Context(), "list radios", "err", rerr)
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err := playerEmptyTemplate.Execute(w, playerEmptyData{Radios: radios}); err != nil {
				slog.ErrorContext(r.Context(), "render player empty", "err", err)
			}
			return
		}
		slog.ErrorContext(r.Context(), "fetch playlist", "err", err, "playlist", playlistID)
		http.Error(w, "failed to load playlist", http.StatusInternalServerError)
		return
	}

	songs, err := fetchPlaylistSongs(r.Context(), playlistID)
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
		QueueKey:  "playlist:" + strconv.FormatInt(playlistID, 10),
	}); err != nil {
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

	splits, err := fetchGlobalShuffleBatch(r.Context(), shuffleBatchSize, exclude)
	if err != nil {
		slog.ErrorContext(r.Context(), "global shuffle json", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	tracks := make([]shuffleTrack, 0, len(splits))
	for _, s := range splits {
		tracks = append(tracks, shuffleTrack{
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

	w.Header().Set("Content-Type", "audio/mpeg")
	http.ServeFile(w, r, full)
}

// handleListPlaylistsJSON returns every playlist as JSON.
func handleListPlaylistsJSON(w http.ResponseWriter, r *http.Request) {
	playlists, err := fetchAllPlaylists(r.Context())
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

	playlist, err := fetchPlaylist(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "playlist not found")
		return
	}

	songs, err := fetchPlaylistSongs(r.Context(), id)
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
	songs, err := fetchAllSongs(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "list songs", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, songs)
}

// handleListRadiosJSON returns the distinct radios with playable splits.
func handleListRadiosJSON(w http.ResponseWriter, r *http.Request) {
	radios, err := fetchRadios(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "list radios", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, radios)
}
