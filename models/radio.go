package models

// Radio is one distinct radio that has split files, plus the number of splits
// available for it.
type Radio struct {
	Name       string
	SplitCount int
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

// RadioBrowserStation mirrors the subset of the radio-browser station JSON we
// care about. url_resolved is the resolved stream URL and is the one used for
// recording; name is the key used to select a station.
type RadioBrowserStation struct {
	StationUUID   string `json:"stationuuid"`
	Name          string `json:"name"`
	URLResolved   string `json:"url_resolved"`
	Favicon       string `json:"favicon"`
	Tags          string `json:"tags"`
	CountryCode   string `json:"countrycode"`
	LanguageCodes string `json:"languagecodes"`
}

// RadioGroup is a set of splits that share the same source radio, plus a
// display color for that radio.
type RadioGroup struct {
	Radio  string
	Color  string
	Splits []Split
}

// RadioPalette assigns a stable color to each distinct radio.
var RadioPalette = []string{
	"#6366f1", // indigo
	"#ec4899", // pink
	"#10b981", // emerald
	"#f59e0b", // amber
	"#06b6d4", // cyan
	"#8b5cf6", // violet
	"#ef4444", // red
	"#14b8a6", // teal
}