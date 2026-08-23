package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/hibooboo2/gradio/db"
)

// initStationsPath is the local snapshot of the radio-browser "list of all
// radio stations" endpoint used to populate the radio_stations table.
const initStationsPath = "initstations.json"

// radioBrowserStation mirrors the subset of the radio-browser station JSON we
// care about. url_resolved is the resolved stream URL and is the one used for
// recording; name is the key used to select a station.
type radioBrowserStation struct {
	StationUUID   string `json:"stationuuid"`
	Name          string `json:"name"`
	URLResolved   string `json:"url_resolved"`
	Favicon       string `json:"favicon"`
	Tags          string `json:"tags"`
	CountryCode   string `json:"countrycode"`
	LanguageCodes string `json:"languagecodes"`
}

// syncRadioStations reads every station from initstations.json and upserts
// them into the radio_stations table. It returns the number of stations
// stored.
func syncRadioStations(ctx context.Context) (int, error) {
	body, err := os.ReadFile(initStationsPath)
	if err != nil {
		return 0, err
	}

	var page []radioBrowserStation
	if err := json.Unmarshal(body, &page); err != nil {
		return 0, err
	}

	stations := make([]db.RadioStation, 0, len(page))
	for _, s := range page {
		if s.URLResolved == "" {
			continue
		}
		stations = append(stations, db.RadioStation{
			StationUUID:   s.StationUUID,
			Name:          s.Name,
			URLResolved:   s.URLResolved,
			Favicon:       s.Favicon,
			Tags:          s.Tags,
			CountryCode:   s.CountryCode,
			LanguageCodes: s.LanguageCodes,
		})
	}

	if err := db.UpsertRadioStations(ctx, stations); err != nil {
		return 0, fmt.Errorf("upsert radio stations: %w", err)
	}

	return len(stations), nil
}

// radioURLs returns the recording urls keyed by station name, loaded from the
// radio_stations table. It falls back to the built-in defaults when the table
// has no stations yet.
func radioURLs(ctx context.Context) map[string]string {
	urls, err := db.FetchRadioStationURLs(ctx)
	if err != nil {
		slog.Error("load radio station urls", "err", err)
		return defaultURLs()
	}
	if len(urls) == 0 {
		return defaultURLs()
	}
	return urls
}

// defaultURLs is the small set of known-good streams used when the
// radio_stations table is empty or the database is unreachable.
func defaultURLs() map[string]string {
	return map[string]string{
		"GayPHXRadio": streamURL,
		"RandomRadio": streamURL2,
		"Slotex":      "https://s3.slotex.pl:7076/;",
	}
}

const (
	streamURL  = "https://radio.gayphx.com/listen/gayphx/radio.mp3"
	streamURL2 = "https://maxfm.ice.infomaniak.ch/maxfm-945.mp3"
	PlayRadio  = "Slotex"
)
