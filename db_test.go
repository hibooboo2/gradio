package main

import (
	"database/sql"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

const testDBPath = "postgres://root@localhost:26257/gradio_test?sslmode=disable"

func TestRecordingDBLifecycle(t *testing.T) {
	// Ensure the dedicated test database exists (connect to the default db to
	// create it, since CockroachDB won't create it implicitly).
	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	require.NoError(t, err)
	_, err = admin.Exec(`CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	// Point the package DB at the dedicated test database so we don't touch the
	// real recordings, then start from a clean slate.
	setRecordDBPath(testDBPath)
	CreateDBHandle()

	_, err = recordDB.Exec(`DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recordings;`)
	require.NoError(t, err)
	// Recreate the schema on the freshly-dropped tables.
	require.NoError(t, createSchema(recordDB))

	sourcePath := "/tmp/test-source-2026-08-18_00-00-00.mp3"
	radio := "TestRadio"
	recordedAt := time.Now().Add(-time.Hour)

	id, err := insertRecording(sourcePath, radio, recordedAt, 12345)
	require.NoError(t, err)
	require.NotZero(t, id)

	// Inserting the same source again should not create a duplicate row.
	id2, err := insertRecording(sourcePath, radio, recordedAt, 99999)
	require.NoError(t, err)
	require.Zero(t, id2)

	pending, err := fetchPendingRecordings()
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

	require.NoError(t, setRecordingStatus(id, StatusProcessing))
	require.NoError(t, setRecordingStatus(id, StatusProcessed))

	// A split linked to this recording.
	require.NoError(t, insertSplit(Split{
		RecordingID: id,
		SourcePath:  sourcePath,
		Index:       0,
		Start:       42.696,
		End:         274.092,
		OutputPath:  "/tmp/test-source/output_00001.mp3",
	}))

	splits, err := fetchSplitsForRecording(id)
	require.NoError(t, err)
	require.Len(t, splits, 1)
	require.Equal(t, 0, splits[0].Index)
	require.InDelta(t, 42.696, splits[0].Start, 0.0001)
	require.InDelta(t, 274.092, splits[0].End, 0.0001)
	require.Empty(t, splits[0].Classification)

	all, err := fetchAllSplits()
	require.NoError(t, err)
	require.NotEmpty(t, all)

	fetched, err := fetchSplit(splits[0].ID)
	require.NoError(t, err)
	require.Equal(t, splits[0].ID, fetched.ID)

	require.NoError(t, updateSplit(Split{
		ID:             splits[0].ID,
		RecordingID:    id,
		SourcePath:     sourcePath,
		Index:          0,
		Start:          100.5,
		End:            200.25,
		OutputPath:     "/tmp/test-source/output_00001.mp3",
		Classification: "track",
	}))

	updated, err := fetchSplit(splits[0].ID)
	require.NoError(t, err)
	require.InDelta(t, 100.5, updated.Start, 0.0001)
	require.InDelta(t, 200.25, updated.End, 0.0001)
	require.Equal(t, "track", updated.Classification)

	recs, err := fetchAllRecordings()
	require.NoError(t, err)
	require.NotEmpty(t, recs)
}
