package models

// StationViewRow is one row in the Radio Stations tab.
type StationViewRow struct {
	RadioStation
	Recording bool
	Favorited bool
	Queued    bool
	Domain    string
	QueuePos  int
}

// StationsViewData is the data model for the Radio Stations / Favorites tab
// fragments.
type StationsViewData struct {
	Stations []StationViewRow
	// Query is the current name search, kept so the search box keeps its value.
	Query string
}

// StationRecordResponse is the JSON payload returned by
// POST /stations/{uuid}/record when the client asks for JSON. It carries the
// queue to load into the persistent mini player without swapping any HTML, so
// the current tab is left untouched. When the station was queued behind
// another station on the same domain, Queued is true and no tracks are
// returned.
type StationRecordResponse struct {
	StationName   string         `json:"station_name"`
	QueueKey      string         `json:"queue_key"`
	Source        string         `json:"source"`
	Tracks        []ShuffleTrack `json:"tracks"`
	Queued        bool           `json:"queued,omitempty"`
	Domain        string         `json:"domain,omitempty"`
	QueuePosition int            `json:"queue_position,omitempty"`
}