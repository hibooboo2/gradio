package models

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// InsecureClient skips TLS certificate verification for audio stream fetches.
// Many radio streams are served over HTTPS with self-signed or otherwise
// invalid certificates, so we accept them rather than failing to record/play.
var InsecureClient = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	},
}

// The following function variables are injected by the main package so the
// models package stays free of database and application dependencies.
var (
	// GetMaxDownloads returns the configured global concurrent-recording cap.
	GetMaxDownloads func(ctx context.Context) (int, error)
	// InsertRecording records a just-saved recording file in the database.
	InsertRecording func(ctx context.Context, sourcePath, radio string, recordedAt time.Time, sizeBytes int64) (int64, error)
	// RecordingsDir returns the directory a station's recordings are stored in.
	RecordingsDir func(radio string) string
	// DomainForURL returns the registrable domain (eTLD+1) for a stream URL.
	DomainForURL func(url string) string
)

// GlobalQueueKey is the queue key used for stations that have no registrable
// domain but are waiting on the global concurrent-recording cap.
const GlobalQueueKey = "__global__"

// RecorderSet manages the set of active recorders. Every recorder started
// through it runs in the set's errgroup under a context derived from the
// context passed to NewRecorderSet, so cancelling that context (e.g. program
// shutdown) stops all of them.
type RecorderSet struct {
	mu        sync.Mutex
	ctx       context.Context
	g         *errgroup.Group
	recorders map[string]*Recorder
	// domains tracks which station is currently recording per registrable
	// domain (eTLD+1), so only one station per domain records at a time.
	domains map[string]string
	// queues holds the FIFO of stations waiting for a recording slot on a
	// domain that is already being recorded.
	queues map[string][]QueuedRecorder
}

// QueuedRecorder is one station waiting for a recording slot on a domain.
type QueuedRecorder struct {
	name string
	url  string
}

// StartResult describes the outcome of a RecorderSet.start call.
type StartResult struct {
	Started       bool   // a recorder was actually started
	Queued        bool   // the station was added to its domain's queue
	Domain        string // registrable domain involved
	QueuePosition int    // 1-based position in the domain queue (0 when started)
}

// DomainStatus is one active domain with its queued stations, for the
// /api/record-domains endpoint.
type DomainStatus struct {
	Domain string   `json:"domain"`
	Active string   `json:"active_station"`
	Queued []string `json:"queued_stations"`
}

// ActiveRecorder is one station currently being recorded, with display info
// for the Active Recordings tab and the /api/active-recordings endpoint.
type ActiveRecorder struct {
	Name        string    `json:"name"`
	Domain      string    `json:"domain"`
	URL         string    `json:"url"`
	Started     time.Time `json:"started"`
	Duration    string    `json:"duration"`     // human-readable elapsed recording time
	BufferBytes int       `json:"buffer_bytes"` // bytes currently buffered in RAM
	BufferHuman string    `json:"buffer_human"` // human-readable buffered size
}

// QueuedRecorderView is one station waiting for a recording slot on a domain
// that is already being recorded.
type QueuedRecorderView struct {
	Name     string `json:"name"`
	Domain   string `json:"domain"`
	URL      string `json:"url"`
	Position int    `json:"position"`
}

// ActiveRecordingsViewData is the data model for the Active Recordings tab
// fragment.
type ActiveRecordingsViewData struct {
	Active []ActiveRecorder
	Queued []QueuedRecorderView
}

// Recorder records a single radio stream into an in-memory buffer, rotating
// the buffer to disk periodically and registering each saved file with the
// database.
type Recorder struct {
	Mu        sync.Mutex
	URL       string
	RadioName string
	Domain    string
	Buffer    *bytes.Buffer
	Filename  string
	Started   time.Time
	Cancel    context.CancelFunc
}

// NewRecorderSet creates a recorder set bound to ctx. Every recorder started
// through it runs in the set's errgroup under a context derived from ctx, so
// cancelling ctx (e.g. program shutdown) stops all of them.
func NewRecorderSet(ctx context.Context) *RecorderSet {
	g, gctx := errgroup.WithContext(ctx)
	return &RecorderSet{
		ctx:       gctx,
		g:         g,
		recorders: map[string]*Recorder{},
		domains:   map[string]string{},
		queues:    map[string][]QueuedRecorder{},
	}
}

// maxDownloads returns the configured global concurrent-recording cap, or 0
// when unlimited. It queries the settings table without holding rs.mu, so it
// must be called before acquiring the lock. A DB error is treated as
// unlimited.
func (rs *RecorderSet) maxDownloads() int {
	ctx := rs.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	max, err := GetMaxDownloads(ctx)
	if err != nil {
		return 0
	}
	return max
}

