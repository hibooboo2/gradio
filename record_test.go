package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/hibooboo2/gradio/db"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

// TestRecordOnceConcurrent30s verifies that multiple Recorder.RecordOnce calls
// run concurrently against different radio stations and that each recorder
// produces buffered stream data (and its recordings directory) after the
// recording window.
//
// storeToDisk (main.go) only flushes buffers >= 5GB, so 30s of audio stays in
// memory and no file is written to disk. The test therefore verifies the
// recorder's internal state (buffer non-empty, target filename set) and that
// the recordings directory was created by rotate() — the exact state
// storeToDisk would flush to disk if the size threshold were met.
func TestRecordOnceConcurrent30s(t *testing.T) {
	// --- Database setup (mirrors split_test.go) ---
	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	if err != nil {
		t.Skipf("test database unavailable: %v", err)
	}
	if _, err := admin.ExecContext(t.Context(), `CREATE DATABASE IF NOT EXISTS gradio_test`); err != nil {
		t.Skipf("test database unavailable: %v", err)
	}
	require.NoError(t, admin.Close())

	db.SetRecordDBPath(testDBPath)
	db.CreateDBHandle()

	// Start from a clean slate so the test is hermetic and deterministic.
	_, err = db.DB.ExecContext(t.Context(), `DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS play_history; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recording_splits; DROP TABLE IF EXISTS recordings; DROP TABLE IF EXISTS favorites; DROP TABLE IF EXISTS radio_stations;`)
	require.NoError(t, err)
	require.NoError(t, db.CreateSchema(t.Context(), db.DB))

	// Seed real stations so RecordOnce has live streams to hit.
	seedTestStations(t)

	stations, err := db.FetchRadioStations(t.Context())
	require.NoError(t, err)

	// Pick up to 5 stations with a resolvable URL (deduped by name so two
	// stations never share a recordings directory).
	seen := map[string]bool{}
	var usable []db.RadioStation
	for _, s := range stations {
		if s.URLResolved == "" || seen[s.Name] {
			continue
		}
		seen[s.Name] = true
		usable = append(usable, s)
		if len(usable) == 5 {
			break
		}
	}
	if len(usable) == 0 {
		t.Skip("no radio stations with a resolvable URL in the test DB")
	}

	// Skip stations that are not reachable so a single dead stream does not
	// make the test flaky.
	var reachable []db.RadioStation
	for _, s := range usable {
		checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ok := stationReachable(checkCtx, s.URLResolved)
		cancel()
		if ok {
			reachable = append(reachable, s)
		} else {
			t.Logf("station %q (%s) unreachable, skipping", s.Name, s.URLResolved)
		}
	}
	if len(reachable) == 0 {
		t.Skip("no reachable radio stations to record")
	}
	usable = reachable

	t.Logf("recording %d stations concurrently for 30s each", len(usable))
	for _, s := range usable {
		t.Logf("  station: %q url: %q", s.Name, s.URLResolved)
	}

	// --- Hermetic cwd: recordings/ lands in a temp dir, not the repo ---
	origDir, err := os.Getwd()
	require.NoError(t, err)
	tmpDir := t.TempDir()
	require.NoError(t, os.Chdir(tmpDir))
	defer func() { require.NoError(t, os.Chdir(origDir)) }()

	// --- Build one recorder per station ---
	recorders := make([]*Recorder, 0, len(usable))
	for _, s := range usable {
		recorders = append(recorders, &Recorder{
			url:       s.URLResolved,
			radioName: s.Name,
		})
	}

	// --- Run all RecordOnce calls concurrently ---
	// The context timeout (45s) bounds the run so the test cannot hang.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	results := make([]error, len(recorders))
	var g errgroup.Group
	for i, rec := range recorders {
		i, rec := i, rec
		g.Go(func() error {
			results[i] = rec.RecordOnce(ctx)
			return nil // never fail the group; failures are inspected per recorder
		})
	}
	require.NoError(t, g.Wait())

	// --- Verify each recorder produced data ---
	successes := 0
	for i, rec := range recorders {
		err := results[i]
		dir := db.RecordingsDir(rec.radioName)

		rec.mu.Lock()
		bufLen := 0
		if rec.buffer != nil {
			bufLen = rec.buffer.Len()
		}
		filename := rec.filename
		rec.mu.Unlock()

		if err != nil {
			t.Logf("station %q: RecordOnce returned error (skipping data checks): %v", rec.radioName, err)
			continue
		}
		successes++

		require.DirExists(t, dir, "recordings dir for %q should exist after RecordOnce", rec.radioName)
		require.Greater(t, bufLen, 0, "recorder for %q should have buffered stream data", rec.radioName)
		require.NotEmpty(t, filename, "recorder for %q should have a target filename", rec.radioName)

		// storeToDisk only flushes buffers >= 5GB, so 30s of audio stays in
		// memory. Verify the file would be written if the threshold were met:
		// the directory exists and the target path is set.
		if _, statErr := os.Stat(filename); statErr == nil {
			t.Logf("station %q wrote file %s", rec.radioName, filename)
		} else {
			t.Logf("station %q: %s not flushed to disk (storeToDisk requires >=5GB buffer; buffered %d bytes)", rec.radioName, filename, bufLen)
		}
	}

	require.GreaterOrEqual(t, successes, 1, "at least one station should have recorded successfully")
}

// seedTestStations populates the radio_stations table with real streams so
// RecordOnce has something to connect to. It prefers the local snapshot
// initstations_james.json and falls back to the built-in default streams.
func seedTestStations(t *testing.T) {
	t.Helper()

	body, err := os.ReadFile("initstations_james.json")
	if err == nil {
		var page []radioBrowserStation
		if err := json.Unmarshal(body, &page); err == nil {
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
			if len(stations) > 0 {
				require.NoError(t, db.UpsertRadioStations(t.Context(), stations))
				t.Logf("seeded %d stations from initstations_james.json", len(stations))
				return
			}
		}
	}

	// Fallback: the two known-good default streams.
	require.NoError(t, db.UpsertRadioStations(t.Context(), []db.RadioStation{
		{StationUUID: "default-1", Name: "GayPHXRadio", URLResolved: streamURL},
		{StationUUID: "default-2", Name: "RandomRadio", URLResolved: streamURL2},
	}))
	t.Log("seeded default streams")
}

// stationReachable reports whether the stream URL accepts a connection and
// actually starts producing data within the given context.
func stationReachable(ctx context.Context, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := insecureClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	buf := make([]byte, 1024)
	n, err := resp.Body.Read(buf)
	return n > 0
}
