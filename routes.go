package main

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hibooboo2/gradio/db"
)

// serveAPI runs the management HTTP server until ctx is cancelled.
func serveAPI(ctx context.Context, addr string) error {
	server := &http.Server{
		Addr:    addr,
		Handler: routes(),
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("http server listening", "addr", addr)
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	case err := <-errCh:
		return err
	}
}

// routes returns the HTTP handler for the management API and web app.
//
// Page URLs (serve the htmx app shell; these are safe to navigate to directly
// and appear in the browser address bar):
//
//	GET    /splits                splits view
//	GET    /player                player view
//	GET    /playlists             play lists view
//	GET    /stations              radio stations view
//	GET    /favorites             favorite stations view
//	GET    /history               play history view
//	GET    /active                active recordings view
//
// JSON API backed directly by the cockroach tables:
//
//	GET    /api/splits            list all splits
//	GET    /api/playlists         list playlists
//	GET    /api/playlists/{id}    get one playlist with its songs
//	GET    /api/songs             list all splits available as songs
//
// GET    /api/radios            list radios with playable splits
// GET    /api/shuffle           next global-shuffle batch (?exclude=ids)
// GET    /api/history           play history (?sort=recency|frequency, ?group=radio, ?limit=N)
// GET    /api/active-recordings active recordings and queued stations
//
// Splits/recordings detail API:
//
//	GET    /splits/{id}           get one split
//	PATCH  /splits/{id}           update a split's start, end, and/or classification
//	POST   /api/splits/{id}/play  record that a split was listened to
//	POST   /api/splits/{id}/rating  like/dislike a split ({"rating":"like"|"dislike"|""})
//	POST   /api/splits/{id}/resplit  cut a split at {"cut":seconds} into two new splits
//	GET    /recordings            list all recordings
//	GET    /music/{path...}       serve a split output file from split_music/
//
// The htmx fragments (fetched by the UI; never shown in the address bar):
//
//	GET    /splits/view            splits tab fragment
//	GET    /playlists/view         play lists tab fragment
//	GET    /player/view            player tab fragment (?shuffle=1, ?playlist=.. or ?radio=..)
//	GET    /history/view           play history tab fragment (?sort=.., ?group=radio, ?limit=N)
//	GET    /active/view            active recordings tab fragment
//	POST   /playlists/create       create a playlist (form: name)
//	POST   /playlists/{id}/delete  delete a playlist
//	POST   /playlists/{id}/songs   add a split to a playlist (form: split_id)
//	POST   /playlists/{id}/songs/{split_id}/delete  remove a split from a playlist
func routes() http.Handler {
	mux := http.NewServeMux()

	// Page URLs serve the app shell so any view can be opened (or reloaded)
	// directly from the address bar.
	mux.HandleFunc("GET /splits", serveIndex)
	mux.HandleFunc("GET /player", serveIndex)
	mux.HandleFunc("GET /playlists", serveIndex)
	mux.HandleFunc("GET /stations", serveIndex)
	mux.HandleFunc("GET /favorites", serveIndex)
	mux.HandleFunc("GET /history", serveIndex)
	mux.HandleFunc("GET /active", serveIndex)

	// JSON API backed directly by the cockroach tables.
	mux.HandleFunc("GET /api/splits", handleListSplits)
	mux.HandleFunc("GET /api/playlists", handleListPlaylistsJSON)
	mux.HandleFunc("GET /api/playlists/{id}", handleGetPlaylistJSON)
	mux.HandleFunc("GET /api/songs", handleListSongsJSON)
	mux.HandleFunc("GET /api/radios", handleListRadiosJSON)
	mux.HandleFunc("GET /api/history", handleHistoryJSON)

	// Splits/recordings detail API.
	mux.HandleFunc("GET /splits/{id}", handleGetSplit)
	mux.HandleFunc("PATCH /splits/{id}", handleUpdateSplit)
	mux.HandleFunc("GET /recordings", handleListRecordings)
	mux.HandleFunc("POST /api/splits/{id}/play", handleRecordPlay)
	mux.HandleFunc("POST /api/splits/{id}/rating", handleSetRating)
	mux.HandleFunc("POST /api/splits/{id}/resplit", handleResplitSplit)
	mux.HandleFunc("POST /api/splits/{id}/merge", handleMergeSplit)

	// Global shuffle queue (JSON) for the player's Shuffle All mode.
	mux.HandleFunc("GET /api/shuffle", handleShuffleJSON)

	// Domain recording status (JSON): which domains are recording and which
	// stations are queued behind them.
	mux.HandleFunc("GET /api/record-domains", handleRecordDomainsJSON)

	// Active recordings (JSON): stations currently being recorded plus the
	// stations queued behind a busy domain.
	mux.HandleFunc("GET /api/active-recordings", handleActiveRecordingsJSON)

	// htmx fragments.
	mux.HandleFunc("GET /splits/view", handleSplitsView)
	mux.HandleFunc("GET /playlists/view", handlePlaylistsView)
	mux.HandleFunc("GET /player/view", handlePlayerView)
	mux.HandleFunc("GET /stations/view", handleStationsView)
	mux.HandleFunc("GET /favorites/view", handleFavoritesView)
	mux.HandleFunc("GET /history/view", handleHistoryView)
	mux.HandleFunc("GET /active/view", handleActiveView)
	mux.HandleFunc("POST /stations/{uuid}/record", handleStationRecord)
	mux.HandleFunc("POST /stations/{uuid}/favorite", handleToggleFavorite)

	mux.HandleFunc("POST /playlists/create", handleCreatePlaylist)
	mux.HandleFunc("POST /playlists/{id}/delete", handleDeletePlaylist)
	mux.HandleFunc("POST /playlists/{id}/songs", handleAddSong)
	mux.HandleFunc("POST /playlists/{id}/songs/{split_id}/delete", handleRemoveSong)

	// Serve the mp3 output files from split_music/ for the player.
	mux.HandleFunc("GET /music/{path...}", handleMusic)

	// Serve the htmx web app from the web/ directory.
	mux.Handle("/", http.FileServer(http.Dir("web")))

	return requireAuth(mux)
}

