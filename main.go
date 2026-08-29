package main

import (
	"context"
	"flag"
	"io"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/faiface/beep"
	"github.com/faiface/beep/mp3"
	"github.com/faiface/beep/speaker"
	"github.com/hibooboo2/gradio/db"
	glog "github.com/hibooboo2/gradio/log"
	"github.com/hibooboo2/gradio/models"
	"golang.org/x/sync/errgroup"
)

var speakerOnce sync.Once

var recorderManager *models.RecorderSet

// init wires the models package's injected dependencies to the database and
// application functions they need, keeping the models package free of any
// database or application imports.
func init() {
	models.GetMaxDownloads = db.GetMaxDownloads
	models.InsertRecording = db.InsertRecording
	models.RecordingsDir = db.RecordingsDir
	models.DomainForURL = domainForURL
}

func main() {
	glog.Init()

	db.CreateDBHandle()

	play := flag.Bool("play", false, "play audio while recording")
	record := flag.Bool("record", false, "record radio stations")
	watch := flag.Bool("watch", false, "watch files for splitts")
	file := flag.String("f", "", "file to auto split")
	addr := flag.String("http", "", "http listen address for the management API")
	syncRadio := flag.Bool("sync", false, "force a full resync of the radio stations from radio-browser.info")
	flag.Parse()

	var wg errgroup.Group

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGKILL, syscall.SIGTERM)
	defer cancel()

	// Rebind the on-demand recorder manager to the process context so recorders
	// started from the web UI stop when the program is cancelled.
	recorderManager = models.NewRecorderSet(ctx)

	if *syncRadio {
		wg.Go(func() error {
			// Populate the recording urls from the radio_stations table. When the
			// table is empty on first run, load the stations from initstations.json so
			// all stations are available; otherwise the last sync is reused.
			n, err := syncRadioStations(ctx)
			if err != nil {
				slog.ErrorContext(ctx, "sync radio stations", "err", err)
			} else {
				slog.InfoContext(ctx, "synced radio stations", "count", n)
			}

			return nil
		})
	}

	if *file != "" {
		if err := SplitStream(ctx, *file); err != nil {
			slog.ErrorContext(ctx, "Split stream failed", "err", err)
		}
		return
	}

	wg.Go(func() error {
		if *addr != "" {
			if (*addr)[0] != ':' {
				*addr = ":" + *addr
			}
			return serveAPI(ctx, *addr)
		}
		return nil
	})

	if *watch {
		wg.Go(func() error {
			filepath.WalkDir("recordings", func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					slog.Error("Walk dir error", "err", err)
				}
				slog.Info("Recordings walk", "path", path, "entry", d.Name(), "dir", d.IsDir())
				if d.IsDir() {
					return nil
				}
				if filepath.Ext(path) != ".mp3" {
					slog.Info("Non mp3", "ext", filepath.Ext(path))
					return nil
				}
				wg.Go(func() error {
					err = SplitStream(ctx, path)
					if err != nil {
						slog.ErrorContext(ctx, "Failed to split stream", "err", err)
					}
					return nil
				})
				return nil
			})
			watchAndSplit(ctx)
			return nil
		})
	}

	if *record {
		recordingCheck := time.NewTicker(time.Minute * 5)
		wg.Go(func() error {
			for {
				rec, _ := recorderManager.ActiveRecorders()
				if len(rec) < 1 {
					stations, err := db.FetchRadioStationURLs(ctx)
					if err == nil {
						for name, url := range stations {
							recorderManager.Start(name, url)
						}
					}
					slog.InfoContext(ctx, "Record loop done one starting next")
				}
				select {
				case <-recordingCheck.C:
				case <-ctx.Done():
					return nil
				}
			}
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

	// Wait for on-demand recorders (started from the web UI) to wind down now
	// that their context has been cancelled.
	if werr := recorderManager.Wait(); werr != nil {
		slog.ErrorContext(ctx, "Recorder group done", "err", werr)
	}

	slog.Info("shutdown complete")
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
	recs, err := db.FetchPendingRecordings(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "fetch pending recordings", "err", err)
		return
	}

	for _, rec := range recs {
		if ctx.Err() != nil {
			return
		}

		slog.InfoContext(ctx, "processing recording", "id", rec.ID, "source", rec.SourcePath, "radio", rec.Radio)

		if err := db.SetRecordingStatus(ctx, rec.ID, models.StatusProcessing); err != nil {
			slog.ErrorContext(ctx, "mark recording processing", "err", err, "id", rec.ID)
			continue
		}

		if err := splitRecording(ctx, rec); err != nil {
			slog.ErrorContext(ctx, "split recording failed", "err", err, "id", rec.ID, "source", rec.SourcePath)
			if serr := db.SetRecordingStatus(ctx, rec.ID, models.StatusError); serr != nil {
				slog.ErrorContext(ctx, "mark recording error", "err", serr, "id", rec.ID)
			}
			continue
		}

		if err := db.SetRecordingStatus(ctx, rec.ID, models.StatusProcessed); err != nil {
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

			slog.Error("playback error", "err", err)

			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func playOnce(ctx context.Context) error {
	stations, err := db.FetchRadioStations(ctx, "")
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		stations[rand.IntN(len(stations))-1].URLResolved,
		nil,
	)
	if err != nil {
		return err
	}

	resp, err := models.InsecureClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	slog.Info("playback connected", "status", resp.Status)

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
