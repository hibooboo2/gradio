package models

// PlaylistsViewData is the data model for the play lists tab fragment.
type PlaylistsViewData struct {
	Playlists []PlaylistViewItem
	AllSongs  []Split
	Expanded  int64
}

// PlaylistViewItem pairs a playlist with the songs shown when it is expanded.
type PlaylistViewItem struct {
	Playlist
	Songs []PlaylistSong
}

// PlayerViewData is the data model for the player tab fragment.
type PlayerViewData struct {
	Playlist  Playlist
	Songs     []PlaylistSong
	StartSong int64
	// Subtitle overrides the default "Playlist · <name>" subtitle, e.g. for a
	// radio which shows "Radio · <name>".
	Subtitle string
	// QueueKey uniquely identifies the loaded queue (e.g. "radio:Slotex" or
	// "playlist:123") so the client can keep the currently playing queue when
	// the view is re-rendered instead of restarting it.
	QueueKey string
}

// PlayerEmptyData is the data model for the player empty state.
type PlayerEmptyData struct {
	Radios []Radio
}

// ShuffleTrack is one track in the global shuffle queue, as returned by the
// JSON continuation endpoint so the player can keep fetching fresh batches of
// least-played songs without a full view swap.
type ShuffleTrack struct {
	ID             int64   `json:"id"`
	Title          string  `json:"title"`
	DerivedTitle   string  `json:"derived_title"`
	CustomTitle    string  `json:"custom_title"`
	Src            string  `json:"src"`
	Start          float64 `json:"start"`
	End            float64 `json:"end"`
	Classification string  `json:"classification"`
	Plays          int     `json:"plays"`
	Rating         int     `json:"rating"`
}