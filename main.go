package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/faiface/beep"
	"github.com/faiface/beep/mp3"
	"github.com/faiface/beep/speaker"
	"golang.org/x/sync/errgroup"
)

const (
	streamURL  = "https://radio.gayphx.com/listen/gayphx/radio.mp3"
	streamURL2 = "https://maxfm.ice.infomaniak.ch/maxfm-945.mp3"
	PlayRadio  = "Slotex"
)

var urls = map[string]string{
	// "GayPHXRadio": streamURL,
	"RandomRadio": streamURL2,
	"Slotex":      "https://s3.slotex.pl:7076/;",
}

var speakerOnce sync.Once

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	play := flag.Bool("play", false, "play audio while recording")
	file := flag.String("f", "", "file to auto split")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGKILL, syscall.SIGTERM)
	defer cancel()

	if *file != "" {
		err := SplitStream(ctx, *file)
		if err != nil {
			slog.ErrorContext(ctx, "Split stream failed", "err", err)
		}
		return
	}

	var wg errgroup.Group

	wg.Go(func() error {
		watchAndSplit(ctx)
		return nil
	})

	for name, url := range urls {
		rec := &Recorder{
			url:       url,
			radioName: name,
		}

		wg.Go(func() error {
			rec.Run(ctx)
			return nil
		})
	}

	if *play {
		wg.Go(func() error {
			PlayStream(ctx)
			slog.InfoContext(ctx, "Done playing")
			return nil
		})
	}

	err := wg.Wait()
	if err != nil {
		slog.ErrorContext(ctx, "Group done", "err", err)
	}

	log.Println("shutdown complete")
}

type Recorder struct {
	mu        sync.Mutex
	url       string
	radioName string
	file      *os.File
	fullName  string
	started   time.Time
}

// recordFileToDB marks a just-saved recording file as available for splitting.
// It is called with r.mu held whenever a file has been fully written and
// closed (rotated out or shut down).
func (r *Recorder) recordFileToDB(fullName string, started time.Time, size int64) {
	if fullName == "" {
		return
	}

	if _, err := insertRecording(fullName, r.radioName, started, size); err != nil {
		log.Printf("record %s to db: %v", fullName, err)
		return
	}

	slog.Info("recording saved to db", "filename", fullName, "radio", r.radioName)
}

func (r *Recorder) Run(ctx context.Context) {
	defer r.Close()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := r.recordOnce(ctx, r.url, time.Minute*60); err != nil {
			if ctx.Err() != nil {
				return
			}

			log.Printf("recording error: %v", err)

			select {
			case <-ctx.Done():

				return
			default:
				continue
			}
		}
	}
}

func (r *Recorder) recordOnce(ctx context.Context, streamURL string, rotateTime time.Duration) error {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		streamURL,
		nil,
	)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	log.Printf("recording connected (%s)", resp.Status)

	if err := r.rotate(); err != nil {
		return err
	}

	buf := make([]byte, 64*1024)

	currentLoop := time.Now()
	for {
		if time.Since(currentLoop) > rotateTime {
			err = r.rotate()
			if err != nil {
				return fmt.Errorf("failed to rotate file during recording")
			}
			currentLoop = time.Now()
		}

		n, err := resp.Body.Read(buf)

		if n > 0 {
			if _, werr := r.write(buf[:n]); werr != nil {
				return werr
			}
		}

		if err != nil {
			if err == io.EOF {
				return err
			}
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		default:
		}
	}
}

func (r *Recorder) write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file == nil {
		return 0, os.ErrClosed
	}

	return r.file.Write(p)
}

func (r *Recorder) rotate() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file != nil {
		if err := r.file.Sync(); err != nil {
			return err
		}

		if err := r.file.Close(); err != nil {
			return err
		}

		info, err := os.Stat(r.fullName)
		if err != nil {
			log.Printf("stat closed recording %s: %v", r.fullName, err)
		} else {
			r.recordFileToDB(r.fullName, r.started, info.Size())
		}
	}

	if err := os.MkdirAll("recordings/"+r.radioName, 0755); err != nil {
		return err
	}

	now := time.Now()

	filename := filepath.Join(
		"recordings",
		r.radioName,
		fmt.Sprintf(
			"gradio-%s.mp3",
			now.Format("2006-01-02_15-04-05"),
		),
	)

	f, err := os.Create(filename)
	if err != nil {
		return err
	}

	slog.Info("recording to", "filename", filename, "radio", r.radioName)

	r.file = f
	r.started = now
	r.fullName = filename

	return nil
}

func (r *Recorder) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file == nil {
		return
	}

	if err := r.file.Sync(); err != nil {
		log.Printf("sync error: %v", err)
	}

	if err := r.file.Close(); err != nil {
		log.Printf("close error: %v", err)
	}

	info, err := os.Stat(r.fullName)
	if err != nil {
		log.Printf("stat closed recording %s: %v", r.fullName, err)
	} else {
		r.recordFileToDB(r.fullName, r.started, info.Size())
	}

	r.file = nil
	r.fullName = ""
}

// watchAndSplit periodically polls the recordings table for files that were
// saved by the recorder but have not yet been split. When it finds one it
// marks it as processing, splits it, and records the resulting output files
// and cutoffs.
func watchAndSplit(ctx context.Context) {
	ticker := time.NewTicker(time.Second * 30)
	defer ticker.Stop()

	// Check immediately on startup so files recorded in a previous run are
	// picked up, then every tick thereafter.
	for {
		processPendingRecordings(ctx)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func processPendingRecordings(ctx context.Context) {
	recs, err := fetchPendingRecordings()
	if err != nil {
		slog.ErrorContext(ctx, "fetch pending recordings", "err", err)
		return
	}

	for _, rec := range recs {
		if ctx.Err() != nil {
			return
		}

		slog.InfoContext(ctx, "processing recording", "id", rec.ID, "source", rec.SourcePath, "radio", rec.Radio)

		if err := setRecordingStatus(rec.ID, StatusProcessing); err != nil {
			slog.ErrorContext(ctx, "mark recording processing", "err", err, "id", rec.ID)
			continue
		}

		if err := splitRecording(ctx, rec); err != nil {
			slog.ErrorContext(ctx, "split recording failed", "err", err, "id", rec.ID, "source", rec.SourcePath)
			if serr := setRecordingStatus(rec.ID, StatusError); serr != nil {
				slog.ErrorContext(ctx, "mark recording error", "err", serr, "id", rec.ID)
			}
			continue
		}

		if err := setRecordingStatus(rec.ID, StatusProcessed); err != nil {
			slog.ErrorContext(ctx, "mark recording processed", "err", err, "id", rec.ID)
		}

		slog.InfoContext(ctx, "recording processed", "id", rec.ID, "source", rec.SourcePath)
	}
}

func PlayStream(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := playOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}

			log.Printf("playback error: %v", err)

			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func playOnce(ctx context.Context) error {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		urls[PlayRadio],
		nil,
	)
	if err != nil {
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	log.Printf("playback connected (%s)", resp.Status)

	streamer, format, err := mp3.Decode(resp.Body)
	if err != nil {
		return err
	}
	defer streamer.Close()

	speakerOnce.Do(func() {
		speaker.Init(
			format.SampleRate,
			format.SampleRate.N(time.Second/10),
		)
	})

	done := make(chan struct{})

	speaker.Play(beep.Seq(
		streamer,
		beep.Callback(func() {
			close(done)
		}),
	))

	select {
	case <-ctx.Done():
		speaker.Clear()
		return nil

	case <-done:
		return io.EOF
	}
}
