package models

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

// Silence is an interval of silence detected by ffmpeg's silencedetect filter.
type Silence struct {
	Start float64
	End   float64
}

// Boundary marks where one output file ends and the next begins. A split is
// never made in the middle of a silence: the previous file runs through the
// end of the detected silence (boundary.end) while the next file begins at
// the start of that same silence (boundary.start), so both files keep the
// whole silence instead of cutting through it.
type Boundary struct {
	Start float64
	End   float64
}

// ClassificationOption pairs a classification value with the emoji + label
// shown in the UI dropdowns. The order here is the order the options appear.
type ClassificationOption struct {
	Value string
	Label string
}

// ClassificationOptions is the central list of every classification a split
// may carry, used by the UI dropdowns and for validation.
var ClassificationOptions = []ClassificationOption{
	{Value: ClassificationNotSong, Label: "⏭️ Not Song / Short clip"},
	{Value: ClassificationLikelySong, Label: "🎵 Likely Song"},
	{Value: ClassificationSong, Label: "🎶 Song"},
	{Value: ClassificationCommercial, Label: "📢 Commercial"},
	{Value: ClassificationInformational, Label: "ℹ️ Informational"},
	{Value: ClassificationNews, Label: "📰 News"},
	{Value: ClassificationReSplit, Label: "✂️ Re-split"},
}

// ValidClassifications is the set of classification values a split may carry.
var ValidClassifications = func() map[string]struct{} {
	m := make(map[string]struct{}, len(ClassificationOptions))
	for _, o := range ClassificationOptions {
		m[o.Value] = struct{}{}
	}
	return m
}()

// IsValidClassification reports whether cls is one of the known classifications.
func IsValidClassification(cls string) bool {
	_, ok := ValidClassifications[cls]
	return ok
}

// ClsLabel returns the emoji + description label for a classification value.
func ClsLabel(cls string) string {
	for _, o := range ClassificationOptions {
		if o.Value == cls {
			return o.Label
		}
	}
	return cls
}