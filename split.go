package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"
)

// silence is an interval of silence detected by ffmpeg's silencedetect filter.
type silence struct {
	Start float64
	End   float64
}

// boundary marks where one output file ends and the next begins. A split is
// never made in the middle of a silence: the previous file runs through the
// end of the detected silence (boundary.end) while the next file begins at
// the start of that same silence (boundary.start), so both files keep the
// whole silence instead of cutting through it.
type boundary struct {
	Start float64
	End   float64
}

const (
	silenceNoise    = "-17.8dB"
	silenceDuration = "1.3"

	// Classifications assigned to a split when it is first created. A split
	// under a minute is unlikely to be a full song; anything at or over a
	// minute is a probable song.
	classificationNotSong       = "not_song"
	classificationLikelySong    = "likely_song"
	classificationCommercial    = "commercial"
	classificationSong          = "song"
	classificationReSplit       = "re_split"
	classificationInformational = "informational"
	classificationNews          = "news"
)

// classificationOption pairs a classification value with the emoji + label
// shown in the UI dropdowns. The order here is the order the options appear.
type classificationOption struct {
	Value string
	Label string
}

// classificationOptions is the central list of every classification a split
// may carry, used by the UI dropdowns and for validation.
var classificationOptions = []classificationOption{
	{Value: classificationNotSong, Label: "⏭️ Not Song / Short clip"},
	{Value: classificationLikelySong, Label: "🎵 Likely Song"},
	{Value: classificationSong, Label: "🎶 Song"},
	{Value: classificationCommercial, Label: "📢 Commercial"},
	{Value: classificationInformational, Label: "ℹ️ Informational"},
	{Value: classificationNews, Label: "📰 News"},
	{Value: classificationReSplit, Label: "✂️ Re-split"},
}

// validClassifications is the set of classification values a split may carry.
var validClassifications = func() map[string]struct{} {
	m := make(map[string]struct{}, len(classificationOptions))
	for _, o := range classificationOptions {
		m[o.Value] = struct{}{}
	}
	return m
}()

// isValidClassification reports whether cls is one of the known classifications.
func isValidClassification(cls string) bool {
	_, ok := validClassifications[cls]
	return ok
}

// clsLabel returns the emoji + description label for a classification value.
func clsLabel(cls string) string {
	for _, o := range classificationOptions {
		if o.Value == cls {
			return o.Label
		}
	}
	return cls
}

// errCutOutsideSplit is returned when a resplit cut time falls outside a
// split's boundaries.
var errCutOutsideSplit = errors.New("cut time is outside the split")

// errNoAdjacentSplit is returned when a split has no neighbor to merge with
// in its source recording.
var errNoAdjacentSplit = errors.New("no adjacent split to merge with")

var (
	silenceStartRE = regexp.MustCompile(`silence_start:\s*([0-9.]+)`)
	silenceEndRE   = regexp.MustCompile(`silence_end:\s*([0-9.]+)`)
)

var (
	memoizeDB     *sql.DB
	memoizeDBOnce sync.Once
)

