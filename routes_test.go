package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hibooboo2/gradio/db"
	"github.com/hibooboo2/gradio/models"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

// TestPageURLsServeShell ensures the page URLs (/splits, /player, /playlists)
// serve the htmx app shell so any view can be opened or reloaded directly,
// including when query params carry the view state.
func TestPageURLsServeShell(t *testing.T) {
	mux := routes()
	for _, path := range []string{
		"/splits", "/player", "/playlists",
		"/history", "/history?sort=frequency&group=radio",
		"/player?playlist=123&song=456",
		"/playlists?expand=99",
		"/active",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.SetBasicAuth(testUsername, testPassword)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "path %s: %s", path, rec.Body.String())
		require.True(t, strings.Contains(rec.Body.String(), "data-tab"),
			"path %s did not serve the app shell", path)
	}
}

func TestRoutes(t *testing.T) {
	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	require.NoError(t, err)
	_, err = admin.ExecContext(t.Context(), `CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	db.SetRecordDBPath(testDBPath)
	db.CreateDBHandle()

	_, err = db.DB.ExecContext(t.Context(), `DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS play_history; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recording_splits; DROP TABLE IF EXISTS recordings;`)
	require.NoError(t, err)
	require.NoError(t, db.CreateSchema(t.Context(), db.DB))

	recID, err := db.InsertRecording(t.Context(), "/tmp/routes-test.mp3", "TestRadio", time.Now(), 123)
	require.NoError(t, err)
	require.NoError(t, db.InsertSplit(t.Context(), models.Split{
		RecordingID: recID,
		SourcePath:  "/tmp/routes-test.mp3",
		Index:       0,
		Start:       10.5,
		End:         20.75,
		OutputPath:  "/tmp/routes-test/output_00000.mp3",
	}))
	require.NoError(t, db.InsertSplit(t.Context(), models.Split{
		RecordingID: recID,
		SourcePath:  "/tmp/routes-test.mp3",
		Index:       1,
		Start:       20.75,
		End:         40.0,
		OutputPath:  "/tmp/routes-test/output_00001.mp3",
	}))

	server := httptest.NewServer(routes())
	defer server.Close()

	splits := listSplits(t, server.URL)
	require.Len(t, splits, 2)
	first := splitByIndex(t, splits, 0)
	second := splitByIndex(t, splits, 1)

	t.Run("update start end classification", func(t *testing.T) {
		body := `{"start": 11.25, "end": 19.5, "classification": "talking"}`
		req, err := http.NewRequest(http.MethodPatch, server.URL+"/splits/"+strconv.FormatInt(first.ID, 10), bytes.NewBufferString(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := authedClient().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var updated models.Split
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&updated))
		require.InDelta(t, 11.25, updated.Start, 0.0001)
		require.InDelta(t, 19.5, updated.End, 0.0001)
		require.Equal(t, "talking", updated.Classification)
	})

	t.Run("partial update keeps other fields", func(t *testing.T) {
		body := `{"classification": "music"}`
		req, err := http.NewRequest(http.MethodPatch, server.URL+"/splits/"+strconv.FormatInt(second.ID, 10), bytes.NewBufferString(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := authedClient().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var updated models.Split
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&updated))
		require.InDelta(t, 20.75, updated.Start, 0.0001)
		require.InDelta(t, 40.0, updated.End, 0.0001)
		require.Equal(t, "music", updated.Classification)
	})

	t.Run("get single split", func(t *testing.T) {
		resp, err := authedClient().Get(server.URL + "/splits/" + strconv.FormatInt(first.ID, 10))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var split models.Split
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&split))
		require.Equal(t, first.ID, split.ID)
	})

	t.Run("get missing split", func(t *testing.T) {
		resp, err := authedClient().Get(server.URL + "/splits/999999999")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("list recordings", func(t *testing.T) {
		resp, err := authedClient().Get(server.URL + "/recordings")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var recs []models.Recording
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&recs))
		require.NotEmpty(t, recs)
	})
}

func listSplits(t *testing.T, baseURL string) []models.Split {
	t.Helper()
	resp, err := authedClient().Get(baseURL + "/api/splits")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var splits []models.Split
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&splits))
	return splits
}

// TestSongPlayAndRatingEndpoints covers recording plays and like/dislike votes
// over the HTTP API, plus the global shuffle JSON endpoint.
func TestSongPlayAndRatingEndpoints(t *testing.T) {
	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	require.NoError(t, err)
	_, err = admin.ExecContext(t.Context(), `CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	db.SetRecordDBPath(testDBPath)
	db.CreateDBHandle()

	_, err = db.DB.ExecContext(t.Context(), `DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS play_history; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recording_splits; DROP TABLE IF EXISTS recordings;`)
	require.NoError(t, err)
	require.NoError(t, db.CreateSchema(t.Context(), db.DB))

	recID, err := db.InsertRecording(t.Context(), "/tmp/endpoints.mp3", "TestRadio", time.Now(), 123)
	require.NoError(t, err)
	require.NoError(t, db.InsertSplit(t.Context(), models.Split{
		RecordingID: recID, SourcePath: "/tmp/endpoints.mp3",
		Index: 0, Start: 0, End: 100,
		OutputPath: "split_music/TestRadio/endpoints/output_00000.mp3",
	}))
	require.NoError(t, db.InsertSplit(t.Context(), models.Split{
		RecordingID: recID, SourcePath: "/tmp/endpoints.mp3",
		Index: 1, Start: 100, End: 200,
		OutputPath: "split_music/TestRadio/endpoints/output_00001.mp3",
	}))
	splits, err := db.FetchSplitsForRecording(t.Context(), recID)
	require.NoError(t, err)
	require.Len(t, splits, 2)

	server := httptest.NewServer(routes())
	defer server.Close()

	// Recording a play increments the count and returns the new stats.
	for i := 0; i < 3; i++ {
		resp, err := authedClient().Post(server.URL+"/api/splits/"+strconv.FormatInt(splits[0].ID, 10)+"/play", "application/json", nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var out map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		require.Equal(t, float64(i+1), out["plays"])
		resp.Body.Close()
	}

	// Like twice, then dislike once: the rating counter tracks +1, +1, -1.
	expectedRatings := []float64{1, 2, 1}
	for i, rating := range []string{"like", "like", "dislike"} {
		body := `{"rating":"` + rating + `"}`
		resp, err := authedClient().Post(server.URL+"/api/splits/"+strconv.FormatInt(splits[0].ID, 10)+"/rating", "application/json", bytes.NewBufferString(body))
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var out map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		require.Equal(t, expectedRatings[i], out["rating"])
		require.Equal(t, float64(3), out["plays"], "rating must not change the play count")
		resp.Body.Close()
	}

	// An invalid rating is rejected.
	body := `{"rating":"meh"}`
	resp, err := authedClient().Post(server.URL+"/api/splits/"+strconv.FormatInt(splits[0].ID, 10)+"/rating", "application/json", bytes.NewBufferString(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	// Global shuffle returns only the unrated split (the rated one is not
	// commercial, so both should appear in a large batch).
	resp, err = authedClient().Get(server.URL + "/api/shuffle")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var tracks []models.ShuffleTrack
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&tracks))
	require.Len(t, tracks, 2)
	resp.Body.Close()

	// Excluding the second split leaves only the first.
	resp, err = authedClient().Get(server.URL + "/api/shuffle?exclude=" + strconv.FormatInt(splits[1].ID, 10))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var tracks2 []models.ShuffleTrack
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&tracks2))
	require.Len(t, tracks2, 1)
	require.Equal(t, splits[0].ID, tracks2[0].ID)
	resp.Body.Close()

	// The shuffle view fragment renders as a player queue.
	resp, err = authedClient().Get(server.URL + "/player/view?shuffle=1")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	fragBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	frag := string(fragBytes)
	require.Contains(t, frag, "data-player-queue")
	require.Contains(t, frag, "Global Shuffle")
	require.Contains(t, frag, "data-classification-select")
	require.Contains(t, frag, "data-player-merge-prev")
	require.Contains(t, frag, "data-player-merge-next")
	resp.Body.Close()
}

