package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

func TestRoutes(t *testing.T) {
	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	require.NoError(t, err)
	_, err = admin.Exec(`CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	setRecordDBPath(testDBPath)
	CreateDBHandle()

	_, err = recordDB.Exec(`DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recordings;`)
	require.NoError(t, err)
	require.NoError(t, createSchema(recordDB))

	recID, err := insertRecording("/tmp/routes-test.mp3", "TestRadio", time.Now(), 123)
	require.NoError(t, err)
	require.NoError(t, insertSplit(Split{
		RecordingID: recID,
		SourcePath:  "/tmp/routes-test.mp3",
		Index:       0,
		Start:       10.5,
		End:         20.75,
		OutputPath:  "/tmp/routes-test/output_00000.mp3",
	}))
	require.NoError(t, insertSplit(Split{
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

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var updated Split
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

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var updated Split
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&updated))
		require.InDelta(t, 20.75, updated.Start, 0.0001)
		require.InDelta(t, 40.0, updated.End, 0.0001)
		require.Equal(t, "music", updated.Classification)
	})

	t.Run("get single split", func(t *testing.T) {
		resp, err := http.Get(server.URL + "/splits/" + strconv.FormatInt(first.ID, 10))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var split Split
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&split))
		require.Equal(t, first.ID, split.ID)
	})

	t.Run("get missing split", func(t *testing.T) {
		resp, err := http.Get(server.URL + "/splits/999999999")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("list recordings", func(t *testing.T) {
		resp, err := http.Get(server.URL + "/recordings")
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var recs []Recording
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&recs))
		require.NotEmpty(t, recs)
	})
}

func listSplits(t *testing.T, baseURL string) []Split {
	t.Helper()
	resp, err := http.Get(baseURL + "/splits")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var splits []Split
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&splits))
	return splits
}

func splitByIndex(t *testing.T, splits []Split, index int) Split {
	t.Helper()
	for _, s := range splits {
		if s.Index == index {
			return s
		}
	}
	t.Fatalf("no split with index %d", index)
	return Split{}
}
