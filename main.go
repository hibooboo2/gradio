package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/rand/v2"
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
	glog "github.com/hibooboo2/gradio/log"
	"golang.org/x/sync/errgroup"
)

var speakerOnce sync.Once

// insecureClient skips TLS certificate verification for audio stream fetches.
// Many radio streams are served over HTTPS with self-signed or otherwise
// invalid certificates, so we accept them rather than failing to record/play.
var insecureClient = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	},
}

var recorderManager *recorderSet

type recorderSet struct {
	mu        sync.Mutex
	ctx       context.Context
	g         *errgroup.Group
	recorders map[string]*Recorder
	// domains tracks which station is currently recording per registrable
	// domain (eTLD+1), so only one station per domain records at a time.
	domains map[string]string
	// queues holds the FIFO of stations waiting for a recording slot on a
	// domain that is already being recorded.
	queues map[string][]queuedRecorder
}

// queuedRecorder is one station waiting for a recording slot on a domain.
type queuedRecorder struct {
	name string
	url  string
}

// startResult describes the outcome of a recorderSet.start call.
type startResult struct {
	started       bool   // a recorder was actually started
	queued        bool   // the station was added to its domain's queue
	domain        string // registrable domain involved
	queuePosition int    // 1-based position in the domain queue (0 when started)
}

// NewRecorderSet creates a recorder set bound to ctx. Every recorder started
// through it runs in the set's errgroup under a context derived from ctx, so
// cancelling ctx (e.g. program shutdown) stops all of them.
func NewRecorderSet(ctx context.Context) *recorderSet {
	g, gctx := errgroup.WithContext(ctx)
	return &recorderSet{
		ctx:       gctx,
		g:         g,
		recorders: map[string]*Recorder{},
		domains:   map[string]string{},
		queues:    map[string][]queuedRecorder{},
	}
}

// start begins recording the named station if it is not already being
// recorded. When another station on the same registrable domain is already
// recording, the station is queued for the next free slot instead and the
// returned startResult has queued=true. url may be empty when the station was
// already registered by the -record flag loop.
func (rs *recorderSet) start(name, url string) startResult {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.startLocked(name, url)
}

// startLocked is start with rs.mu already held.
func (rs *recorderSet) startLocked(name, url string) startResult {
	if _, ok := rs.recorders[name]; ok {
		return startResult{}
	}
	domain := domainForURL(url)
	if domain == "" {
		// No domain could be determined; start unrestricted.
		rs.launchLocked(name, url, "")
		return startResult{started: true}
	}
	if active, ok := rs.domains[domain]; ok {
		// If the station is already queued for this domain, report its
		// existing position instead of enqueueing it twice.
		for i, q := range rs.queues[domain] {
			if q.name == name {
				return startResult{queued: true, domain: domain, queuePosition: i + 1}
			}
		}
		rs.queues[domain] = append(rs.queues[domain], queuedRecorder{name: name, url: url})
		slog.Info("Recorder queued for domain", "radio", name, "domain", domain, "active", active, "position", len(rs.queues[domain]))
		return startResult{queued: true, domain: domain, queuePosition: len(rs.queues[domain])}
	}
	rs.launchLocked(name, url, domain)
	return startResult{started: true, domain: domain}
}

// launchLocked registers and launches a recorder goroutine, marking the domain
// as active. Callers must hold rs.mu.
func (rs *recorderSet) launchLocked(name, url, domain string) {
	ctx, cancel := context.WithCancel(rs.ctx)
	slog.InfoContext(ctx, "Recorder set starting", "radio", name, "url", url)
	rec := &Recorder{url: url, radioName: name, cancel: cancel, domain: domain}
	rs.recorders[name] = rec
	if domain != "" {
		rs.domains[domain] = name
	}
	rs.g.Go(func() error {
		defer rs.deleteRecorder(name)
		slog.InfoContext(ctx, "Starting recorder", "radio", name, "url", url)
		recCtx, cancel := context.WithTimeout(ctx, time.Hour)
		defer cancel()
		err := rec.RecordOnce(recCtx)
		if err != nil {
			slog.ErrorContext(ctx, "REcord once returned", "err", err)
			return nil
		}
		slog.InfoContext(ctx, "RecordOnce finished no error")
		return nil
	})
}

