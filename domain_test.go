package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDomainForURL(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://stream.example.com/live", "example.com"},
		{"https://stream.example.com:8443/live", "example.com"},
		{"http://radio.example.co.uk:8080/stream", "example.co.uk"},
		{"https://sub.example.org/", "example.org"},
		{"https://example.com", "example.com"},
		{"http://127.0.0.1:8080/stream", "127.0.0.1"},
		{"https://localhost:8080/stream", "localhost"},
		{"", ""},
		{"not a url", ""},
	}
	for _, c := range cases {
		require.Equal(t, c.want, domainForURL(c.url), "domainForURL(%q)", c.url)
	}
}

// TestRecorderSetDomainQueue verifies that only one station per registrable
// domain records at a time: further stations on the same domain are queued
// FIFO and auto-start when the active recorder finishes.
func TestRecorderSetDomainQueue(t *testing.T) {
	// A local server that accepts connections and holds them open so each
	// recorder stays "recording" for the duration of the test.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	// Hermetic cwd so recordings/ lands in a temp dir, not the repo.
	origDir, err := os.Getwd()
	require.NoError(t, err)
	tmpDir := t.TempDir()
	require.NoError(t, os.Chdir(tmpDir))
	defer func() { require.NoError(t, os.Chdir(origDir)) }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rs := NewRecorderSet(ctx)

	// First station on the domain starts recording immediately.
	res := rs.start("Alpha", srv.URL)
	require.True(t, res.started)
	require.False(t, res.queued)
	require.Equal(t, "127.0.0.1", res.domain)
	require.True(t, rs.isRecording("Alpha"))
	require.True(t, rs.isRecordingDomain("127.0.0.1"))

	// Second and third stations on the same domain are queued, not started.
	res = rs.start("Beta", srv.URL)
	require.False(t, res.started)
	require.True(t, res.queued)
	require.Equal(t, 1, res.queuePosition)
	require.False(t, rs.isRecording("Beta"))

	res = rs.start("Gamma", srv.URL)
	require.True(t, res.queued)
	require.Equal(t, 2, res.queuePosition)

	// Re-requesting a station that is already queued reports its existing
	// position instead of enqueueing it twice.
	res = rs.start("Beta", srv.URL)
	require.True(t, res.queued)
	require.Equal(t, 1, res.queuePosition)
	_, pos := rs.queueInfo("Beta")
	require.Equal(t, 1, pos)

	// queueInfo reports the queued station's domain and 1-based position.
	domain, pos := rs.queueInfo("Beta")
	require.Equal(t, "127.0.0.1", domain)
	require.Equal(t, 1, pos)
	domain, pos = rs.queueInfo("Gamma")
	require.Equal(t, "127.0.0.1", domain)
	require.Equal(t, 2, pos)
	_, pos = rs.queueInfo("Alpha")
	require.Equal(t, 0, pos)

	// A station on a different domain starts immediately.
	res = rs.start("Other", "http://192.0.2.1:8080/stream")
	require.True(t, res.started)
	require.Equal(t, "192.0.2.1", res.domain)

	// When the active recorder finishes, the next queued station starts.
	rs.deleteRecorder("Alpha")
	require.False(t, rs.isRecording("Alpha"))
	require.True(t, rs.isRecording("Beta"))
	_, pos = rs.queueInfo("Beta")
	require.Equal(t, 0, pos)
	domain, pos = rs.queueInfo("Gamma")
	require.Equal(t, "127.0.0.1", domain)
	require.Equal(t, 1, pos)

	// And when that one finishes, the last queued station starts.
	rs.deleteRecorder("Beta")
	require.True(t, rs.isRecording("Gamma"))
	_, pos = rs.queueInfo("Gamma")
	require.Equal(t, 0, pos)

	// Cleanup: stop the last recorder; the domain slot is freed.
	rs.deleteRecorder("Gamma")
	require.False(t, rs.isRecording("Gamma"))
	require.False(t, rs.isRecordingDomain("127.0.0.1"))
}