func memoizeDBHandle() *sql.DB {
	memoizeDBOnce.Do(func() {
		db, err := sql.Open("pgx", defaultDBPath)
		if err != nil {
			slog.Error("memoize: open db", "err", err)
			os.Exit(1)
			return
		}

		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS memoize (
			op     STRING NOT NULL,
			input  STRING NOT NULL,
			result STRING NOT NULL,
			PRIMARY KEY (op, input)
		)`); err != nil {
			slog.Error("memoize: create table", "err", err)
			os.Exit(1)
			return
		}

		memoizeDB = db
	})

	return memoizeDB
}

// Memoize runs f on the inputPath. The result is stored in a sqlite database
// keyed by the function name and the input path. When Memoize is called again
// with the same op and input, the stored result is returned instead of
// running f. If the database is not usable, f is run and the error is logged.
func Memoize[Val any, op func(context.Context, string) (Val, error)](ctx context.Context, inputPath string, f op) (Val, error) {
	var result Val

	// db := memoizeDBHandle()
	if memoizeDB != nil {
		var data string
		err := memoizeDB.QueryRow(
			`SELECT result FROM memoize WHERE op = $1 AND input = $2`,
			opName(f), inputPath,
		).Scan(&data)
		switch {
		case err == sql.ErrNoRows:
			// No cached result, run f below.
		case err != nil:
			slog.Error("memoize: query", "err", err)
		default:
			if err := json.Unmarshal([]byte(data), &result); err != nil {
				slog.Error("memoize: unmarshal", "err", err)
			} else {
				return result, nil
			}
		}
	}

	result, err := f(ctx, inputPath)
	if err != nil {
		return result, err
	}

	if memoizeDB != nil {
		data, err := json.Marshal(result)
		if err != nil {
			slog.Error("memoize: marshal", "err", err)
			return result, nil
		}

		if _, err := memoizeDB.Exec(
			`INSERT INTO memoize (op, input, result) VALUES ($1, $2, $3)
			 ON CONFLICT(op, input) DO UPDATE SET result = excluded.result`,
			opName(f), inputPath, string(data),
		); err != nil {
			slog.Error("memoize: store", "err", err)
		}
	}

	return result, nil
}

func opName(f any) string {
	return runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
}

func SplitStream(ctx context.Context, fname string) error {
	info, err := os.Stat(fname)
	if err != nil {
		return err
	}
	radio := filepath.Base(filepath.Dir(fname))
	if radio == "." || radio == "" {
		radio = "manual"
	}
	// When the file lives under a hashed recordings directory, resolve the hash
	// back to the original station name so the DB stores the display name.
	radio = radioDisplayName(radio)
	id, err := insertRecording(fname, radio, info.ModTime(), info.Size())
	if err != nil {
		return err
	}
	rec := Recording{
		ID:         id,
		SourcePath: fname,
		Radio:      radio,
		RecordedAt: info.ModTime(),
		SizeBytes:  info.Size(),
		Status:     StatusProcessed,
	}

	err = setRecordingStatus(id, StatusProcessing)
	if err != nil {
		return fmt.Errorf("failed to set recording status: %w", err)
	}

	err = splitRecording(ctx, rec)
	if err != nil {
		return fmt.Errorf("failed to split recording: %w", err)
	}
	err = setRecordingStatus(id, StatusProcessed)
	if err != nil {
		return fmt.Errorf("failed to store recording status: %w", err)
	}
	return nil
}

// splitRecording splits a single source recording and stores each resulting
// output file, its cutoffs, and its position in the original stream, in the
// splits table.
func splitRecording(ctx context.Context, rec Recording) error {
	wd, _ := os.Getwd()
	if !filepath.IsAbs(rec.SourcePath) {
		rec.SourcePath = filepath.Join(wd, rec.SourcePath)
	}

	// If this recording was fully split in a previous run and its output
	// folder is still on disk, skip the expensive silence-detection pass
	// entirely. The recording_splits row is written only after every split
	// file has been created and stored, so its presence means the splits table
	// is complete.
	if folder, done, err := recordingSplitFolder(rec.ID); err != nil {
		return fmt.Errorf("check recording split: %w", err)
	} else if done {
		if _, err := os.Stat(folder); err == nil {
			slog.InfoContext(ctx, "recording already split, skipping silence detection", "recording", rec.ID, "folder", folder)
			return nil
		}
		slog.InfoContext(ctx, "recording split folder missing, re-splitting", "recording", rec.ID, "folder", folder)
	}

	existing, err := fetchSplitsForRecording(rec.ID)
	if err != nil {
		return fmt.Errorf("fetch existing splits for recording %d: %w", rec.ID, err)
	}
	existingByIndex := make(map[int]Split, len(existing))
	for _, s := range existing {
		existingByIndex[s.Index] = s
	}

	i := 0

	silences, err := detectSilence(ctx, rec.SourcePath)
	if err != nil {
		return fmt.Errorf("detect silence in %s: %w", rec.SourcePath, err)
	}

	boundaries := chooseSplitBoundaries(silences)

	g := errgroup.Group{}
	g.SetLimit(100)
	for b := range boundaries {
		idx := i
		i++
		g.Go(func() error {
			outputPath := splitOutputPath(rec.Radio, rec.SourcePath, idx)

			if _, err := os.Stat(outputPath); err == nil {
				expected := b.End - b.Start
				actual, derr := fileDuration(outputPath)
				if derr != nil {
					slog.WarnContext(ctx, "measure existing split duration", "err", derr, "output", outputPath)
				}
				match := durationsMatch(expected, actual)

				slog.InfoContext(ctx,
					"skipping split, output file already exists",
					"recording", rec.ID,
					"index", idx,
					"output", outputPath,
					"existing_duration_seconds", actual,
					"expected_duration_seconds", expected,
					"duration_matches", match,
				)

				// The row may be missing if a previous run was interrupted
				// between writing the file and storing the split.
				if _, ok := existingByIndex[idx]; !ok {
					if err := insertSplit(Split{
						RecordingID:    rec.ID,
						SourcePath:     rec.SourcePath,
						Index:          idx,
						Start:          b.Start,
						End:            b.End,
						OutputPath:     outputPath,
						Classification: classifySplit(b.Start, b.End),
					}); err != nil {
						slog.ErrorContext(ctx, "store skipped split", "err", err, "output", outputPath)
					}
				}
				return nil
			}

			outputPath, err := writeSegment(ctx, rec.Radio, rec.SourcePath, b.Start, b.End, idx)
			if err != nil {
				slog.ErrorContext(ctx, "Boundary failed split", "err", err)
				return nil
			}

			err = insertSplit(Split{
				RecordingID:    rec.ID,
				SourcePath:     rec.SourcePath,
				Index:          idx,
				Start:          b.Start,
				End:            b.End,
				OutputPath:     outputPath,
				Classification: classifySplit(b.Start, b.End),
			})
			if err != nil {
				slog.ErrorContext(ctx, "store split", "err", err, "output", outputPath)
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}

	return markRecordingSplit(rec.ID, splitOutputDir(rec.Radio, rec.SourcePath))
}

// nextSplitPositions returns n positions not yet used by any split of a
// recording, so newly created splits never collide with existing ones.
func nextSplitPositions(recordingID int64, n int) ([]int, error) {
	if recordDB == nil {
		return nil, fmt.Errorf("nil db")
	}

	var maxPos sql.NullInt64
	if err := recordDB.QueryRow(
		`SELECT MAX(position) FROM splits WHERE recording_id = $1`,
		recordingID,
	).Scan(&maxPos); err != nil {
		return nil, err
	}

	pos := make([]int, n)
	next := int(maxPos.Int64)
	for i := range pos {
		next++
		pos[i] = next
	}
	return pos, nil
}

// resplitSplit replaces a split with two finer splits cut at the given time
// within the original recording. The original split's boundaries and output
// file are left untouched; only its classification changes to re_split. Both
// new splits get their own output files, extracted from the original recording
// (not from the existing split file) so the new cut point is exact.
func resplitSplit(ctx context.Context, splitID int64, cut float64) (original Split, splitA Split, splitB Split, err error) {
	orig, err := fetchSplit(splitID)
	if err != nil {
		return Split{}, Split{}, Split{}, err
	}
	if cut <= orig.Start || cut >= orig.End {
		return Split{}, Split{}, Split{}, fmt.Errorf("%w: cut %v outside [%v, %v]", errCutOutsideSplit, cut, orig.Start, orig.End)
	}
	if orig.Classification == classificationReSplit {
		return Split{}, Split{}, Split{}, fmt.Errorf("split %d is already re_split", splitID)
	}

	wd, _ := os.Getwd()
	inputPath := orig.SourcePath
	if !filepath.IsAbs(inputPath) {
		inputPath = filepath.Join(wd, inputPath)
	}

	positions, err := nextSplitPositions(orig.RecordingID, 2)
	if err != nil {
		return Split{}, Split{}, Split{}, fmt.Errorf("allocate split positions: %w", err)
	}

	radio := radioFromPath(orig.SourcePath)
	// Prefer the original station name from the recordings table: the source
	// path's directory is a hash, not the display name.
	if rec, err := fetchRecordingByID(orig.RecordingID); err == nil {
		radio = rec.Radio
	}

	outA, err := writeSegment(ctx, radio, inputPath, orig.Start, cut, positions[0])
	if err != nil {
		return Split{}, Split{}, Split{}, fmt.Errorf("write first segment: %w", err)
	}
	outB, err := writeSegment(ctx, radio, inputPath, cut, orig.End, positions[1])
	if err != nil {
		return Split{}, Split{}, Split{}, fmt.Errorf("write second segment: %w", err)
	}

	a := Split{
		RecordingID:    orig.RecordingID,
		SourcePath:     orig.SourcePath,
		Index:          positions[0],
		Start:          orig.Start,
		End:            cut,
		OutputPath:     outA,
		Classification: orig.Classification,
	}
	b := Split{
		RecordingID:    orig.RecordingID,
		SourcePath:     orig.SourcePath,
		Index:          positions[1],
		Start:          cut,
		End:            orig.End,
		OutputPath:     outB,
		Classification: orig.Classification,
	}

	if err := insertSplit(a); err != nil {
		return Split{}, Split{}, Split{}, fmt.Errorf("store first split: %w", err)
	}
	if err := insertSplit(b); err != nil {
		return Split{}, Split{}, Split{}, fmt.Errorf("store second split: %w", err)
	}

	// Mark the original only after the new splits exist so a failure leaves it
	// fully playable.
	orig.Classification = classificationReSplit
	if err := updateSplit(orig); err != nil {
		return Split{}, Split{}, Split{}, fmt.Errorf("mark split re_split: %w", err)
	}

	return orig, a, b, nil
}

// boundaryMatch reports whether two stream positions are close enough to be
// the same boundary between adjacent splits (accounting for float rounding
// from re-encoding).
func boundaryMatch(a, b float64) bool {
	return math.Abs(a-b) < 0.01
}

// mergeSplit joins the given split with the split that comes immediately
// before (prev=true) or after (prev=false) it in the same source recording,
// then stores a single merged split in their place. Both source splits are
// marked re_split. The merged output is extracted from the original recording
// (not from either source file) so the combined boundaries are exact.
//
// A song whose end was cut too soon should be merged with the split that came
// after it (prev=false); a song whose start was cut too soon should be merged
// with the split that came before it (prev=true).
func mergeSplit(ctx context.Context, splitID int64, prev bool) (current Split, other Split, merged Split, err error) {
	cur, err := fetchSplit(splitID)
	if err != nil {
		return Split{}, Split{}, Split{}, err
	}
	if cur.Classification == classificationReSplit {
		return Split{}, Split{}, Split{}, fmt.Errorf("split %d is already re_split", splitID)
	}

	splits, err := fetchSplitsForRecording(cur.RecordingID)
	if err != nil {
		return Split{}, Split{}, Split{}, fmt.Errorf("fetch recording splits: %w", err)
	}

	// Find the adjacent split by matching its boundary to the current one.
	var adj Split
	found := false
	for _, s := range splits {
		if s.ID == cur.ID {
			continue
		}
		if prev && boundaryMatch(s.End, cur.Start) {
			adj = s
			found = true
			break
		}
		if !prev && boundaryMatch(s.Start, cur.End) {
			adj = s
			found = true
			break
		}
	}
	if !found {
		dir := "next"
		if prev {
			dir = "previous"
		}
		return Split{}, Split{}, Split{}, fmt.Errorf("%w: split %d has no %s neighbor", errNoAdjacentSplit, splitID, dir)
	}
	if adj.Classification == classificationReSplit {
		return Split{}, Split{}, Split{}, fmt.Errorf("adjacent split %d is already re_split", adj.ID)
	}

	var start, end float64
	if prev {
		start, end = adj.Start, cur.End
	} else {
		start, end = cur.Start, adj.End
	}

	wd, _ := os.Getwd()
	inputPath := cur.SourcePath
	if !filepath.IsAbs(inputPath) {
		inputPath = filepath.Join(wd, inputPath)
	}

	positions, err := nextSplitPositions(cur.RecordingID, 1)
	if err != nil {
		return Split{}, Split{}, Split{}, fmt.Errorf("allocate split position: %w", err)
	}

	radio := radioFromPath(cur.SourcePath)
	// Prefer the original station name from the recordings table: the source
	// path's directory is a hash, not the display name.
	if rec, err := fetchRecordingByID(cur.RecordingID); err == nil {
		radio = rec.Radio
	}

	out, err := writeSegment(ctx, radio, inputPath, start, end, positions[0])
	if err != nil {
		return Split{}, Split{}, Split{}, fmt.Errorf("write merged segment: %w", err)
	}

	merged = Split{
		RecordingID:    cur.RecordingID,
		SourcePath:     cur.SourcePath,
		Index:          positions[0],
		Start:          start,
		End:            end,
		OutputPath:     out,
		Classification: cur.Classification,
	}
	if err := insertSplit(merged); err != nil {
		return Split{}, Split{}, Split{}, fmt.Errorf("store merged split: %w", err)
	}

	// Mark both source splits re_split only after the merged split exists so a
	// failure leaves them fully playable.
	cur.Classification = classificationReSplit
	if err := updateSplit(cur); err != nil {
		return Split{}, Split{}, Split{}, fmt.Errorf("mark split re_split: %w", err)
	}
	adj.Classification = classificationReSplit
	if err := updateSplit(adj); err != nil {
		return Split{}, Split{}, Split{}, fmt.Errorf("mark adjacent split re_split: %w", err)
	}

	return cur, adj, merged, nil
}

func detectSilence(ctx context.Context, inputPath string) (chan silence, error) {
	cmd := exec.CommandContext(
		ctx,
		"ffmpeg",
		"-hide_banner",
		"-i", inputPath,
		"-af", fmt.Sprintf(
			"silencedetect=noise=%s:d=%s",
			silenceNoise,
			silenceDuration,
		),
		"-f", "null",
		"-",
	)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}

	silences := make(chan silence)
	go func() {
		var currentStart *float64
		defer close(silences)

		scanner := bufio.NewScanner(stderr)
		var lastLine string
		for scanner.Scan() {
			line := scanner.Text()
			lastLine = line
			if m := silenceStartRE.FindStringSubmatch(line); m != nil {
				start, err := strconv.ParseFloat(m[1], 64)
				if err != nil {
					continue
				}
				currentStart = &start
				continue
			}

			if m := silenceEndRE.FindStringSubmatch(line); m != nil && currentStart != nil {
				end, err := strconv.ParseFloat(m[1], 64)
				if err != nil {
					continue
				}
				silences <- silence{Start: *currentStart, End: end}
				currentStart = nil
			}
		}

		if err := scanner.Err(); err != nil {
			slog.Error("scan ffmpeg", "stderr", err)
		}

		if err := cmd.Wait(); err != nil {
			slog.Error("silence detection errored", "err", err, "lastLine", lastLine, "inputPath", inputPath)
		}
	}()

	return silences, nil
}

// chooseSplitBoundaries turns every detected silence into a boundary. A
// boundary is only made on a silence, so a clip never ends or begins while
// the audio is still playing: the previous clip ends at the end of the
// silence and the next clip begins at the start of that same silence.
func chooseSplitBoundaries(silences chan silence) chan boundary {
	boundaries := make(chan boundary)
	lastSilence := <-silences
	first := true
	go func() {
		defer close(boundaries)

		for silence := range silences {
			if !first && silence.End-lastSilence.Start < 3 {
				// lastSilence = silence
				continue
			}
			if silence.End-lastSilence.Start < 30 {
				lastSilence = silence
				continue
			}
			boundaries <- boundary{Start: lastSilence.Start, End: silence.End}
			lastSilence = silence
		}
	}()

	return boundaries
}

// classifySplit assigns the initial classification for a newly created split
// based on its duration in the original stream.
func classifySplit(start, end float64) string {
	if end-start < 60 {
		return classificationNotSong
	}
	return classificationLikelySong
}

// splitOutputDir returns the directory a source recording's split output files
// are written to: one folder per source file, nested under its radio. The
// radio segment is the hash of the station name so names with slashes or other
// path-hostile characters never break the output path.
func splitOutputDir(radio, inputPath string) string {
	return filepath.Join("split_music", RadioHash(radio), strings.Split(filepath.Base(inputPath), ".")[0])
}

// splitOutputPath returns the deterministic path a split's output file will
// be written to for a given source recording and stream position.
func splitOutputPath(radio, inputPath string, index int) string {
	return filepath.Join(splitOutputDir(radio, inputPath), fmt.Sprintf("output_%05d.mp3", index))
}

func writeSegment(ctx context.Context, radio string, inputPath string, start, end float64, index int) (string, error) {
	outputPath := splitOutputPath(radio, inputPath, index)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return "", fmt.Errorf("create output dir %s: %w", filepath.Dir(outputPath), err)
	}

	cmd := exec.CommandContext(
		ctx,
		"ffmpeg",
		"-hide_banner",
		"-y",
		"-ss", formatTime(start),
		"-to", formatTime(end),
		"-i", inputPath,
		"-map", "0:a:0",
		"-c:a", "libmp3lame",
		"-b:a", "196k",
		outputPath,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("create %s: %w", outputPath, err)
	}

	return outputPath, nil
}

// fileDuration returns the length in seconds of an audio file using ffprobe.
func fileDuration(path string) (float64, error) {
	cmd := exec.Command(
		"ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe %s: %w", path, err)
	}

	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0, fmt.Errorf("parse duration %q: %w", out, err)
	}

	return d, nil
}

// durationsMatch reports whether the measured duration of an existing output
// file is close enough to the duration of the split it represents (allowing
// for encoder frame alignment).
func durationsMatch(expected, actual float64) bool {
	if actual <= 0 {
		return false
	}
	return math.Abs(expected-actual) < 1.0
}

func formatTime(t float64) string {
	return strconv.FormatFloat(t, 'f', 3, 64)
}
