package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	_ "modernc.org/sqlite"
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
)

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
		db, err := sql.Open("sqlite", "file:/home/wizardofmath/go/src/github.com/hibooboo2/gradio/memoize.db?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
		if err != nil {
			log.Fatalf("memoize: open db: %v", err)
			return
		}

		if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS memoize (
			op     TEXT NOT NULL,
			input  TEXT NOT NULL,
			result TEXT NOT NULL,
			PRIMARY KEY (op, input)
		)`); err != nil {
			log.Fatalf("memoize: create table: %v", err)
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
			`SELECT result FROM memoize WHERE op = ? AND input = ?`,
			opName(f), inputPath,
		).Scan(&data)
		switch {
		case err == sql.ErrNoRows:
			// No cached result, run f below.
		case err != nil:
			log.Printf("memoize: query: %v", err)
		default:
			if err := json.Unmarshal([]byte(data), &result); err != nil {
				log.Printf("memoize: unmarshal: %v", err)
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
			log.Printf("memoize: marshal: %v", err)
			return result, nil
		}

		if _, err := memoizeDB.Exec(
			`INSERT INTO memoize (op, input, result) VALUES (?, ?, ?)
			 ON CONFLICT(op, input) DO UPDATE SET result = excluded.result`,
			opName(f), inputPath, string(data),
		); err != nil {
			log.Printf("memoize: store: %v", err)
		}
	}

	return result, nil
}

func opName(f any) string {
	return runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
}

func SplitStream(ctx context.Context, fname string) error {

	silences, err := detectSilence(ctx, fname)
	if err != nil {
		return fmt.Errorf("detect silence in %s: %w", fname, err)
	}

	boundaries := chooseSplitBoundaries(silences)

	f, err := os.Create(fmt.Sprintf("cutoffs_%s.txt", strings.TrimSuffix(filepath.Base(fname), filepath.Ext(fname))))
	if err != nil {
		return fmt.Errorf("create cutoffs file: %w", err)
	}

	w := csv.NewWriter(f)
	w.Comma = '\t'
	i := 0

	g := errgroup.Group{}
	g.SetLimit(4)
	for b := range boundaries {
		err = w.Write([]string{fmt.Sprintf("%.5f", b.Start), fmt.Sprintf("%.5f", b.End), fmt.Sprintf("Audio %d(%s)", i, time.Second*time.Duration(b.End-b.Start))})
		if err != nil {
			return fmt.Errorf("write cutoffs file: %w", err)
		}
		i++
		w.Flush()
		// slog.InfoContext(ctx, "Got boundary", "fname", fname, "id", i, "length_in_seconds", b.End-b.Start)
		// boundariesSlice = append(boundariesSlice, b)

		g.Go(func() error {
			err = splitAtBoundaries(ctx, fname, b, i)
			if err != nil {
				slog.ErrorContext(ctx, "Boundary failed split", "err", err)
			}
			return nil
		})
	}
	err = f.Close()
	if err != nil {
		return fmt.Errorf("close cutoffs file: %w", err)
	}

	return g.Wait()
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
		for scanner.Scan() {
			line := scanner.Text()
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
			slog.Error("silence detection errored", "err", err)
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

func splitAtBoundaries(ctx context.Context, inputPath string, b boundary, i int) error {
	if err := writeSegment(ctx, inputPath, b.Start, b.End, i); err != nil {
		return fmt.Errorf("segment %d: %w", i, err)
	}

	return nil
}

func writeSegment(ctx context.Context, inputPath string, start, end float64, index int) error {
	outputDir := strings.Split(filepath.Base(inputPath), ".")[0]
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("create output dir %s: %w", outputDir, err)
	}

	outputPath := filepath.Join(".", outputDir, fmt.Sprintf("output_%05d.mp3", index))
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
		return fmt.Errorf("create %s: %w", outputPath, err)
	}

	return nil
}

func formatTime(t float64) string {
	return strconv.FormatFloat(t, 'f', 3, 64)
}
