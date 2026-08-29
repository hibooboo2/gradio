package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/hibooboo2/gradio/db"
	"github.com/hibooboo2/gradio/models"
)

// initStationsPath is the local snapshot of the radio-browser "list of all
// radio stations" endpoint used to populate the radio_stations table.
const (
	initStationsPath    = "initstations.json"
	initStationsPathURL = "http://de1.api.radio-browser.info/json/stations?limit=1000000"
)

// syncRadioStations reads every station from initstations.json and upserts
// them into the radio_stations table. It returns the number of stations
// stored.
func syncRadioStations(ctx context.Context) (int, error) {
	stationsParsed := StreamItems[models.RadioBrowserStation](ctx)
	total := 0
	batchSize := 5000
	stations := make([]models.RadioStation, 0, batchSize)
	for s := range stationsParsed {
		if s.URLResolved == "" {
			continue
		}
		stations = append(stations, models.RadioStation{
			StationUUID:   s.StationUUID,
			Name:          s.Name,
			URLResolved:   s.URLResolved,
			Favicon:       s.Favicon,
			Tags:          s.Tags,
			CountryCode:   s.CountryCode,
			LanguageCodes: s.LanguageCodes,
		})
		total++

		if len(stations) == cap(stations) {
			if err := db.UpsertRadioStations(ctx, stations); err != nil {
				return 0, fmt.Errorf("upsert radio stations: %w", err)
			}
			slog.DebugContext(ctx, "Stations upserted", "total", len(stations))
			stations = make([]models.RadioStation, 0, batchSize)
		}
	}

	if err := db.UpsertRadioStations(ctx, stations); err != nil {
		return 0, fmt.Errorf("upsert radio stations: %w", err)
	}

	slog.DebugContext(ctx, "Stations upserted", "total", len(stations))
	return total, nil
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

func StreamItems[Val any](ctx context.Context) chan Val {
	vals := make(chan Val)
	go func() {
		var data io.ReadCloser
		defer func() {
			slog.InfoContext(ctx, fmt.Sprintf("Calling close on data: %T", data))
			data.Close()
		}()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, initStationsPathURL, nil)
		if err != nil {
			slog.ErrorContext(ctx, "failed to get stations from url", "url", initStationsPathURL, "err", err)
			data, err = os.Open(initStationsPath)
			if err != nil {
				slog.ErrorContext(ctx, "Failed to open station json path", "err", err)
				return
			}
		} else {
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				slog.ErrorContext(ctx, "failed to do client request")
				return
			}
			data = resp.Body
			if resp.StatusCode >= 400 {
				slog.ErrorContext(ctx, "FAiled due to status", "status", resp.Status)
				return
			}
		}

		dec := json.NewDecoder(data)

		// [
		tok, err := dec.Token()
		if err != nil {
			slog.ErrorContext(ctx, "Did not start a list with token", "err", err)
			return
		}

		switch tok {
		case json.Delim('['):
		default:
			slog.ErrorContext(ctx, "expected JSON array", "token", tok)
			return
		}

		defer close(vals)
		for dec.More() {
			var v Val
			if err := dec.Decode(&v); err != nil {
				slog.ErrorContext(ctx, "failed to decode val", "err", err)
				continue
			}
			Send(ctx, v, vals)
		}
		// ]
		_, err = dec.Token()
		if err != nil {
			slog.ErrorContext(ctx, "Failed token", "err", err)
		}
	}()

	return vals
}

func Send[Val any](ctx context.Context, v Val, vals chan Val) {
	select {
	case vals <- v:
	case <-ctx.Done():
	}
}
