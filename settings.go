package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/hibooboo2/gradio/db"
	"github.com/hibooboo2/gradio/views"
	"github.com/jackc/pgx/v5"
)

// handleSettingsView renders the Settings tab fragment listing every stored
// setting.
func handleSettingsView(w http.ResponseWriter, r *http.Request) {
	settings, err := db.ListSettings(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "list settings view", "err", err)
		http.Error(w, "failed to load settings", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.SettingsView(settings).Render(r.Context(), w); err != nil {
		slog.ErrorContext(r.Context(), "render settings view", "err", err)
	}
}

// handleListSettingsJSON returns every setting as a JSON array of
// {key, value, updated_at}. Values are kept as raw JSON (not double-encoded).
func handleListSettingsJSON(w http.ResponseWriter, r *http.Request) {
	settings, err := db.ListSettings(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "list settings json", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

// handleGetSettingJSON returns a single setting as {key, value}, keeping the
// value as raw JSON. It returns 404 when the key has not been set.
func handleGetSettingJSON(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	value, err := db.GetSettingRaw(r.Context(), key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "setting not found")
			return
		}
		slog.ErrorContext(r.Context(), "get setting json", "err", err, "key", key)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "value": value})
}

// handleSetSettingJSON stores a setting. The request body is the raw JSON
// value (any valid JSON); it is validated before being stored. Returns the
// stored {key, value} on success.
func handleSetSettingJSON(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	body, err := io.ReadAll(r.Body)
	if err != nil {
		slog.ErrorContext(r.Context(), "read setting body", "err", err, "key", key)
		writeError(w, http.StatusInternalServerError, "failed to read request body")
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "request body must be a JSON value")
		return
	}
	if !json.Valid(body) {
		writeError(w, http.StatusBadRequest, "request body is not valid JSON")
		return
	}

	value := json.RawMessage(body)
	if err := db.SetSetting(r.Context(), key, value); err != nil {
		slog.ErrorContext(r.Context(), "set setting json", "err", err, "key", key)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"key": key, "value": value})
}

// handleDeleteSettingJSON deletes a setting by key. It returns 404 when the
// key does not exist.
func handleDeleteSettingJSON(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	err := db.DeleteSetting(r.Context(), key)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "setting not found")
			return
		}
		slog.ErrorContext(r.Context(), "delete setting json", "err", err, "key", key)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": key})
}