// deleteRecorder stops and removes the named recorder, then starts the next
// station queued for the same domain (if any) so only one station per domain
// records at a time.
func (rs *recorderSet) deleteRecorder(name string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rec, ok := rs.recorders[name]
	if ok && rec.cancel != nil {
		rec.cancel()
	}
	delete(rs.recorders, name)
	if !ok {
		return
	}
	rs.releaseDomainLocked(name, rec.domain)
}

// releaseDomainLocked frees the recording slot for domain when it is held by
// the just-removed recorder, then starts the next queued station (if any).
// When the slot is held by a different recorder (e.g. a bulk -record recorder
// on the same domain), the domain and its queue are left untouched so only one
// station per domain records at a time. Callers must hold rs.mu.
func (rs *recorderSet) releaseDomainLocked(name, domain string) {
	if domain == "" {
		return
	}
	if active, ok := rs.domains[domain]; !ok || active != name {
		return
	}
	delete(rs.domains, domain)
	if next := rs.popNextLocked(domain); next != nil {
		rs.startLocked(next.name, next.url)
	}
}

// popNextLocked removes and returns the head of the domain's queue, or nil
// when the queue is empty. Callers must hold rs.mu.
func (rs *recorderSet) popNextLocked(domain string) *queuedRecorder {
	q := rs.queues[domain]
	if len(q) == 0 {
		delete(rs.queues, domain)
		return nil
	}
	next := q[0]
	if len(q) == 1 {
		delete(rs.queues, domain)
	} else {
		rs.queues[domain] = q[1:]
	}
	return &next
}

// wait blocks until every recorder started through the set has finished.
func (rs *recorderSet) wait() error {
	return rs.g.Wait()
}

// stop cancels the recorder for the named station, causing its Run loop to
// return. It is a no-op when the station is not being recorded.
func (rs *recorderSet) stop(name string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if r, ok := rs.recorders[name]; ok {
		if r.cancel != nil {
			r.cancel()
			r.cancel = nil
		}
	}
}

// register adds an externally-owned recorder (e.g. one created by the -record
// flag loop) so start() treats the station as already recording.
func (rs *recorderSet) register(name string, rec *Recorder) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rec.domain == "" {
		rec.domain = domainForURL(rec.url)
	}
	rs.recorders[name] = rec
	if rec.domain != "" {
		rs.domains[rec.domain] = name
	}
}

// unregister removes a recorder that has finished, so the station can be
// recorded again later. If other stations are queued for the same domain, the
// next one is started immediately.
func (rs *recorderSet) unregister(name string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rec, ok := rs.recorders[name]
	delete(rs.recorders, name)
	if !ok {
		return
	}
	rs.releaseDomainLocked(name, rec.domain)
}

// isRecording reports whether the named station is currently being recorded.
func (rs *recorderSet) isRecording(name string) bool {
	rs.mu.Lock()
	_, ok := rs.recorders[name]
	rs.mu.Unlock()
	return ok
}

// isRecordingDomain reports whether any station on the given registrable
// domain is currently being recorded.
func (rs *recorderSet) isRecordingDomain(domain string) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	_, ok := rs.domains[domain]
	return ok
}

// queueInfo returns the registrable domain and 1-based queue position for the
// named station, or ("", 0) when it is not queued.
func (rs *recorderSet) queueInfo(name string) (domain string, position int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for d, q := range rs.queues {
		for i, n := range q {
			if n.name == name {
				return d, i + 1
			}
		}
	}
	return "", 0
}

// domainStatus is one active domain with its queued stations, for the
// /api/record-domains endpoint.
type domainStatus struct {
	Domain string   `json:"domain"`
	Active string   `json:"active_station"`
	Queued []string `json:"queued_stations"`
}