// serveIndex serves the htmx app shell so the page URLs work when opened or
// reloaded directly, letting the client-side router load the matching view.
func serveIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join("web", "index.html"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write json response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func handleListSplits(w http.ResponseWriter, r *http.Request) {
	splits, err := db.FetchAllSplits(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "list splits", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, splits)
}

func handleGetSplit(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid split id")
		return
	}

	split, err := db.FetchSplit(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "split not found")
		return
	}

	writeJSON(w, http.StatusOK, split)
}

// updateSplitRequest is the JSON body accepted by PATCH /splits/{id}. Fields
// are pointers so that omitted fields are left unchanged.
type updateSplitRequest struct {
	Start          *float64 `json:"start"`
	End            *float64 `json:"end"`
	Classification *string  `json:"classification"`
	// CustomTitle is the display-only rename for the split. "title" is
	// accepted as an alias for backward compatibility.
	CustomTitle *string `json:"custom_title"`
	Title       *string `json:"title"`
}

func handleUpdateSplit(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid split id")
		return
	}

	var req updateSplitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	split, err := db.FetchSplit(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "split not found")
		return
	}

	if req.Start != nil {
		split.Start = *req.Start
	}
	if req.End != nil {
		split.End = *req.End
	}
	if req.Classification != nil {
		split.Classification = *req.Classification
	}
	if req.CustomTitle != nil {
		split.CustomTitle = *req.CustomTitle
	} else if req.Title != nil {
		split.CustomTitle = *req.Title
	}

	if err := db.UpdateSplit(r.Context(), split); err != nil {
		slog.ErrorContext(r.Context(), "update split", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, split)
}

// handleRecordDomainsJSON reports which domains are currently being recorded
// and which stations are queued behind them.
func handleRecordDomainsJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, recorderManager.domainStatuses())
}

// handleRecordPlay records that a split was listened to, incrementing its play
// count in the song_plays table.
func handleRecordPlay(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid split id")
		return
	}

	if err := db.RecordPlay(r.Context(), id); err != nil {
		slog.ErrorContext(r.Context(), "record play", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	plays, rating, err := db.FetchSongStats(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "plays": plays, "rating": rating})
}

// handleSetRating records a like or dislike for a split. A like increments the
// split's rating counter; a dislike decrements it.
func handleSetRating(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid split id")
		return
	}

	var req struct {
		Rating string `json:"rating"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Rating != "like" && req.Rating != "dislike" {
		writeError(w, http.StatusBadRequest, "rating must be \"like\" or \"dislike\"")
		return
	}

	if err := db.SetRating(r.Context(), id, req.Rating == "like"); err != nil {
		slog.ErrorContext(r.Context(), "set rating", "err", err, "id", id, "rating", req.Rating)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	plays, rating, err := db.FetchSongStats(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "plays": plays, "rating": rating})
}

func handleListRecordings(w http.ResponseWriter, r *http.Request) {
	recordings, err := db.FetchAllRecordings(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "list recordings", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, recordings)
}

// handleResplitSplit cuts the current split at the given time, creating two new
// splits (with their own output files extracted from the original recording)
// and marking the original split as re_split.
func handleResplitSplit(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid split id")
		return
	}

	var req struct {
		Cut *float64 `json:"cut"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Cut == nil {
		writeError(w, http.StatusBadRequest, "cut is required")
		return
	}

	original, a, b, err := resplitSplit(r.Context(), id, *req.Cut)
	if err != nil {
		if errors.Is(err, errCutOutsideSplit) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		slog.ErrorContext(r.Context(), "resplit split", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"original": original,
		"a":        a,
		"b":        b,
	})
}

