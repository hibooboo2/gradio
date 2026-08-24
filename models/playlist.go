package models

import "time"

// Playlist is one row in the playlists table: a user-created collection of
// split output files.
type Playlist struct {
	ID         int64
	Name       string
	CreatedAt  time.Time
	TrackCount int
}

// PlaylistSong is one row in the playlist_splits table: a split that has been
// added to a playlist, joined with the full split row for display and playback.
type PlaylistSong struct {
	PlaylistID int64
	SplitID    int64
	Position   int
	Split      Split
	Plays      int
	Rating     int
}