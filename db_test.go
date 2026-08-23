package main

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

const testDBPath = "postgres://root@localhost:26257/gradio_test?sslmode=disable"

func TestContentIDs(t *testing.T) {
	// Recording ids are a deterministic, positive hash of path.
	r1 := recordingID("/radio/file.mp3")
	r2 := recordingID("/radio/file.mp3")
	require.Equal(t, r1, r2)
	require.Positive(t, r1)
	require.NotEqual(t, r1, recordingID("/radio/other.mp3"))

	// Split ids are a deterministic hash of path + start/end boundaries.
	s1 := splitID("/radio/file.mp3", 10.5, 30.25)
	s2 := splitID("/radio/file.mp3", 10.5, 30.25)
	require.Equal(t, s1, s2)
	require.Positive(t, s1)
	require.NotEqual(t, s1, splitID("/radio/file.mp3", 10.5, 30.26))
	require.NotEqual(t, s1, splitID("/radio/other.mp3", 10.5, 30.25))
}

func TestRecordingDBLifecycle(t *testing.T) {
	// Ensure the dedicated test database exists (connect to the default db to
	// create it, since CockroachDB won't create it implicitly).
	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	require.NoError(t, err)
	_, err = admin.ExecContext(t.Context(), `CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	// Point the package DB at the dedicated test database so we don't touch the
	// real recordings, then start from a clean slate.
	setRecordDBPath(testDBPath)
	CreateDBHandle()

	_, err = recordDB.ExecContext(t.Context(), `DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS play_history; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recording_splits; DROP TABLE IF EXISTS recordings; DROP TABLE IF EXISTS favorites; DROP TABLE IF EXISTS radio_stations;`)
	require.NoError(t, err)
	// Recreate the schema on the freshly-dropped tables.
	require.NoError(t, createSchema(t.Context(), recordDB))

	sourcePath := "/tmp/test-source-2026-08-18_00-00-00.mp3"
	radio := "TestRadio"
	recordedAt := time.Now().Add(-time.Hour)

	id, err := insertRecording(t.Context(), sourcePath, radio, recordedAt, 12345)
	require.NoError(t, err)
	require.NotZero(t, id)
	require.Equal(t, recordingID(sourcePath), id, "id is a hash of the source path")

	// Re-inserting the same file (same path and size) returns the same id
	// instead of creating a duplicate row.
	id2, err := insertRecording(t.Context(), sourcePath, radio, recordedAt, 12345)
	require.NoError(t, err)
	require.Equal(t, id, id2)

	// A re-recorded file with a different size maps to the same recording (id
	// is path-only).
	id3, err := insertRecording(t.Context(), sourcePath, radio, recordedAt, 99999)
	require.NoError(t, err)
	require.Equal(t, id, id3, "same source_path with different size must return same deterministic id")
	require.Equal(t, recordingID(sourcePath), id3)

	pending, err := fetchPendingRecordings(t.Context())
	require.NoError(t, err)

	found := false
	for _, r := range pending {
		if r.ID == id {
			found = true
			require.Equal(t, sourcePath, r.SourcePath)
			require.Equal(t, radio, r.Radio)
			require.Equal(t, RecordingStatus("pending"), r.Status)
		}
	}
	require.True(t, found, "pending recording should contain the inserted row")

	require.NoError(t, setRecordingStatus(t.Context(), id, StatusProcessing))
	require.NoError(t, setRecordingStatus(t.Context(), id, StatusProcessed))

	// A split linked to this recording.
	require.NoError(t, insertSplit(t.Context(), Split{
		RecordingID: id,
		SourcePath:  sourcePath,
		Index:       0,
		Start:       42.696,
		End:         274.092,
		OutputPath:  "/tmp/test-source/output_00001.mp3",
	}))

	splits, err := fetchSplitsForRecording(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, splits, 1)
	require.Equal(t, 0, splits[0].Index)
	require.InDelta(t, 42.696, splits[0].Start, 0.0001)
	require.InDelta(t, 274.092, splits[0].End, 0.0001)
	require.Empty(t, splits[0].Classification)

	all, err := fetchAllSplits(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, all)

	fetched, err := fetchSplit(t.Context(), splits[0].ID)
	require.NoError(t, err)
	require.Equal(t, splits[0].ID, fetched.ID)

	require.NoError(t, updateSplit(t.Context(), Split{
		ID:             splits[0].ID,
		RecordingID:    id,
		SourcePath:     sourcePath,
		Index:          0,
		Start:          100.5,
		End:            200.25,
		OutputPath:     "/tmp/test-source/output_00001.mp3",
		Classification: "track",
	}))

	updated, err := fetchSplit(t.Context(), splits[0].ID)
	require.NoError(t, err)
	require.InDelta(t, 100.5, updated.Start, 0.0001)
	require.InDelta(t, 200.25, updated.End, 0.0001)
	require.Equal(t, "track", updated.Classification)

	recs, err := fetchAllRecordings(t.Context())
	require.NoError(t, err)
	require.NotEmpty(t, recs)
}

// TestSongPlaysAndGlobalShuffle covers the song_plays table (play counts and
// ratings) and the global shuffle batch selection.
func TestSongPlaysAndGlobalShuffle(t *testing.T) {
	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	require.NoError(t, err)
	_, err = admin.ExecContext(t.Context(), `CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	setRecordDBPath(testDBPath)
	CreateDBHandle()

	_, err = recordDB.ExecContext(t.Context(), `DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS play_history; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recording_splits; DROP TABLE IF EXISTS recordings; DROP TABLE IF EXISTS favorites; DROP TABLE IF EXISTS radio_stations;`)
	require.NoError(t, err)
	require.NoError(t, createSchema(t.Context(), recordDB))

	recID, err := insertRecording(t.Context(), "/tmp/global-shuffle.mp3", "TestRadio", time.Now(), 123)
	require.NoError(t, err)

	splitIDs := make([]int64, 0, 4)
	for i, cls := range []string{"", "", classificationCommercial, ""} {
		s := Split{
			RecordingID:    recID,
			SourcePath:     "/tmp/global-shuffle.mp3",
			Index:          i,
			Start:          float64(i * 100),
			End:            float64(i*100 + 100),
			OutputPath:     fmt.Sprintf("split_music/TestRadio/gs/output_%05d.mp3", i),
			Classification: cls,
		}
		require.NoError(t, insertSplit(t.Context(), s))
		splits, err := fetchSplitsForRecording(t.Context(), recID)
		require.NoError(t, err)
		splitIDs = append(splitIDs, splits[len(splits)-1].ID)
	}
	require.Len(t, splitIDs, 4)

	// recordPlay increments the counter and creates the row on first play.
	require.NoError(t, recordPlay(t.Context(), splitIDs[0]))
	require.NoError(t, recordPlay(t.Context(), splitIDs[0]))

	plays, rating, err := fetchSongStats(t.Context(), splitIDs[0])
	require.NoError(t, err)
	require.Equal(t, 2, plays)
	require.Zero(t, rating)

	// A like increments the rating counter; a dislike decrements it. Play
	// counts are preserved.
	require.NoError(t, setRating(t.Context(), splitIDs[0], true))
	_, rating, err = fetchSongStats(t.Context(), splitIDs[0])
	require.NoError(t, err)
	require.Equal(t, 1, rating)

	require.NoError(t, setRating(t.Context(), splitIDs[0], true))
	_, rating, err = fetchSongStats(t.Context(), splitIDs[0])
	require.NoError(t, err)
	require.Equal(t, 2, rating)

	require.NoError(t, setRating(t.Context(), splitIDs[0], false))
	plays, rating, err = fetchSongStats(t.Context(), splitIDs[0])
	require.NoError(t, err)
	require.Equal(t, 2, plays, "a rating must not change the play count")
	require.Equal(t, 1, rating)

	// A split that was never played/rated reports zero stats.
	plays, rating, err = fetchSongStats(t.Context(), splitIDs[1])
	require.NoError(t, err)
	require.Zero(t, plays)
	require.Zero(t, rating)

	// Global shuffle skips commercials and returns everything else once.
	batch, err := fetchGlobalShuffleBatch(t.Context(), 100, nil)
	require.NoError(t, err)
	require.Len(t, batch, 3, "commercial split must be excluded")
	for _, s := range batch {
		require.NotEqual(t, classificationCommercial, s.Classification)
	}

	// Excluding already-played splits keeps them out of the next batch.
	excluded := []int64{splitIDs[1]}
	batch, err = fetchGlobalShuffleBatch(t.Context(), 100, excluded)
	require.NoError(t, err)
	require.Len(t, batch, 2)
	for _, s := range batch {
		require.NotContains(t, excluded, s.ID)
	}

	// The split with the most plays sorts after splits with fewer plays when
	// every other factor is equal: a batch of size 2 over the three non-commercial
	// splits must include the 2-play split only after the two 0-play ones.
	excluded = nil
	batch, err = fetchGlobalShuffleBatch(t.Context(), 2, nil)
	require.NoError(t, err)
	require.Len(t, batch, 2)
	for _, s := range batch {
		require.NotEqual(t, splitIDs[0], s.ID, "most-played split should sort last")
	}
}

// TestRadioStationDB covers the radio_stations table: bulk upsert, idempotent
// re-sync, and the name -> url_resolved lookup map.
func TestRadioStationDB(t *testing.T) {
	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	require.NoError(t, err)
	_, err = admin.ExecContext(t.Context(), `CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	setRecordDBPath(testDBPath)
	CreateDBHandle()

	_, err = recordDB.ExecContext(t.Context(), `DROP TABLE IF EXISTS favorites; DROP TABLE IF EXISTS radio_stations;`)
	require.NoError(t, err)
	require.NoError(t, createSchema(t.Context(), recordDB))

	// Upsert a batch of stations.
	require.NoError(t, upsertRadioStations(t.Context(), []RadioStation{
		{StationUUID: "uuid-1", Name: "Alpha", URLResolved: "https://a.example/stream", Favicon: "https://a.example/icon.png", Tags: "jazz,pop", CountryCode: "US", LanguageCodes: "eng"},
		{StationUUID: "uuid-2", Name: "Beta", URLResolved: "https://b.example/stream", Tags: "classical", CountryCode: "DE", LanguageCodes: "ger"},
		{StationUUID: "uuid-3", Name: "Gamma", URLResolved: "", CountryCode: "FR"}, // no resolved url: kept but excluded from map
	}))

	// Re-syncing the same uuid updates metadata in place.
	require.NoError(t, upsertRadioStations(t.Context(), []RadioStation{
		{StationUUID: "uuid-1", Name: "Alpha", URLResolved: "https://a.example/stream", Favicon: "https://a.example/new-icon.png", Tags: "jazz,pop,rock", CountryCode: "US", LanguageCodes: "eng"},
	}))

	stations, err := fetchRadioStations(t.Context())
	require.NoError(t, err)
	require.Len(t, stations, 3, "upsert must not create duplicate rows")

	urls, err := fetchRadioStationURLs(t.Context())
	require.NoError(t, err)
	require.Equal(t, "https://a.example/stream", urls["Alpha"])
	require.Equal(t, "https://b.example/stream", urls["Beta"])
	require.NotContains(t, urls, "Gamma", "stations without url_resolved are not recordable")
	require.Len(t, urls, 2)

	// An empty table makes radioURLs fall back to the built-in defaults.
	_, err = recordDB.ExecContext(t.Context(), `DELETE FROM radio_stations;`)
	require.NoError(t, err)
	require.Equal(t, defaultURLs(), radioURLs(t.Context()))
}
