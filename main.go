package main

import (
	"bytes"
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
)

var speakerOnce sync.Once

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	play := flag.Bool("play", false, "play audio while recording")
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &Recorder{}

	var wg errgroup.Group

	wg.Go(func() error {
		rec.Run(ctx)
		return nil
	})

	if *play {
		wg.Go(func() error {
			PlayStream(ctx)
			slog.InfoContext(ctx, "Done playing")
			return nil
		})
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	<-sig

	log.Println("shutdown requested")

	cancel()

	err := wg.Wait()
	if err != nil {
		slog.ErrorContext(ctx, "Group done", "err", err)
	}

	log.Println("shutdown complete")
}

type Recorder struct {
	mu      sync.Mutex
	file    *os.File
	started time.Time
}

func (r *Recorder) Run(ctx context.Context) {
	defer r.Close()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := r.recordOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}

			log.Printf("recording error: %v", err)

			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (r *Recorder) recordOnce(ctx context.Context) error {
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

	var buff bytes.Buffer
	body := io.TeeReader(resp.Body, &buff)
	go SplitStream(&buff)

	log.Printf("recording connected (%s)", resp.Status)

	if err := r.rotate(); err != nil {
		return err
	}

	buf := make([]byte, 64*1024)

	for {
		n, err := body.Read(buf)

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
	}

	if err := os.MkdirAll("recordings", 0755); err != nil {
		return err
	}

	now := time.Now()

	filename := filepath.Join(
		"recordings",
		fmt.Sprintf(
			"gayphx-%s.mp3",
			now.Format("2006-01-02_15-04-05"),
		),
	)

	f, err := os.Create(filename)
	if err != nil {
		return err
	}

	log.Printf("recording to %s", filename)

	r.file = f
	r.started = now

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

	r.file = nil
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
