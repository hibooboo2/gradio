package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/hibooboo2/gradio/models"
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
	rs := models.NewRecorderSet(ctx)

	// First station on the domain starts recording immediately.
	res := rs.Start("Alpha", srv.URL)
	require.True(t, res.Started)
	require.False(t, res.Queued)
	require.Equal(t, "127.0.0.1", res.Domain)
	require.True(t, rs.IsRecording("Alpha"))
	require.True(t, rs.IsRecordingDomain("127.0.0.1"))

	// Second and third stations on the same domain are queued, not started.
	res = rs.Start("Beta", srv.URL)
	require.False(t, res.Started)
	require.True(t, res.Queued)
	require.Equal(t, 1, res.QueuePosition)
	require.False(t, rs.IsRecording("Beta"))

	res = rs.Start("Gamma", srv.URL)
	require.True(t, res.Queued)
	require.Equal(t, 2, res.QueuePosition)

	// Re-requesting a station that is already queued reports its existing
	// position instead of enqueueing it twice.
	res = rs.Start("Beta", srv.URL)
	require.True(t, res.Queued)
	require.Equal(t, 1, res.QueuePosition)
	_, pos := rs.QueueInfo("Beta")
	require.Equal(t, 1, pos)

	// queueInfo reports the queued station's domain and 1-based position.
	domain, pos := rs.QueueInfo("Beta")
	require.Equal(t, "127.0.0.1", domain)
	require.Equal(t, 1, pos)
	domain, pos = rs.QueueInfo("Gamma")
	require.Equal(t, "127.0.0.1", domain)
	require.Equal(t, 2, pos)
	_, pos = rs.QueueInfo("Alpha")
	require.Equal(t, 0, pos)

	// A station on a different domain starts immediately.
	res = rs.Start("Other", "http://192.0.2.1:8080/stream")
	require.True(t, res.Started)
	require.Equal(t, "192.0.2.1", res.Domain)

	// When the active recorder finishes, the next queued station starts.
	rs.DeleteRecorder("Alpha")
	require.False(t, rs.IsRecording("Alpha"))
	require.True(t, rs.IsRecording("Beta"))
	_, pos = rs.QueueInfo("Beta")
	require.Equal(t, 0, pos)
	domain, pos = rs.QueueInfo("Gamma")
	require.Equal(t, "127.0.0.1", domain)
	require.Equal(t, 1, pos)

	// And when that one finishes, the last queued station starts.
	rs.DeleteRecorder("Beta")
	require.True(t, rs.IsRecording("Gamma"))
	_, pos = rs.QueueInfo("Gamma")
	require.Equal(t, 0, pos)

	// Cleanup: stop the last recorder; the domain slot is freed.
	rs.DeleteRecorder("Gamma")
	require.False(t, rs.IsRecording("Gamma"))
	require.False(t, rs.IsRecordingDomain("127.0.0.1"))
}