// start begins recording the named station if it is not already being
// recorded. When another station on the same registrable domain is already
// recording, the station is queued for the next free slot instead and the
// returned StartResult has Queued=true. url may be empty when the station was
// already registered by the -record flag loop.
func (rs *RecorderSet) Start(name, url string) StartResult {
	max := rs.maxDownloads()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.startLocked(name, url, max)
}

// startLocked is start with rs.mu already held.
func (rs *RecorderSet) startLocked(name, url string, max int) StartResult {
	if _, ok := rs.recorders[name]; ok {
		return StartResult{}
	}
	domain := DomainForURL(url)
	if domain == "" {
		// No domain could be determined; start unrestricted unless the global
		// concurrent-recording cap is reached, in which case queue globally.
		if max > 0 && len(rs.recorders) >= max {
			return rs.enqueueLocked(GlobalQueueKey, name, url)
		}
		rs.launchLocked(name, url, "")
		return StartResult{Started: true}
	}
	if active, ok := rs.domains[domain]; ok {
		// If the station is already queued for this domain, report its
		// existing position instead of enqueueing it twice.
		for i, q := range rs.queues[domain] {
			if q.name == name {
				return StartResult{Queued: true, Domain: domain, QueuePosition: i + 1}
			}
		}
		rs.queues[domain] = append(rs.queues[domain], QueuedRecorder{name: name, url: url})
		slog.Info("Recorder queued for domain", "radio", name, "domain", domain, "active", active, "position", len(rs.queues[domain]))
		return StartResult{Queued: true, Domain: domain, QueuePosition: len(rs.queues[domain])}
	}
	// The domain is free, but the global concurrent-recording cap may still be
	// reached. Queue on this domain so the station starts when a slot frees.
	if max > 0 && len(rs.recorders) >= max {
		return rs.enqueueLocked(domain, name, url)
	}
	rs.launchLocked(name, url, domain)
	return StartResult{Started: true, Domain: domain}
}

// enqueueLocked adds the station to the FIFO queue for key (a registrable
// domain or the global queue), deduplicating by name. Callers must hold rs.mu.
func (rs *RecorderSet) enqueueLocked(key, name, url string) StartResult {
	for i, q := range rs.queues[key] {
		if q.name == name {
			return StartResult{Queued: true, Domain: key, QueuePosition: i + 1}
		}
	}
	rs.queues[key] = append(rs.queues[key], QueuedRecorder{name: name, url: url})
	slog.Info("Recorder queued", "radio", name, "queue", key, "position", len(rs.queues[key]))
	return StartResult{Queued: true, Domain: key, QueuePosition: len(rs.queues[key])}
}

