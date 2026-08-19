package main

import (
	"context"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"time"
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

// routes returns the HTTP handler for the management API.
//
//	GET    /splits            list all splits
//	GET    /splits/{id}       get one split
//	PATCH  /splits/{id}       update a split's start, end, and/or classification
//	GET    /recordings        list all recordings
func routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /splits", handleListSplits)
	mux.HandleFunc("GET /splits/{id}", handleGetSplit)
	mux.HandleFunc("PATCH /splits/{id}", handleUpdateSplit)
	mux.HandleFunc("GET /recordings", handleListRecordings)

	mux.HandleFunc("GET /splits/view", handleSplitsView)

	// Serve the htmx web app from the web/ directory.
	mux.Handle("/", http.FileServer(http.Dir("web")))

	return mux
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
	splits, err := fetchAllSplits()
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

	split, err := fetchSplit(id)
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

	split, err := fetchSplit(id)
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

	if err := updateSplit(split); err != nil {
		slog.ErrorContext(r.Context(), "update split", "err", err, "id", id)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, split)
}

func handleListRecordings(w http.ResponseWriter, r *http.Request) {
	recordings, err := fetchAllRecordings()
	if err != nil {
		slog.ErrorContext(r.Context(), "list recordings", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, recordings)
}

// splitsViewTemplate renders the htmx fragment listing all splits.
var splitsViewTemplate = template.Must(template.New("splits").Parse(`<table>
	<thead>
		<tr>
			<th>ID</th>
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
		{{range .}}
		<tr>
			<td>{{.ID}}</td>
			<td>{{.SourcePath}}</td>
			<td>{{.Index}}</td>
			<td>{{printf "%.1f" .Start}}</td>
			<td>{{printf "%.1f" .End}}</td>
			<td>{{printf "%.1f" .Duration}}</td>
			<td>{{.Classification}}</td>
			<td>{{.OutputPath}}</td>
		</tr>
		{{else}}
		<tr><td colspan="8">No splits found.</td></tr>
		{{end}}
	</tbody>
</table>`))

// handleSplitsView renders an htmx-friendly HTML fragment listing all splits.
func handleSplitsView(w http.ResponseWriter, r *http.Request) {
	splits, err := fetchAllSplits()
	if err != nil {
		slog.ErrorContext(r.Context(), "list splits view", "err", err)
		http.Error(w, "failed to load splits", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := splitsViewTemplate.Execute(w, splits); err != nil {
		slog.ErrorContext(r.Context(), "render splits view", "err", err)
	}
}
