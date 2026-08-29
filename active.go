package main

import (
	"log/slog"
	"net/http"

	"github.com/hibooboo2/gradio/models"
	"github.com/hibooboo2/gradio/views"
)

// handleActiveView renders the Active Recordings tab fragment listing every
// station currently being recorded (and those queued behind a busy domain).
func handleActiveView(w http.ResponseWriter, r *http.Request) {
	active := []models.ActiveRecorder{}
	queued := []models.QueuedRecorderView{}
	if recorderManager != nil {
		active, queued = recorderManager.ActiveRecorders()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.ActiveView(models.ActiveRecordingsViewData{
		Active: active,
		Queued: queued,
	}).Render(r.Context(), w); err != nil {
		slog.ErrorContext(r.Context(), "render active recordings view", "err", err)
	}
}

// handleActiveRecordingsJSON returns the currently active recordings and the
// stations queued behind a busy domain as JSON.
func handleActiveRecordingsJSON(w http.ResponseWriter, r *http.Request) {
	active := []models.ActiveRecorder{}
	queued := []models.QueuedRecorderView{}
	if recorderManager != nil {
		active, queued = recorderManager.ActiveRecorders()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active": active,
		"queued": queued,
	})
}