// launchLocked registers and launches a recorder goroutine, marking the domain
// as active. Callers must hold rs.mu.
func (rs *RecorderSet) launchLocked(name, url, domain string) {
	ctx, cancel := context.WithCancel(rs.ctx)
	slog.InfoContext(ctx, "Recorder set starting", "radio", name, "url", url)
	rec := &Recorder{URL: url, RadioName: name, Cancel: cancel, Domain: domain}
	rs.recorders[name] = rec
	if domain != "" {
		rs.domains[domain] = name
	}
	rs.g.Go(func() error {
		defer rs.DeleteRecorder(name)
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
func (rs *RecorderSet) DeleteRecorder(name string) {
	max := rs.maxDownloads()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rec, ok := rs.recorders[name]
	if ok && rec.Cancel != nil {
		rec.Cancel()
	}
	delete(rs.recorders, name)
	if !ok {
		return
	}
	rs.releaseDomainLocked(name, rec.Domain, max)
	rs.startQueuedLocked(max)
}

// releaseDomainLocked frees the recording slot for domain when it is held by
// the just-removed recorder, then starts the next queued station (if any).
// When the slot is held by a different recorder (e.g. a bulk -record recorder
// on the same domain), the domain and its queue are left untouched so only one
// station per domain records at a time. Callers must hold rs.mu.
func (rs *RecorderSet) releaseDomainLocked(name, domain string, max int) {
	if domain == "" {
		return
	}
	if active, ok := rs.domains[domain]; !ok || active != name {
		return
	}
	delete(rs.domains, domain)
	// Respect the global concurrent-recording cap: when it is reached, leave
	// the queue intact so the next station starts when a slot frees up.
	if max > 0 && len(rs.recorders) >= max {
		return
	}
	if next := rs.popNextLocked(domain); next != nil {
		rs.startLocked(next.name, next.url, max)
	}
}

// startQueuedLocked starts the next globally-queued station now that a
// recording slot has freed up, if the global cap allows it. Callers must hold
// rs.mu.
func (rs *RecorderSet) startQueuedLocked(max int) {
	if max > 0 && len(rs.recorders) >= max {
		return
	}
	if next := rs.popNextLocked(GlobalQueueKey); next != nil {
		rs.startLocked(next.name, next.url, max)
	}
}

// popNextLocked removes and returns the head of the domain's queue, or nil
// when the queue is empty. Callers must hold rs.mu.
func (rs *RecorderSet) popNextLocked(domain string) *QueuedRecorder {
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
func (rs *RecorderSet) Wait() error {
	return rs.g.Wait()
}

// stop cancels the recorder for the named station, causing its Run loop to
// return. It is a no-op when the station is not being recorded.
func (rs *RecorderSet) Stop(name string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if r, ok := rs.recorders[name]; ok {
		if r.Cancel != nil {
			r.Cancel()
			r.Cancel = nil
		}
	}
}

// register adds an externally-owned recorder (e.g. one created by the -record
// flag loop) so start() treats the station as already recording.
func (rs *RecorderSet) register(name string, rec *Recorder) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rec.Domain == "" {
		rec.Domain = DomainForURL(rec.URL)
	}
	rs.recorders[name] = rec
	if rec.Domain != "" {
		rs.domains[rec.Domain] = name
	}
}

// unregister removes a recorder that has finished, so the station can be
// recorded again later. If other stations are queued for the same domain, the
// next one is started immediately.
func (rs *RecorderSet) unregister(name string) {
	max := rs.maxDownloads()
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rec, ok := rs.recorders[name]
	delete(rs.recorders, name)
	if !ok {
		return
	}
	rs.releaseDomainLocked(name, rec.Domain, max)
	rs.startQueuedLocked(max)
}

// isRecording reports whether the named station is currently being recorded.
func (rs *RecorderSet) IsRecording(name string) bool {
	rs.mu.Lock()
	_, ok := rs.recorders[name]
	rs.mu.Unlock()
	return ok
}

// isRecordingDomain reports whether any station on the given registrable
// domain is currently being recorded.
func (rs *RecorderSet) IsRecordingDomain(domain string) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	_, ok := rs.domains[domain]
	return ok
}

// queueInfo returns the registrable domain and 1-based queue position for the
// named station, or ("", 0) when it is not queued.
func (rs *RecorderSet) QueueInfo(name string) (domain string, position int) {
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

// domainStatuses returns a snapshot of every domain currently being recorded
// and the stations queued behind it.
func (rs *RecorderSet) DomainStatuses() []DomainStatus {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([]DomainStatus, 0, len(rs.domains))
	for d, active := range rs.domains {
		q := rs.queues[d]
		names := make([]string, 0, len(q))
		for _, n := range q {
			names = append(names, n.name)
		}
		out = append(out, DomainStatus{Domain: d, Active: active, Queued: names})
	}
	return out
}

// activeRecorders returns a snapshot of every station currently being recorded
// and every station queued behind a busy domain, sorted for stable display.
func (rs *RecorderSet) ActiveRecorders() ([]ActiveRecorder, []QueuedRecorderView) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	active := make([]ActiveRecorder, 0, len(rs.recorders))
	for name, rec := range rs.recorders {
		rec.Mu.Lock()
		started := rec.Started
		url := rec.URL
		bufferBytes := 0
		if rec.Buffer != nil {
			bufferBytes = rec.Buffer.Len()
		}
		rec.Mu.Unlock()
		duration := ""
		if !started.IsZero() {
			duration = fmtDuration(time.Since(started))
		}
		active = append(active, ActiveRecorder{
			Name:        name,
			Domain:      rec.Domain,
			URL:         url,
			Started:     started,
			Duration:    duration,
			BufferBytes: bufferBytes,
			BufferHuman: fmtBytes(bufferBytes),
		})
	}
	sort.Slice(active, func(i, j int) bool { return active[i].Name < active[j].Name })

	queued := make([]QueuedRecorderView, 0)
	for d, q := range rs.queues {
		for i, n := range q {
			queued = append(queued, QueuedRecorderView{
				Name:     n.name,
				Domain:   d,
				URL:      n.url,
				Position: i + 1,
			})
		}
	}
	sort.Slice(queued, func(i, j int) bool {
		if queued[i].Domain != queued[j].Domain {
			return queued[i].Domain < queued[j].Domain
		}
		return queued[i].Position < queued[j].Position
	})

	return active, queued
}

// fmtBytes renders a byte count as a compact human-readable string, e.g.
// "512 B", "1.2 MB" or "3.4 GB". "0 B" when b is zero or negative.
func fmtBytes(b int) string {
	if b <= 0 {
		return "0 B"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// fmtDuration renders a duration as a compact human-readable string, e.g.
// "3m 12s" or "1h 05m". Empty when d is zero or negative.
func fmtDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// recordFileToDB marks a just-saved recording file as available for splitting.
// It is called with r.Mu held whenever a file has been fully written and
// closed (rotated out or shut down).
func (r *Recorder) recordFileToDB(ctx context.Context, fullName string, started time.Time, size int64) {
	if fullName == "" {
		return
	}

	if _, err := InsertRecording(ctx, fullName, r.RadioName, started, size); err != nil {
		slog.Error("record to db", "filename", fullName, "err", err)
		return
	}

	slog.Info("recording saved to db", "filename", fullName, "radio", r.RadioName)
}

// Run records the station until ctx is cancelled, restarting RecordOnce after
// every failure or completed pass.
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
				slog.InfoContext(ctx, "Record once finished", "radioName", r.Filename)
				continue
			}
		}
	}
}

