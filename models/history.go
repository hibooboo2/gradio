package models

import "time"

// HistoryEntry is one song in the play history, joined with its full split row
// and the radio it was recorded from, plus the aggregated play count and the
// first and last time it was played.
type HistoryEntry struct {
	Split       Split     `json:"split"`
	Radio       string    `json:"radio"`
	Plays       int       `json:"plays"`
	LastPlayed  time.Time `json:"last_played"`
	FirstPlayed time.Time `json:"first_played"`
}

// RadioHistoryGroup is a set of history entries that share the same radio,
// plus a display color for that radio.
type RadioHistoryGroup struct {
	Radio string         `json:"radio"`
	Color string         `json:"color"`
	Plays []HistoryEntry `json:"plays"`
}

// HistoryViewData is the data model for the play history tab fragment.
type HistoryViewData struct {
	Sort    string // "recency" or "frequency"
	Group   bool   // group entries by radio
	Limit   int
	Total   int // number of distinct songs shown
	Entries []HistoryEntry
	Groups  []RadioHistoryGroup
}