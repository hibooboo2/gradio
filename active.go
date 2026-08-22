package main

import (
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"time"
)

// activeRecorder is one station currently being recorded, with display info
// for the Active Recordings tab and the /api/active-recordings endpoint.
type activeRecorder struct {
	Name     string    `json:"name"`
	Domain   string    `json:"domain"`
	URL      string    `json:"url"`
	Started  time.Time `json:"started"`
	Duration string    `json:"duration"` // human-readable elapsed recording time
}

// queuedRecorderView is one station waiting for a recording slot on a domain
// that is already being recorded.
type queuedRecorderView struct {
	Name     string `json:"name"`
	Domain   string `json:"domain"`
	URL      string `json:"url"`
	Position int    `json:"position"`
}

// activeRecordingsViewData is the data model for the Active Recordings tab
// fragment.
type activeRecordingsViewData struct {
	Active []activeRecorder
	Queued []queuedRecorderView
}

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

// activeRecorders returns a snapshot of every station currently being recorded
// and every station queued behind a busy domain, sorted for stable display.
func (rs *recorderSet) activeRecorders() ([]activeRecorder, []queuedRecorderView) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	active := make([]activeRecorder, 0, len(rs.recorders))
	for name, rec := range rs.recorders {
		rec.mu.Lock()
		started := rec.started
		url := rec.url
		rec.mu.Unlock()
		duration := ""
		if !started.IsZero() {
			duration = fmtDuration(time.Since(started))
		}
		active = append(active, activeRecorder{
			Name:     name,
			Domain:   rec.domain,
			URL:      url,
			Started:  started,
			Duration: duration,
		})
	}
	sort.Slice(active, func(i, j int) bool { return active[i].Name < active[j].Name })

	queued := make([]queuedRecorderView, 0)
	for d, q := range rs.queues {
		for i, n := range q {
			queued = append(queued, queuedRecorderView{
				Name:     n.name,
				Domain:   d,
				URL:      n.url,
				Position: i + 1,
			})
		}
	}
	sort.Slice(queued, func(i, j int) bool {
		if queued[i].Domain != queued[j].Domain {
			return queued[i].Domain < queued[j].Domain
		}
		return queued[i].Position < queued[j].Position
	})

	return active, queued
}

// fmtDuration renders a duration as a compact human-readable string, e.g.
// "3m 12s" or "1h 05m". Empty when d is zero or negative.
func fmtDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// handleActiveView renders the Active Recordings tab fragment listing every
// station currently being recorded (and those queued behind a busy domain).
func handleActiveView(w http.ResponseWriter, r *http.Request) {
	active := []activeRecorder{}
	queued := []queuedRecorderView{}
	if recorderManager != nil {
		active, queued = recorderManager.activeRecorders()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := activeRecordingsViewTemplate.Execute(w, activeRecordingsViewData{
		Active: active,
		Queued: queued,
	}); err != nil {
		slog.ErrorContext(r.Context(), "render active recordings view", "err", err)
	}
}

// handleActiveRecordingsJSON returns the currently active recordings and the
// stations queued behind a busy domain as JSON.
func handleActiveRecordingsJSON(w http.ResponseWriter, r *http.Request) {
	active := []activeRecorder{}
	queued := []queuedRecorderView{}
	if recorderManager != nil {
		active, queued = recorderManager.activeRecorders()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"active": active,
		"queued": queued,
	})
}