// domainStatuses returns a snapshot of every domain currently being recorded
// and the stations queued behind it.
func (rs *recorderSet) domainStatuses() []domainStatus {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([]domainStatus, 0, len(rs.domains))
	for d, active := range rs.domains {
		q := rs.queues[d]
		names := make([]string, 0, len(q))
		for _, n := range q {
			names = append(names, n.name)
		}
		out = append(out, domainStatus{Domain: d, Active: active, Queued: names})
	}
	return out
}

func main() {
	glog.Init()

	recordDB = CreateDBHandle()

	play := flag.Bool("play", false, "play audio while recording")
	record := flag.Bool("record", false, "record radio stations")
	watch := flag.Bool("watch", false, "watch files for splitts")
	file := flag.String("f", "", "file to auto split")
	addr := flag.String("http", "", "http listen address for the management API")
	syncRadio := flag.Bool("sync", false, "force a full resync of the radio stations from radio-browser.info")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGKILL, syscall.SIGTERM)
	defer cancel()

	// Rebind the on-demand recorder manager to the process context so recorders
	// started from the web UI stop when the program is cancelled.
	recorderManager = NewRecorderSet(ctx)

	if *syncRadio {
		n, err := syncRadioStations(ctx)
		if err != nil {
			slog.ErrorContext(ctx, "sync radio stations", "err", err)
		} else {
			slog.InfoContext(ctx, "synced radio stations", "count", n)
		}
	}

	// Populate the recording urls from the radio_stations table. When the
	// table is empty on first run, load the stations from initstations.json so
	// all stations are available; otherwise the last sync is reused.
	stations, err := fetchRadioStations()
	if err != nil {
		slog.ErrorContext(ctx, "load radio stations", "err", err)
	} else if len(stations) == 0 {
		n, err := syncRadioStations(ctx)
		if err != nil {
			slog.ErrorContext(ctx, "sync radio stations", "err", err)
		} else {
			slog.InfoContext(ctx, "synced radio stations", "count", n)
		}
	}

	if *file != "" {
		if err := SplitStream(ctx, *file); err != nil {
			slog.ErrorContext(ctx, "Split stream failed", "err", err)
		}
		return
	}

	var wg errgroup.Group

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
		stations, err := fetchRadioStationURLs()
		if err == nil {
			for name, url := range stations {
				rec := &Recorder{
					url:       url,
					radioName: name,
				}
				recorderManager.register(name, rec)

				wg.Go(func() error {
					recContext, cancel := context.WithTimeout(ctx, time.Hour)
					rec.cancel = cancel
					defer cancel()
					rec.RecordOnce(recContext)
					recorderManager.unregister(name)
					return nil
				})
			}
		}
	}

	if *play {
		wg.Go(func() error {
			PlayStream(ctx)
			slog.InfoContext(ctx, "Done playing")
			return nil
		})
	}

	err = wg.Wait()
	if err != nil {
		slog.ErrorContext(ctx, "Group done", "err", err)
	}

	// Wait for on-demand recorders (started from the web UI) to wind down now
	// that their context has been cancelled.
	if werr := recorderManager.wait(); werr != nil {
		slog.ErrorContext(ctx, "Recorder group done", "err", werr)
	}

	slog.Info("shutdown complete")
}

type Recorder struct {
	mu        sync.Mutex
	url       string
	radioName string
	domain    string
	buffer    *bytes.Buffer
	filename  string
	started   time.Time
	cancel    context.CancelFunc
}

// recordFileToDB marks a just-saved recording file as available for splitting.
// It is called with r.mu held whenever a file has been fully written and
// closed (rotated out or shut down).
func (r *Recorder) recordFileToDB(fullName string, started time.Time, size int64) {
	if fullName == "" {
		return
	}

	if _, err := insertRecording(fullName, r.radioName, started, size); err != nil {
		slog.Error("record to db", "filename", fullName, "err", err)
		return
	}

	slog.Info("recording saved to db", "filename", fullName, "radio", r.radioName)
}

