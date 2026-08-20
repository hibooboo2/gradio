package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

func TestSplitStreamSkipsExisting(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.mp3")
	// Build a 75s source: 3x (15s tone + 10s silence). Silence detection
	// (noise=-17.8dB, d=1.3) splits at each silent gap, so multiple splits.
	// The sine source alone peaks at ~-18dB (below the -17.8dB silence
	// threshold), so it must be boosted to read as audible.
	inputs := []string{"-f", "lavfi", "-i", "sine=frequency=440:duration=15", "-f", "lavfi", "-i", "anullsrc=channel_layout=mono:sample_rate=44100"}
	segParts := make([]string, 0, 6)
	for i := 0; i < 3; i++ {
		segParts = append(segParts,
			fmt.Sprintf("[0:a]volume=10dB,atrim=end=15,asetpts=PTS-STARTPTS[t%d]", i),
			fmt.Sprintf("[1:a]atrim=end=10,asetpts=PTS-STARTPTS[s%d]", i),
		)
	}
	concatIn := ""
	for i := 0; i < 3; i++ {
		concatIn += fmt.Sprintf("[t%d][s%d]", i, i)
	}
	filter := strings.Join(segParts, ";") + ";" + concatIn + "concat=n=6:v=0:a=1[out]"

	args := append(append([]string{"-hide_banner", "-y"}, inputs...),
		"-filter_complex", filter, "-map", "[out]", "-c:a", "libmp3lame", "-b:a", "128k", fixture)
	cmd := exec.Command("ffmpeg", args...)
	require.NoError(t, cmd.Run())

	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	require.NoError(t, err)
	_, err = admin.Exec(`CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	setRecordDBPath(testDBPath)
	CreateDBHandle()

	_, err = recordDB.Exec(`DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recordings;`)
	require.NoError(t, err)
	require.NoError(t, createSchema(recordDB))

	// First run: fully split the file.
	require.NoError(t, SplitStream(t.Context(), fixture))

	rec, err := fetchRecordingByPath(fixture)
	require.NoError(t, err)
	require.NotZero(t, rec.ID)

	splits1, err := fetchSplitsForRecording(rec.ID)
	require.NoError(t, err)
	require.NotEmpty(t, splits1)

	mtimes := make(map[string]time.Time, len(splits1))
	for _, s := range splits1 {
		require.Equal(t, classifySplit(s.Start, s.End), s.Classification, "split %d classification", s.Index)
		info, err := os.Stat(s.OutputPath)
		require.NoError(t, err)
		mtimes[s.OutputPath] = info.ModTime()
	}

	// Second run: reprocess the same file. Existing outputs must be skipped,
	// not re-encoded, and no duplicate split rows may be created.
	require.NoError(t, SplitStream(t.Context(), fixture))

	splits2, err := fetchSplitsForRecording(rec.ID)
	require.NoError(t, err)
	require.Len(t, splits2, len(splits1))

	for _, s := range splits2 {
		prev, ok := mtimes[s.OutputPath]
		require.True(t, ok, "split output missing from first run: %s", s.OutputPath)
		info, err := os.Stat(s.OutputPath)
		require.NoError(t, err)
		require.Equal(t, prev, info.ModTime(), "output file was re-split: %s", s.OutputPath)
	}
}

func TestClassifySplit(t *testing.T) {
	require.Equal(t, classificationNotSong, classifySplit(0, 59.9))
	require.Equal(t, classificationNotSong, classifySplit(10, 69))
	require.Equal(t, classificationLikelySong, classifySplit(0, 60))
	require.Equal(t, classificationLikelySong, classifySplit(100, 300))
}

func TestResplitSplit(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "resplit.mp3")
	// A continuous 40s tone (a real file so writeSegment can slice it).
	cmd := exec.Command("ffmpeg", "-hide_banner", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=40",
		"-c:a", "libmp3lame", "-b:a", "128k", fixture)
	require.NoError(t, cmd.Run())

	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	require.NoError(t, err)
	_, err = admin.Exec(`CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	setRecordDBPath(testDBPath)
	CreateDBHandle()

	_, err = recordDB.Exec(`DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recordings;`)
	require.NoError(t, err)
	require.NoError(t, createSchema(recordDB))

	recID, err := insertRecording(fixture, "TestRadio", time.Now(), 123)
	require.NoError(t, err)
	orig := Split{
		RecordingID: recID,
		SourcePath:  fixture,
		Index:       0,
		Start:       0,
		End:         40,
		OutputPath:  filepath.Join(dir, "output_00000.mp3"),
	}
	// Create the original output file so the "original is untouched" check has
	// something to compare against.
	cmd = exec.Command("ffmpeg", "-hide_banner", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=40",
		"-c:a", "libmp3lame", "-b:a", "128k", orig.OutputPath)
	require.NoError(t, cmd.Run())
	require.NoError(t, insertSplit(orig))
	all, err := fetchSplitsForRecording(recID)
	require.NoError(t, err)
	require.Len(t, all, 1)
	orig = all[0]

	origPath := orig.OutputPath
	origInfo, err := os.Stat(origPath)
	require.NoError(t, err)
	origSize := origInfo.Size()

	original, a, b, err := resplitSplit(t.Context(), orig.ID, 20)
	require.NoError(t, err)
	require.Equal(t, classificationReSplit, original.Classification)
	require.InDelta(t, orig.Start, a.Start, 0.001)
	require.InDelta(t, 20, a.End, 0.001)
	require.InDelta(t, 20, b.Start, 0.001)
	require.InDelta(t, orig.End, b.End, 0.001)

	// Both new splits produced their own files from the original recording.
	for _, s := range []Split{a, b} {
		info, err := os.Stat(s.OutputPath)
		require.NoError(t, err)
		require.Positive(t, info.Size())
		require.NotEqual(t, origPath, s.OutputPath)
		d, err := fileDuration(s.OutputPath)
		require.NoError(t, err)
		require.InDelta(t, s.End-s.Start, d, 0.5)
	}

	// The original file is untouched, and its row is now re_split.
	afterInfo, err := os.Stat(origPath)
	require.NoError(t, err)
	require.Equal(t, origSize, afterInfo.Size())

	fetched, err := fetchSplit(orig.ID)
	require.NoError(t, err)
	require.Equal(t, classificationReSplit, fetched.Classification)

	// The database now has three splits: the original plus the two new ones.
	all, err = fetchSplitsForRecording(recID)
	require.NoError(t, err)
	require.Len(t, all, 3)

	// A cut outside one of the new (non re_split) splits is rejected without
	// creating rows.
	allSplits, err := fetchSplitsForRecording(recID)
	require.NoError(t, err)
	require.Len(t, allSplits, 3)
	var aID int64
	for _, s := range allSplits {
		if s.Classification != classificationReSplit {
			aID = s.ID
			break
		}
	}
	require.NotZero(t, aID)
	_, _, _, err = resplitSplit(t.Context(), aID, -1)
	require.ErrorIs(t, err, errCutOutsideSplit)
	all, err = fetchSplitsForRecording(recID)
	require.NoError(t, err)
	require.Len(t, all, 3)

	// Re-splitting an already re_split split is rejected.
	_, _, _, err = resplitSplit(t.Context(), orig.ID, 10)
	require.Error(t, err)

	// re_split splits are excluded from the global shuffle.
	batch, err := fetchGlobalShuffleBatch(100, nil)
	require.NoError(t, err)
	for _, s := range batch {
		require.NotEqual(t, classificationReSplit, s.Classification)
	}
}
