package main

import (
	"html/template"
	"log/slog"
	"net/http"

	"github.com/hibooboo2/gradio/models"
)

// activeRecordingsViewTemplate renders the Active Recordings tab fragment:
// every station currently being recorded plus the stations queued behind a
// busy domain.
var activeRecordingsViewTemplate = template.Must(template.New("active").Funcs(viewFuncs).Parse(`
<div class="view-header">
	<h2>Active Recordings</h2>
	<p>{{len .Active}} recording{{if ne (len .Active) 1}}s{{end}} live{{if .Queued}} &mdash; {{len .Queued}} queued{{end}}</p>
</div>

{{if .Active}}
<table class="station-table active-table">
	<thead>
		<tr>
			<th>Station</th>
			<th>Domain</th>
			<th>Started</th>
			<th>Duration</th>
			<th>Buffer</th>
		</tr>
	</thead>
	<tbody>
		{{range .Active}}
		<tr class="station-row">
			<td>
				<span class="rec-indicator" title="Recording now"></span>
				<span class="station-name">{{if .URL}}<button type="button" class="station-name-btn" title="{{.URL}}" onclick="window.open('{{.URL}}','_blank','noopener,noreferrer')">{{.Name}}</button>{{else}}{{.Name}}{{end}}</span>
			</td>
			<td>{{if .Domain}}{{.Domain}}{{else}}&mdash;{{end}}</td>
			<td>{{timeFmt .Started}}</td>
			<td>{{if .Duration}}{{.Duration}}{{else}}&mdash;{{end}}</td>
			<td title="{{.BufferBytes}} bytes">{{if .BufferHuman}}{{.BufferHuman}}{{else}}0 B{{end}}</td>
		</tr>
		{{end}}
	</tbody>
</table>
{{else}}
<p class="empty">No active recordings.</p>
{{end}}

{{if .Queued}}
<h3 class="radio-section-title">Queued for a recording slot</h3>
<table class="station-table active-table">
	<thead>
		<tr>
			<th>Station</th>
			<th>Domain</th>
			<th>Position</th>
		</tr>
	</thead>
	<tbody>
		{{range .Queued}}
		<tr class="station-row">
			<td>
				<span class="rec-indicator queued" title="Waiting for a recording slot"></span>
				<span class="station-name">{{.Name}}</span>
			</td>
			<td>{{.Domain}}</td>
			<td>{{.Position}}</td>
		</tr>
		{{end}}
	</tbody>
</table>
{{end}}
`))

// handleActiveView renders the Active Recordings tab fragment listing every
// station currently being recorded (and those queued behind a busy domain).
func handleActiveView(w http.ResponseWriter, r *http.Request) {
	active := []models.ActiveRecorder{}
	queued := []models.QueuedRecorderView{}
	if recorderManager != nil {
		active, queued = recorderManager.ActiveRecorders()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := activeRecordingsViewTemplate.Execute(w, models.ActiveRecordingsViewData{
		Active: active,
		Queued: queued,
	}); err != nil {
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