// RecordOnce connects to the station's stream and writes it into the in-memory
// buffer until the context is cancelled or the stream ends.
func (r *Recorder) RecordOnce(ctx context.Context) error {
	slog.InfoContext(ctx, "Record once called", "radioName", r.RadioName, "streamURL", r.URL)

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		r.URL,
		nil,
	)
	if err != nil {
		return err
	}

	resp, err := InsecureClient.Do(req)
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
		slog.InfoContext(ctx, "Radio recording go routine finished", "radioStation", r.RadioName, "stored", stored)
	}()

	buf := make([]byte, 64*1024)

	currentLoop := time.Now()
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := r.write(buf[:n]); werr != nil {
				slog.ErrorContext(ctx, "Write failed", "werr", werr, "radioName", r.RadioName)
				return werr
			}
		}

		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, context.DeadlineExceeded) {
				slog.ErrorContext(ctx, "Failed to read body", "err", err, "radioName", r.RadioName)
				return err
			}
			return fmt.Errorf("got error from read: %w", err)
		}

		select {
		case <-ctx.Done():
			slog.ErrorContext(ctx, "Context is done", "radio", r.RadioName)
			return nil
		default:
			if time.Since(currentLoop)/time.Millisecond%20000 == 0 {
				slog.DebugContext(ctx, "Default case in record once loop", "radio", r.RadioName)
			}
		}
	}
}

// write appends stream data to the in-memory buffer.
func (r *Recorder) write(p []byte) (int, error) {
	r.Mu.Lock()
	defer r.Mu.Unlock()

	if r.Buffer == nil {
		return 0, os.ErrClosed
	}

	return r.Buffer.Write(p)
}

// storeToDisk flushes the in-memory buffer to disk when it has grown past the
// 5GB threshold, registering the file with the database.
func (r *Recorder) storeToDisk() bool {
	slog.Debug("Callied storeToDisk", "name", r.RadioName)
	if r.Buffer != nil {
		if r.Buffer.Len() < 1024*1024*5 {
			slog.Info("Not storing recording from buffer", "size", r.Buffer.Len())
			return false
		}
		err := os.WriteFile(r.Filename, r.Buffer.Bytes(), 0644)
		if err != nil {
			slog.Error("Failed to write file", "file", r.Filename)
			return false
		}

		info, err := os.Stat(r.Filename)
		if err != nil {
			slog.Error("stat closed recording ", "fileName", r.Filename, "err", err)
		} else {
			slog.Info("Recording to db", "name", r.Filename, "size", info.Size())
			newCtx, cancel := context.WithTimeout(context.TODO(), time.Second*3)
			defer cancel()
			r.recordFileToDB(newCtx, r.Filename, r.Started, info.Size())
			return true
		}
	}
	return false
}

// rotate starts a fresh recording file: it flushes any existing buffer to disk
// and resets the target filename and start time.
func (r *Recorder) rotate(ctx context.Context) error {
	slog.InfoContext(ctx, "Rotate recorder called", "radioName", r.RadioName)
	r.Mu.Lock()
	defer r.Mu.Unlock()

	if err := os.MkdirAll(RecordingsDir(r.RadioName), 0755); err != nil {
		return err
	}

	r.storeToDisk()

	now := time.Now()

	filename := filepath.Join(
		RecordingsDir(r.RadioName),
		fmt.Sprintf(
			"gradio-%s.mp3",
			now.Format("2006-01-02_15-04-05"),
		),
	)

	slog.Info("recording to", "filename", filename, "radio", r.RadioName)

	r.Buffer = &bytes.Buffer{}
	r.Started = now
	r.Filename = filename

	return nil
}

// Close stops the recorder, flushes any buffered data to disk, and clears its
// state. It reports whether a file was stored.
func (r *Recorder) Close() bool {
	r.Mu.Lock()
	defer r.Mu.Unlock()

	stored := r.storeToDisk()

	r.Buffer = nil
	r.Filename = ""
	if r.Cancel != nil {
		r.Cancel()
		r.Cancel = nil
	}
	return stored
}
