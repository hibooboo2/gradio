package main

import (
	"database/sql"
	"html/template"
	"log/slog"
	"net/http"
)

// stationsViewTemplate renders the Radio Stations tab fragment: every station
// from the radio_stations table with a play/record button.
var stationsViewTemplate = template.Must(template.New("stations").Funcs(viewFuncs).Parse(`
<div class="view-header">
	<h2>Radio Stations</h2>
	<p>{{len .Stations}} station{{if ne (len .Stations) 1}}s{{end}} &mdash; click one to start recording and play its songs</p>
</div>

{{if .Stations}}
<table class="station-table">
	<thead>
		<tr>
			<th>Station</th>
			<th>Country</th>
			<th>Language</th>
			<th>Tags</th>
			<th></th>
		</tr>
	</thead>
	<tbody>
		{{range .Stations}}
		<tr class="station-row">
			<td>
				<span class="station-favicon">
					{{if .Favicon}}<img src="{{.Favicon}}" alt="" loading="lazy" onerror="this.style.visibility='hidden'">{{else}}&#128276;{{end}}
				</span>
				<span class="station-name">{{.Name}}</span>
				{{if .Favorited}}<span class="station-star" title="Favorited">&#11088;</span>{{end}}
			</td>
			<td>{{if .CountryCode}}{{.CountryCode}}{{else}}&mdash;{{end}}</td>
			<td>{{if .LanguageCodes}}{{.LanguageCodes}}{{else}}&mdash;{{end}}</td>
			<td class="station-tags">{{if .Tags}}{{.Tags}}{{else}}&mdash;{{end}}</td>
			<td class="station-actions">
				<button class="station-fav" data-station-uuid="{{.StationUUID}}" data-station-name="{{.Name}}"
					hx-post="/stations/{{.StationUUID}}/favorite" hx-target="#content" hx-swap="innerHTML"
					hx-vals='{"view": "stations"}'
					{{if .Favorited}}title="Unfavorite"{{else}}title="Favorite"{{end}}>
					{{if .Favorited}}&#11088;&#65039;{{else}}&#9734;{{end}}
				</button>
				<button class="station-record" data-station-uuid="{{.StationUUID}}" data-station-name="{{.Name}}"
					hx-post="/stations/{{.StationUUID}}/record" hx-target="#content" hx-swap="innerHTML"
					hx-on:click="selectTab('player')"
					{{if .Recording}}disabled title="Already recording"{{else}}title="Record this station and play its songs"{{end}}>
					{{if .Recording}}&#9889; Recording{{else}}&#128311; Record &amp; Play{{end}}
				</button>
				<span class="station-status" id="status-{{.StationUUID}}"></span>
			</td>
		</tr>
		{{end}}
	</tbody>
</table>
{{else}}
<p class="empty">No radio stations loaded yet. Run <code>gradio -sync</code> to populate the stations table.</p>
{{end}}
`))

// favoritesViewTemplate renders the Favorites tab fragment: only the stations
// that have been favorited.
var favoritesViewTemplate = template.Must(template.New("favorites").Funcs(viewFuncs).Parse(`
<div class="view-header">
	<h2>Favorite Radio Stations</h2>
	<p>{{len .Stations}} favorited station{{if ne (len .Stations) 1}}s{{end}} &mdash; click one to start recording and play its songs</p>
</div>

{{if .Stations}}
<table class="station-table">
	<thead>
		<tr>
			<th>Station</th>
			<th>Country</th>
			<th>Language</th>
			<th>Tags</th>
			<th></th>
		</tr>
	</thead>
	<tbody>
		{{range .Stations}}
		<tr class="station-row">
			<td>
				<span class="station-favicon">
					{{if .Favicon}}<img src="{{.Favicon}}" alt="" loading="lazy" onerror="this.style.visibility='hidden'">{{else}}&#128276;{{end}}
				</span>
				<span class="station-name">{{.Name}}</span>
				<span class="station-star" title="Favorited">&#11088;</span>
			</td>
			<td>{{if .CountryCode}}{{.CountryCode}}{{else}}&mdash;{{end}}</td>
			<td>{{if .LanguageCodes}}{{.LanguageCodes}}{{else}}&mdash;{{end}}</td>
			<td class="station-tags">{{if .Tags}}{{.Tags}}{{else}}&mdash;{{end}}</td>
			<td class="station-actions">
				<button class="station-fav" data-station-uuid="{{.StationUUID}}" data-station-name="{{.Name}}"
					hx-post="/stations/{{.StationUUID}}/favorite" hx-target="#content" hx-swap="innerHTML"
					hx-vals='{"view": "favorites"}'
					title="Unfavorite">&#11088;&#65039;</button>
				<button class="station-record" data-station-uuid="{{.StationUUID}}" data-station-name="{{.Name}}"
					hx-post="/stations/{{.StationUUID}}/record" hx-target="#content" hx-swap="innerHTML"
					hx-on:click="selectTab('player')"
					{{if .Recording}}disabled title="Already recording"{{else}}title="Record this station and play its songs"{{end}}>
					{{if .Recording}}&#9889; Recording{{else}}&#128311; Record &amp; Play{{end}}
				</button>
				<span class="station-status" id="status-{{.StationUUID}}"></span>
			</td>
		</tr>
		{{end}}
	</tbody>
</table>
{{else}}
<p class="empty">No favorite stations yet. Click the &#9734; star next to a station on the Radio Stations tab to favorite it.</p>
{{end}}
`))

