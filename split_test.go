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

// buildSilenceFixture renders a short mp3 with three audible segments separated
// by silences, so silence detection (noise=-17.8dB, d=1.3) splits it into
// multiple files. The sine source alone peaks at ~-18dB (below the -17.8dB
// silence threshold), so it must be boosted to read as audible.
func buildSilenceFixture(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	fixture := filepath.Join(dir, name)
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
	return fixture
}

func TestSplitStreamSkipsExisting(t *testing.T) {
	fixture := buildSilenceFixture(t, "fixture.mp3")

	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	require.NoError(t, err)
	_, err = admin.ExecContext(t.Context(), `CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	setRecordDBPath(testDBPath)
	CreateDBHandle()

	_, err = recordDB.ExecContext(t.Context(), `DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS play_history; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recording_splits; DROP TABLE IF EXISTS recordings;`)
	require.NoError(t, err)
	require.NoError(t, createSchema(t.Context(), recordDB))

	// First run: fully split the file.
	require.NoError(t, SplitStream(t.Context(), fixture))

	rec, err := fetchRecordingByPath(t.Context(), fixture)
	require.NoError(t, err)
	require.NotZero(t, rec.ID)

	splits1, err := fetchSplitsForRecording(t.Context(), rec.ID)
	require.NoError(t, err)
	require.NotEmpty(t, splits1)

	// The recording is tracked as split with the folder its outputs live in.
	folder, done, err := recordingSplitFolder(t.Context(), rec.ID)
	require.NoError(t, err)
	require.True(t, done)
	require.DirExists(t, folder)
	require.Equal(t, filepath.Dir(splits1[0].OutputPath), folder)

	mtimes := make(map[string]time.Time, len(splits1))
	for _, s := range splits1 {
		require.Equal(t, classifySplit(s.Start, s.End), s.Classification, "split %d classification", s.Index)
		info, err := os.Stat(s.OutputPath)
		require.NoError(t, err)
		mtimes[s.OutputPath] = info.ModTime()
	}

	// Second run: reprocess the same file. Existing outputs must be skipped,
	// not re-encoded, and no duplicate split rows may be created. Because the
	// recording is tracked as split and its folder still exists, silence
	// detection is skipped entirely.
	require.NoError(t, SplitStream(t.Context(), fixture))

	splits2, err := fetchSplitsForRecording(t.Context(), rec.ID)
	require.NoError(t, err)
	require.Len(t, splits2, len(splits1))

	folder2, done2, err := recordingSplitFolder(t.Context(), rec.ID)
	require.NoError(t, err)
	require.True(t, done2)
	require.Equal(t, folder, folder2)

	for _, s := range splits2 {
		prev, ok := mtimes[s.OutputPath]
		require.True(t, ok, "split output missing from first run: %s", s.OutputPath)
		info, err := os.Stat(s.OutputPath)
		require.NoError(t, err)
		require.Equal(t, prev, info.ModTime(), "output file was re-split: %s", s.OutputPath)
	}
}

func TestSplitRecordingRerunsWhenFolderMissing(t *testing.T) {
	fixture := buildSilenceFixture(t, "rerun.mp3")

	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	require.NoError(t, err)
	_, err = admin.ExecContext(t.Context(), `CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	setRecordDBPath(testDBPath)
	CreateDBHandle()

	_, err = recordDB.ExecContext(t.Context(), `DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS play_history; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recording_splits; DROP TABLE IF EXISTS recordings;`)
	require.NoError(t, err)
	require.NoError(t, createSchema(t.Context(), recordDB))

	require.NoError(t, SplitStream(t.Context(), fixture))

	rec, err := fetchRecordingByPath(t.Context(), fixture)
	require.NoError(t, err)
	require.NotZero(t, rec.ID)

	folder, done, err := recordingSplitFolder(t.Context(), rec.ID)
	require.NoError(t, err)
	require.True(t, done)
	require.DirExists(t, folder)

	// Removing the output folder means the recording is no longer considered
	// split; silence detection must run again and repopulate the folder.
	require.NoError(t, os.RemoveAll(folder))
	require.NoDirExists(t, folder)

	require.NoError(t, SplitStream(t.Context(), fixture))
	require.DirExists(t, folder)

	folder2, done2, err := recordingSplitFolder(t.Context(), rec.ID)
	require.NoError(t, err)
	require.True(t, done2)
	require.Equal(t, folder, folder2)

	// Splits are still complete after the re-run.
	splits, err := fetchSplitsForRecording(t.Context(), rec.ID)
	require.NoError(t, err)
	require.NotEmpty(t, splits)
	for _, s := range splits {
		require.FileExists(t, s.OutputPath)
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
	_, err = admin.ExecContext(t.Context(), `CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	setRecordDBPath(testDBPath)
	CreateDBHandle()

	_, err = recordDB.ExecContext(t.Context(), `DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS play_history; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recording_splits; DROP TABLE IF EXISTS recordings;`)
	require.NoError(t, err)
	require.NoError(t, createSchema(t.Context(), recordDB))

	recID, err := insertRecording(t.Context(), fixture, "TestRadio", time.Now(), 123)
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
	require.NoError(t, insertSplit(t.Context(), orig))
	all, err := fetchSplitsForRecording(t.Context(), recID)
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

	fetched, err := fetchSplit(t.Context(), orig.ID)
	require.NoError(t, err)
	require.Equal(t, classificationReSplit, fetched.Classification)

	// The database now has three splits: the original plus the two new ones.
	all, err = fetchSplitsForRecording(t.Context(), recID)
	require.NoError(t, err)
	require.Len(t, all, 3)

	// A cut outside one of the new (non re_split) splits is rejected without
	// creating rows.
	allSplits, err := fetchSplitsForRecording(t.Context(), recID)
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
	all, err = fetchSplitsForRecording(t.Context(), recID)
	require.NoError(t, err)
	require.Len(t, all, 3)

	// Re-splitting an already re_split split is rejected.
	_, _, _, err = resplitSplit(t.Context(), orig.ID, 10)
	require.Error(t, err)

	// re_split splits are excluded from the global shuffle.
	batch, err := fetchGlobalShuffleBatch(t.Context(), 100, nil)
	require.NoError(t, err)
	for _, s := range batch {
		require.NotEqual(t, classificationReSplit, s.Classification)
	}
}

func TestMergeSplit(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "merge.mp3")
	// A continuous 60s tone (a real file so writeSegment can slice it).
	cmd := exec.Command("ffmpeg", "-hide_banner", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=60",
		"-c:a", "libmp3lame", "-b:a", "128k", fixture)
	require.NoError(t, cmd.Run())

	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	require.NoError(t, err)
	_, err = admin.ExecContext(t.Context(), `CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	setRecordDBPath(testDBPath)
	CreateDBHandle()

	_, err = recordDB.ExecContext(t.Context(), `DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS play_history; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recording_splits; DROP TABLE IF EXISTS recordings;`)
	require.NoError(t, err)
	require.NoError(t, createSchema(t.Context(), recordDB))

	recID, err := insertRecording(t.Context(), fixture, "TestRadio", time.Now(), 123)
	require.NoError(t, err)

	// Three adjacent splits spanning [0,20),[20,40),[40,60).
	require.NoError(t, insertSplit(t.Context(), Split{RecordingID: recID, SourcePath: fixture, Index: 0, Start: 0, End: 20, OutputPath: filepath.Join(dir, "a.mp3")}))
	require.NoError(t, insertSplit(t.Context(), Split{RecordingID: recID, SourcePath: fixture, Index: 1, Start: 20, End: 40, OutputPath: filepath.Join(dir, "b.mp3")}))
	require.NoError(t, insertSplit(t.Context(), Split{RecordingID: recID, SourcePath: fixture, Index: 2, Start: 40, End: 60, OutputPath: filepath.Join(dir, "c.mp3")}))

	splits, err := fetchSplitsForRecording(t.Context(), recID)
	require.NoError(t, err)
	require.Len(t, splits, 3)
	var middle Split
	for _, s := range splits {
		if s.Index == 1 {
			middle = s
		}
	}
	require.NotZero(t, middle.ID)

	// "End too soon": merge the middle split with the split after it.
	cur, adj, merged, err := mergeSplit(t.Context(), middle.ID, false)
	require.NoError(t, err)
	require.InDelta(t, 20, cur.Start, 0.001)
	require.InDelta(t, 40, cur.End, 0.001)
	require.InDelta(t, 40, adj.Start, 0.001)
	require.InDelta(t, 60, adj.End, 0.001)
	require.InDelta(t, 20, merged.Start, 0.001)
	require.InDelta(t, 60, merged.End, 0.001)

	// The merged output file exists with the combined duration.
	info, err := os.Stat(merged.OutputPath)
	require.NoError(t, err)
	require.Positive(t, info.Size())
	d, err := fileDuration(merged.OutputPath)
	require.NoError(t, err)
	require.InDelta(t, 40, d, 0.5)

	// Both source splits are now re_split.
	for _, id := range []int64{cur.ID, adj.ID} {
		f, err := fetchSplit(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, classificationReSplit, f.Classification)
	}

	// DB now has 4 splits total (3 originals + 1 merged).
	all, err := fetchSplitsForRecording(t.Context(), recID)
	require.NoError(t, err)
	require.Len(t, all, 4)

	// "Start too soon": the merged split spans [20,60), so there is no split
	// before it to join on; merging with the previous neighbor errors.
	var mergedID int64
	for _, s := range all {
		if s.Classification != classificationReSplit {
			mergedID = s.ID
			break
		}
	}
	require.NotZero(t, mergedID)
	_, _, _, err = mergeSplit(t.Context(), mergedID, true)
	require.ErrorIs(t, err, errNoAdjacentSplit)
}
