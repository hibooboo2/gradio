package db

import "time"

// RecordingStatus tracks where a source recording is in the processing pipeline.
type RecordingStatus string

const (
	StatusPending    RecordingStatus = "pending"
	StatusProcessing RecordingStatus = "processing"
	StatusProcessed  RecordingStatus = "processed"
	StatusError      RecordingStatus = "error"
)

// Recording is one row in the recordings table: a source file produced by the
// recorder that still needs (or already received) silence splitting.
type Recording struct {
	ID         int64
	SourcePath string
	Radio      string
	RecordedAt time.Time
	SizeBytes  int64
	Status     RecordingStatus
}

// Split is one row in the splits table: a single output file produced by
// splitting a source recording, along with the boundary (cutoff) in the
// original source stream and the position of the file within that stream.
type Split struct {
	ID             int64
	RecordingID    int64
	SourcePath     string
	Index          int
	Start          float64
	End            float64
	OutputPath     string
	Classification string
	// CustomTitle overrides the derived display title (songTitle). It is a
	// display-only rename: the source/output files are never touched.
	CustomTitle string `json:"custom_title"`
	// Plays and Rating are populated only when a query joins the song_plays
	// table (player queues, global shuffle). Rating is the like count.
	Plays  int
	Rating int
}

// Duration returns the length of the split in seconds.
func (s Split) Duration() float64 {
	return s.End - s.Start
}

// Radio is one distinct radio that has split files, plus the number of splits
// available for it.
type Radio struct {
	Name       string
	SplitCount int
}

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

// User is one row in the users table. Password holds the bcrypt hash of the
// user's basic-auth password; it is never stored in plaintext.
type User struct {
	ID       int64
	Name     string
	Password string
}

// RadioStation is one row in the radio_stations table: a stream resolvable on
// the internet with identifying metadata pulled from radio-browser.info.
type RadioStation struct {
	StationUUID   string
	Name          string
	URLResolved   string
	Favicon       string
	Tags          string
	CountryCode   string
	LanguageCodes string
}

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

// Classification values assigned to a split when it is first created. A split
// under a minute is unlikely to be a full song; anything at or over a minute
// is a probable song.
const (
	ClassificationNotSong       = "not_song"
	ClassificationLikelySong    = "likely_song"
	ClassificationCommercial    = "commercial"
	ClassificationSong          = "song"
	ClassificationReSplit       = "re_split"
	ClassificationInformational = "informational"
	ClassificationNews          = "news"
)