// stationViewRow is one row in the Radio Stations tab.
type stationViewRow struct {
	RadioStation
	Recording bool
	Favorited bool
}

// stationsViewData is the data model for the Radio Stations / Favorites tab
// fragments.
type stationsViewData struct {
	Stations []stationViewRow
}

// handleStationsView renders the Radio Stations tab fragment listing every
// station in the radio_stations table with its current recording state.
func handleStationsView(w http.ResponseWriter, r *http.Request) {
	stations, err := fetchRadioStations()
	if err != nil {
		slog.ErrorContext(r.Context(), "list radio stations", "err", err)
		http.Error(w, "failed to load radio stations", http.StatusInternalServerError)
		return
	}

	favs, err := fetchFavoriteUUIDs()
	if err != nil {
		slog.ErrorContext(r.Context(), "list favorites", "err", err)
		http.Error(w, "failed to load favorites", http.StatusInternalServerError)
		return
	}

	rows := make([]stationViewRow, 0, len(stations))
	for _, s := range stations {
		_, faved := favs[s.StationUUID]
		rows = append(rows, stationViewRow{
			RadioStation: s,
			Recording:    recorderManager.isRecording(s.Name),
			Favorited:    faved,
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := stationsViewTemplate.Execute(w, stationsViewData{Stations: rows}); err != nil {
		slog.ErrorContext(r.Context(), "render stations view", "err", err)
	}
}

// handleFavoritesView renders the Favorites tab fragment listing only the
// stations that have been favorited.
func handleFavoritesView(w http.ResponseWriter, r *http.Request) {
	stations, err := fetchFavoriteStations()
	if err != nil {
		slog.ErrorContext(r.Context(), "list favorite stations", "err", err)
		http.Error(w, "failed to load favorite stations", http.StatusInternalServerError)
		return
	}

	rows := make([]stationViewRow, 0, len(stations))
	for _, s := range stations {
		rows = append(rows, stationViewRow{
			RadioStation: s,
			Recording:    recorderManager.isRecording(s.Name),
			Favorited:    true,
		})
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := favoritesViewTemplate.Execute(w, stationsViewData{Stations: rows}); err != nil {
		slog.ErrorContext(r.Context(), "render favorites view", "err", err)
	}
}

// handleToggleFavorite favorites or unfavorites a station and re-renders the
// view the request came from (?view=stations or ?view=favorites).
func handleToggleFavorite(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	if _, err := fetchRadioStationByUUID(uuid); err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "station not found", http.StatusNotFound)
			return
		}
		slog.ErrorContext(r.Context(), "fetch station", "err", err)
		http.Error(w, "failed to load station", http.StatusInternalServerError)
		return
	}

	favs, err := fetchFavoriteUUIDs()
	if err != nil {
		slog.ErrorContext(r.Context(), "list favorites", "err", err)
		http.Error(w, "failed to load favorites", http.StatusInternalServerError)
		return
	}

	if _, ok := favs[uuid]; ok {
		err = removeFavorite(uuid)
	} else {
		err = addFavorite(uuid)
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "toggle favorite", "err", err, "uuid", uuid)
		http.Error(w, "failed to update favorite", http.StatusInternalServerError)
		return
	}

	view := r.URL.Query().Get("view")
	if view == "favorites" {
		handleFavoritesView(w, r)
		return
	}
	handleStationsView(w, r)
}

// stationNoSongsTemplate is rendered when a station has no recorded songs yet.
var stationNoSongsTemplate = template.Must(template.New("stationNoSongs").Funcs(viewFuncs).Parse(`
<div class="player-empty surface">
	<p class="empty-icon">&#127911;</p>
	<p class="empty">No songs for {{.Name}} yet.</p>
	<p class="empty-sub">Recording is running &mdash; songs will appear here once the stream has been recorded and split. Check back after the first split finishes.</p>
</div>
`))

// handleStationRecord starts recording the given station (if it is not already
// being recorded) and then renders the player for the songs recorded so far.
// When the station has no recorded songs yet, it shows a friendly message
// instead of an empty player.
func handleStationRecord(w http.ResponseWriter, r *http.Request) {
	station, err := fetchRadioStationByUUID(r.PathValue("uuid"))
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "station not found", http.StatusNotFound)
			return
		}
		slog.ErrorContext(r.Context(), "fetch station", "err", err)
		http.Error(w, "failed to load station", http.StatusInternalServerError)
		return
	}

	if station.URLResolved == "" {
		http.Error(w, "station has no resolvable stream url", http.StatusBadRequest)
		return
	}

	recorderManager.start(station.Name, station.URLResolved)

	splits, err := fetchRadioSplits(station.Name, radioQueueSize)
	if err != nil {
		slog.ErrorContext(r.Context(), "load radio splits", "err", err, "radio", station.Name)
		http.Error(w, "failed to load radio", http.StatusInternalServerError)
		return
	}

	if len(splits) == 0 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := stationNoSongsTemplate.Execute(w, struct{ Name string }{station.Name}); err != nil {
			slog.ErrorContext(r.Context(), "render no songs", "err", err)
		}
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
		Playlist: Playlist{Name: station.Name},
		Songs:    songs,
		Subtitle: "Radio · " + station.Name,
		QueueKey: "radio:" + station.Name,
	}); err != nil {
		slog.ErrorContext(r.Context(), "render player view", "err", err)
	}
}
