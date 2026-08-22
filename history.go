package main

import (
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
)

// historyDefaultLimit is how many history entries the view shows by default.
const historyDefaultLimit = 100

// historyMaxLimit caps the ?limit= query param so a single request cannot ask
// for an unbounded history dump.
const historyMaxLimit = 1000

// historyViewData is the data model for the play history tab fragment.
type historyViewData struct {
	Sort    string // "recency" or "frequency"
	Group   bool   // group entries by radio
	Limit   int
	Total   int // number of distinct songs shown
	Entries []HistoryEntry
	Groups  []RadioHistoryGroup
}

// historyViewTemplate renders the htmx fragment for the play history tab. It
// shows the songs that were played, sorted by recency or frequency, optionally
// grouped by radio. The sort/group controls re-fetch the fragment and push the
// matching page URL so the state survives reloads and back/forward.
var historyViewTemplate = template.Must(template.New("history").Funcs(viewFuncs).Parse(`
<div class="view-header">
	<h2>Play History</h2>
	<p>{{.Total}} song{{if ne .Total 1}}s{{end}} played &mdash; sorted by {{if eq .Sort "frequency"}}frequency{{else}}recency{{end}}{{if .Group}}, grouped by radio{{end}}</p>
</div>

<form class="history-form" onsubmit="historyFormSubmit(event)">
	<select name="sort" onchange="this.form.requestSubmit()" title="Sort order">
		<option value="recency"{{if eq .Sort "recency"}} selected{{end}}>Sort by recency</option>
		<option value="frequency"{{if eq .Sort "frequency"}} selected{{end}}>Sort by frequency</option>
	</select>
	<label class="history-group-toggle" title="Group songs by the radio they were recorded from">
		<input type="checkbox" name="group" value="radio"{{if .Group}} checked{{end}} onchange="this.form.requestSubmit()">
		Group by radio
	</label>
</form>

{{if .Group}}
	{{range .Groups}}
	<section class="radio-group">
		<h2>
			<span class="radio-badge" style="background:{{.Color}}">{{.Radio}}</span>
			<span class="count">{{len .Plays}} song{{if ne (len .Plays) 1}}s{{end}}</span>
		</h2>
		<table>
			<thead>
				<tr>
					<th>Title</th>
					<th>Plays</th>
					<th>Last Played</th>
					<th>First Played</th>
				</tr>
			</thead>
			<tbody>
				{{range .Plays}}
				<tr>
					<td>{{songTitle .Split}}</td>
					<td>{{.Plays}}</td>
					<td>{{timeFmt .LastPlayed}}</td>
					<td>{{timeFmt .FirstPlayed}}</td>
				</tr>
				{{else}}
				<tr><td colspan="4" class="empty">No plays recorded yet.</td></tr>
				{{end}}
			</tbody>
		</table>
	</section>
	{{else}}
	<p class="empty">No plays recorded yet. Play some music and it will show up here.</p>
	{{end}}
{{else}}
<table class="history-table">
	<thead>
		<tr>
			<th>Title</th>
			<th>Radio</th>
			<th>Plays</th>
			<th>Last Played</th>
			<th>First Played</th>
		</tr>
	</thead>
	<tbody>
		{{range .Entries}}
		<tr>
			<td>{{songTitle .Split}}</td>
			<td>{{.Radio}}</td>
			<td>{{.Plays}}</td>
			<td>{{timeFmt .LastPlayed}}</td>
			<td>{{timeFmt .FirstPlayed}}</td>
		</tr>
		{{else}}
		<tr><td colspan="5" class="empty">No plays recorded yet. Play some music and it will show up here.</td></tr>
		{{end}}
	</tbody>
</table>
{{end}}
`))

// historyParams parses the sort/group/limit query params shared by the history
// view fragment and the JSON API. sort defaults to "recency"; group is enabled
// when group=radio; limit is clamped to [1, historyMaxLimit].
func historyParams(r *http.Request) (sort string, group bool, limit int) {
	sort = r.URL.Query().Get("sort")
	if sort != "frequency" {
		sort = "recency"
	}
	group = r.URL.Query().Get("group") == "radio"
	limit = historyDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= historyMaxLimit {
			limit = n
		}
	}
	return sort, group, limit
}

// handleHistoryView renders the play history tab fragment. It supports
// ?sort=recency|frequency (default recency), ?group=radio to group entries by
// radio, and ?limit=N to cap the number of entries.
func handleHistoryView(w http.ResponseWriter, r *http.Request) {
	sort, group, limit := historyParams(r)

	var entries []HistoryEntry
	var groups []RadioHistoryGroup
	var err error

	if group {
		groups, err = fetchPlayHistoryGrouped(limit)
	} else if sort == "frequency" {
		entries, err = fetchPlayHistoryFrequency(limit)
	} else {
		entries, err = fetchPlayHistoryRecency(limit)
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "load play history", "err", err)
		http.Error(w, "failed to load play history", http.StatusInternalServerError)
		return
	}

	total := len(entries)
	if group {
		total = 0
		for _, g := range groups {
			total += len(g.Plays)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := historyViewTemplate.Execute(w, historyViewData{
		Sort:    sort,
		Group:   group,
		Limit:   limit,
		Total:   total,
		Entries: entries,
		Groups:  groups,
	}); err != nil {
		slog.ErrorContext(r.Context(), "render play history view", "err", err)
	}
}

// handleHistoryJSON returns the play history as JSON. It accepts the same
// ?sort, ?group and ?limit query params as the view fragment.
func handleHistoryJSON(w http.ResponseWriter, r *http.Request) {
	sort, group, limit := historyParams(r)

	if group {
		groups, err := fetchPlayHistoryGrouped(limit)
		if err != nil {
			slog.ErrorContext(r.Context(), "play history json", "err", err)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, groups)
		return
	}

	var entries []HistoryEntry
	var err error
	if sort == "frequency" {
		entries, err = fetchPlayHistoryFrequency(limit)
	} else {
		entries, err = fetchPlayHistoryRecency(limit)
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "play history json", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entries)
}