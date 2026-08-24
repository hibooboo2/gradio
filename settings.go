package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net/http"

	"github.com/hibooboo2/gradio/db"
)

// settingsViewTemplate renders the Settings tab fragment: a list of every
// stored setting with its raw JSON value. The frontend JS enhances the list
// with inline editing via the data-* attributes.
var settingsViewTemplate = template.Must(template.New("settings").Funcs(viewFuncs).Parse(`
<div data-settings-view>
	<div class="view-header">
		<h2>Settings</h2>
		<p>{{len .}} setting{{if ne (len .) 1}}s{{end}} &mdash; edit raw JSON values</p>
	</div>

	{{if .}}
	<ul class="settings-list">
		{{range .}}
		<li class="surface settings-item" data-setting-key="{{.Key}}">
			<div class="settings-head">
				<span class="settings-key">{{.Key}}</span>
				<span class="settings-meta">updated {{timeFmt .UpdatedAt}}</span>
			</div>
			<pre class="settings-value" data-setting-value>{{.Value}}</pre>
			<div class="settings-actions">
				<button type="button" class="btn-settings-edit" data-setting-edit title="Edit this setting">&#9998; Edit</button>
				<button type="button" class="btn-danger btn-settings-delete" data-setting-delete title="Delete this setting">&#128465; Delete</button>
			</div>
		</li>
		{{end}}
	</ul>
	{{else}}
	<p class="empty">No settings stored yet. Settings appear here as they are created.</p>
	{{end}}
</div>
`))

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
	if err := settingsViewTemplate.Execute(w, settings); err != nil {
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
		if errors.Is(err, sql.ErrNoRows) {
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
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "setting not found")
			return
		}
		slog.ErrorContext(r.Context(), "delete setting json", "err", err, "key", key)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": key})
}
