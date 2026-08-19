package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func init() {
	setRecordDBPath(filepath.Join(os.TempDir(), "gradio-test-recordings.db"))
}

func TestRecordingDBLifecycle(t *testing.T) {
	// Start from a clean database so the test is deterministic across runs.
	os.Remove(filepath.Join(os.TempDir(), "gradio-test-recordings.db"))

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
}