// TestActiveRecordingsEndpoints covers the Active Recordings tab fragment and
// its JSON API. With no recorder set running they render the empty state.
func TestActiveRecordingsEndpoints(t *testing.T) {
	resetAuthTables(t)
	mux := routes()

	// The fragment renders the empty state when nothing is recording.
	req := authedRequest(t, http.MethodGet, "/active/view", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "No active recordings")

	// The JSON API returns empty active/queued lists.
	req = authedRequest(t, http.MethodGet, "/api/active-recordings", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		Active []models.ActiveRecorder     `json:"active"`
		Queued []models.QueuedRecorderView `json:"queued"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	require.Empty(t, out.Active)
	require.Empty(t, out.Queued)
}

func splitByIndex(t *testing.T, splits []models.Split, index int) models.Split {
	t.Helper()
	for _, s := range splits {
		if s.Index == index {
			return s
		}
	}
	t.Fatalf("no split with index %d", index)
	return models.Split{}
}

// TestSplitAudioEndpoint verifies GET /api/splits/{id}/audio extracts the
// split's segment from its source recording with ffmpeg and streams it back as
// audio/mpeg, honoring the split's current start/end boundaries even after a
// PATCH changed them.
func TestSplitAudioEndpoint(t *testing.T) {
	fixture := buildSilenceFixture(t, "split-audio.mp3")

	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	require.NoError(t, err)
	_, err = admin.ExecContext(t.Context(), `CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	db.SetRecordDBPath(testDBPath)
	db.CreateDBHandle()

	_, err = db.DB.ExecContext(t.Context(), `DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS play_history; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recording_splits; DROP TABLE IF EXISTS recordings;`)
	require.NoError(t, err)
	require.NoError(t, db.CreateSchema(t.Context(), db.DB))

	recID, err := db.InsertRecording(t.Context(), fixture, "TestRadio", time.Now(), 123)
	require.NoError(t, err)
	require.NoError(t, db.InsertSplit(t.Context(), models.Split{
		RecordingID: recID,
		SourcePath:  fixture,
		Index:       0,
		Start:       10.0,
		End:         20.0,
		OutputPath:  "split_music/TestRadio/split-audio/output_00000.mp3",
	}))
	// A split whose source recording is missing from disk.
	missingRecID, err := db.InsertRecording(t.Context(), "/tmp/does-not-exist.mp3", "TestRadio", time.Now(), 123)
	require.NoError(t, err)
	require.NoError(t, db.InsertSplit(t.Context(), models.Split{
		RecordingID: missingRecID,
		SourcePath:  "/tmp/does-not-exist.mp3",
		Index:       0,
		Start:       0,
		End:         5,
		OutputPath:  "split_music/TestRadio/split-audio/output_00001.mp3",
	}))

	splits, err := db.FetchSplitsForRecording(t.Context(), recID)
	require.NoError(t, err)
	require.Len(t, splits, 1)
	missingSplits, err := db.FetchSplitsForRecording(t.Context(), missingRecID)
	require.NoError(t, err)
	require.Len(t, missingSplits, 1)

	server := httptest.NewServer(routes())
	defer server.Close()

	audioURL := server.URL + "/api/splits/" + strconv.FormatInt(splits[0].ID, 10) + "/audio"

	t.Run("streams the split from its source", func(t *testing.T) {
		resp, err := authedClient().Get(audioURL)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "audio/mpeg", resp.Header.Get("Content-Type"))

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NotEmpty(t, body)

		out := filepath.Join(t.TempDir(), "split-audio.mp3")
		require.NoError(t, os.WriteFile(out, body, 0o644))
		d, err := fileDuration(out)
		require.NoError(t, err)
		require.InDelta(t, 10.0, d, 1.0, "served audio should match the split duration")
	})

	t.Run("honors updated boundaries", func(t *testing.T) {
		body := `{"start": 5, "end": 12}`
		req, err := http.NewRequest(http.MethodPatch, server.URL+"/splits/"+strconv.FormatInt(splits[0].ID, 10), bytes.NewBufferString(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := authedClient().Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		resp.Body.Close()

		resp, err = authedClient().Get(audioURL)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		audio, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NotEmpty(t, audio)

		out := filepath.Join(t.TempDir(), "split-audio-2.mp3")
		require.NoError(t, os.WriteFile(out, audio, 0o644))
		d, err := fileDuration(out)
		require.NoError(t, err)
		require.InDelta(t, 7.0, d, 1.0, "served audio should track the patched boundaries")
	})

	t.Run("missing split is 404", func(t *testing.T) {
		resp, err := authedClient().Get(server.URL + "/api/splits/999999999/audio")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("missing source recording is 404", func(t *testing.T) {
		resp, err := authedClient().Get(server.URL + "/api/splits/" + strconv.FormatInt(missingSplits[0].ID, 10) + "/audio")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}