// handleMergeSplit joins the current split with the split immediately before
// (direction "prev") or after (direction "next") it in the same recording,
// marking both source splits re_split and creating a single merged split.
func handleMergeSplit(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid split id")
		return
	}

	var req struct {
		Direction *string `json:"direction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Direction == nil || (*req.Direction != "prev" && *req.Direction != "next") {
		writeError(w, http.StatusBadRequest, "direction must be \"prev\" or \"next\"")
		return
	}

	current, other, merged, err := mergeSplit(r.Context(), id, *req.Direction == "prev")
	if err != nil {
		if errors.Is(err, errNoAdjacentSplit) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		slog.ErrorContext(r.Context(), "merge split", "err", err, "id", id, "direction", *req.Direction)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"current": current,
		"other":   other,
		"merged":  merged,
	})
}

// splitsViewTemplate renders the htmx fragment listing all splits, grouped by
// radio with a color-coded badge per radio.
var splitsViewTemplate = template.Must(template.New("splits").Funcs(viewFuncs).Parse(`
{{range .}}
<section class="radio-group">
	<h2>
		<span class="radio-badge" style="background:{{.Color}}">{{.Radio}}</span>
		<span class="count">{{len .Splits}} split{{if ne (len .Splits) 1}}s{{end}}</span>
	</h2>
	<table>
		<thead>
			<tr>
				<th>ID</th>
				<th>Title</th>
				<th>Recording</th>
				<th>#</th>
				<th>Start</th>
				<th>End</th>
				<th>Duration</th>
				<th>Classification</th>
				<th>Output</th>
			</tr>
		</thead>
		<tbody>
			{{range .Splits}}
			<tr class="cls-{{.Classification}}">
				<td>{{.ID}}</td>
				<td>
					<span class="split-title" data-derived-title="{{derivedSongTitle .}}">{{songTitle .}}</span>
					<button type="button" class="title-edit" data-title-edit data-split="{{.ID}}" title="Rename this track">&#9999;&#65039;</button>
				</td>
				<td>{{.SourcePath}}</td>
				<td>{{.Index}}</td>
				<td>{{printf "%.1f" .Start}}</td>
				<td>{{printf "%.1f" .End}}</td>
				<td>{{printf "%.1f" .Duration}}</td>
				<td>{{clsLabel .Classification}}</td>
				<td>{{.OutputPath}}</td>
			</tr>
			{{else}}
			<tr><td colspan="9">No splits in this radio.</td></tr>
			{{end}}
		</tbody>
	</table>
</section>
{{else}}
<p class="empty">No splits found.</p>
{{end}}
`))

// radioGroup is a set of splits that share the same source radio, plus a
// display color for that radio.
type radioGroup struct {
	Radio  string
	Color  string
	Splits []db.Split
}

// radioPalette assigns a stable color to each distinct radio.
var radioPalette = db.RadioPalette

// handleSplitsView renders an htmx-friendly HTML fragment listing all splits,
// grouped by radio.
func handleSplitsView(w http.ResponseWriter, r *http.Request) {
	splits, err := db.FetchAllSplits(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "list splits view", "err", err)
		http.Error(w, "failed to load splits", http.StatusInternalServerError)
		return
	}

	// Group splits by radio, preserving first-seen order.
	order := []string{}
	byRadio := map[string][]db.Split{}
	for _, s := range splits {
		radio := radioFromPath(r.Context(), s.SourcePath)
		if _, ok := byRadio[radio]; !ok {
			order = append(order, radio)
		}
		byRadio[radio] = append(byRadio[radio], s)
	}

	colorOf := map[string]string{}
	for i, radio := range order {
		colorOf[radio] = radioPalette[i%len(radioPalette)]
	}

	groups := make([]radioGroup, 0, len(order))
	for _, radio := range order {
		groups = append(groups, radioGroup{
			Radio:  radio,
			Color:  colorOf[radio],
			Splits: byRadio[radio],
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := splitsViewTemplate.Execute(w, groups); err != nil {
		slog.ErrorContext(r.Context(), "render splits view", "err", err)
	}
}

// radioFromPath extracts the radio name from a source file path. Files are
// stored as recordings/<hash>/<file>.mp3 (or recordings/<radio>/<file>.mp3 for
// legacy recordings), or directly in recordings/ when no radio directory is
// present. A hashed directory is resolved back to the original station name
// via the recordings table so the UI shows display names, not hashes.
func radioFromPath(ctx context.Context, path string) string {
	dir := filepath.Base(filepath.Dir(path))
	if dir == "." || dir == "" || dir == "recordings" {
		return "manual"
	}
	return db.RadioDisplayName(ctx, dir)
}