func (r *Recorder) Run(ctx context.Context) {
	defer r.Close()

	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := r.RecordOnce(ctx); err != nil {
			if ctx.Err() != nil {
				slog.ErrorContext(ctx, "Conext had an error", "err", ctx.Err())
			}

			slog.InfoContext(ctx, "recording failed", "err", err)

			select {
			case <-ctx.Done():
				slog.InfoContext(ctx, "Context is done")
				return
			default:
				slog.InfoContext(ctx, "Record once finished", "radioName", r.filename)
				continue
			}
		}
	}
}

func (r *Recorder) RecordOnce(ctx context.Context) error {
	slog.InfoContext(ctx, "Record once called", "radioName", r.radioName, "streamURL", r.url)

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		r.url,
		nil,
	)
	if err != nil {
		return err
	}

	resp, err := insecureClient.Do(req)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to get resp", "err", err)
		return err
	}
	defer resp.Body.Close()

	slog.Info("recording connected", "status", resp.Status)

	if err := r.rotate(ctx); err != nil {
		slog.ErrorContext(ctx, "Rotate had an error", "err", err)
		return err
	}
	defer func() {
		perr := recover()
		if err != nil {
			slog.Error("PAnicked and recoverd", "perr", perr)
		}

		stored := r.Close()
		slog.InfoContext(ctx, "Radio recording go routine finished", "radioStation", r.radioName, "stored", stored)
	}()

	buf := make([]byte, 64*1024)

	currentLoop := time.Now()
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := r.write(buf[:n]); werr != nil {
				slog.ErrorContext(ctx, "Write failed", "werr", werr, "radioName", r.radioName)
				return werr
			}
		}

		if err != nil {
			slog.ErrorContext(ctx, "Failed to read body", "err", err, "radioName", r.radioName)
			if err == io.EOF {
				return err
			}
			return err
		}

		select {
		case <-ctx.Done():
			slog.ErrorContext(ctx, "Context is done", "radio", r.radioName)
			return nil
		default:
			if time.Since(currentLoop)/time.Millisecond%20000 == 0 {
				slog.DebugContext(ctx, "Default case in record once loop", "radio", r.radioName)
			}
		}
	}
}

func (r *Recorder) write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.buffer == nil {
		return 0, os.ErrClosed
	}

	return r.buffer.Write(p)
}

func (r *Recorder) storeToDisk() bool {
	if r.buffer != nil {
		if r.buffer.Len() < 1024*1024*1024*5 {
			slog.Info("Not storing recording from buffer", "size", r.buffer.Len())
			return false
		}
		err := os.WriteFile(r.filename, r.buffer.Bytes(), 0644)
		if err != nil {
			slog.Error("Failed to write file", "file", r.filename)
			return false
		}

		info, err := os.Stat(r.filename)
		if err != nil {
			slog.Error("stat closed recording ", "fileName", r.filename, "err", err)
		} else {
			slog.Info("Recording to db", "name", r.filename, "size", info.Size())
			r.recordFileToDB(r.filename, r.started, info.Size())
			return true
		}
	}
	return false
}

func (r *Recorder) rotate(ctx context.Context) error {
	slog.InfoContext(ctx, "Rotate recorder called", "radioName", r.radioName)
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := os.MkdirAll(RecordingsDir(r.radioName), 0755); err != nil {
		return err
	}

	r.storeToDisk()

	now := time.Now()

	filename := filepath.Join(
		RecordingsDir(r.radioName),
		fmt.Sprintf(
			"gradio-%s.mp3",
			now.Format("2006-01-02_15-04-05"),
		),
	)

	slog.Info("recording to", "filename", filename, "radio", r.radioName)

	r.buffer = &bytes.Buffer{}
	r.started = now
	r.filename = filename

	return nil
}

func (r *Recorder) Close() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	stored := r.storeToDisk()

	r.buffer = nil
	r.filename = ""
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	return stored
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
	stations, err := fetchRadioStations()
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		stations[rand.IntN(len(stations))-1].URLResolved,
		nil,
	)
	if err != nil {
		return err
	}

	resp, err := insecureClient.Do(req)
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
