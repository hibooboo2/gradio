package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

// TestPlayHistory covers the play_history table: every recordPlay appends a
// row, and the recency/frequency/grouped fetch helpers aggregate it correctly.
func TestPlayHistory(t *testing.T) {
	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	require.NoError(t, err)
	_, err = admin.Exec(`CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	setRecordDBPath(testDBPath)
	CreateDBHandle()

	_, err = recordDB.Exec(`DROP TABLE IF EXISTS play_history; DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recording_splits; DROP TABLE IF EXISTS recordings; DROP TABLE IF EXISTS favorites; DROP TABLE IF EXISTS radio_stations;`)
	require.NoError(t, err)
	require.NoError(t, createSchema(recordDB))

	recID, err := insertRecording("/tmp/history.mp3", "TestRadio", time.Now(), 123)
	require.NoError(t, err)

	splitIDs := make([]int64, 0, 2)
	for i := 0; i < 2; i++ {
		require.NoError(t, insertSplit(Split{
			RecordingID: recID,
			SourcePath:  "/tmp/history.mp3",
			Index:       i,
			Start:       float64(i * 100),
			End:         float64(i*100 + 100),
			OutputPath:  "split_music/TestRadio/history/output_0000" + strconv.Itoa(i) + ".mp3",
		}))
		splits, err := fetchSplitsForRecording(recID)
		require.NoError(t, err)
		splitIDs = append(splitIDs, splits[len(splits)-1].ID)
	}
	require.Len(t, splitIDs, 2)

	// recordPlay appends a history row on every play, not just the first.
	require.NoError(t, recordPlay(splitIDs[0]))
	require.NoError(t, recordPlay(splitIDs[0]))
	require.NoError(t, recordPlay(splitIDs[1]))

	var count int
	require.NoError(t, recordDB.QueryRow(`SELECT count(*) FROM play_history`).Scan(&count))
	require.Equal(t, 3, count)

	// Recency: both songs, most recently played first (splitIDs[1] was last).
	recency, err := fetchPlayHistoryRecency(10)
	require.NoError(t, err)
	require.Len(t, recency, 2)
	require.Equal(t, splitIDs[1], recency[0].Split.ID)
	require.Equal(t, splitIDs[0], recency[1].Split.ID)
	require.Equal(t, "TestRadio", recency[0].Radio)
	require.Equal(t, 1, recency[0].Plays)
	require.Equal(t, 2, recency[1].Plays)
	require.False(t, recency[0].LastPlayed.IsZero())
	require.False(t, recency[0].FirstPlayed.IsZero())

	// Frequency: most played first.
	freq, err := fetchPlayHistoryFrequency(10)
	require.NoError(t, err)
	require.Len(t, freq, 2)
	require.Equal(t, splitIDs[0], freq[0].Split.ID)
	require.Equal(t, 2, freq[0].Plays)

	// Grouped by radio: one group with both songs.
	groups, err := fetchPlayHistoryGrouped(10)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, "TestRadio", groups[0].Radio)
	require.Len(t, groups[0].Plays, 2)
}

// TestHistoryEndpoints covers the play history over HTTP: the JSON API
// (?sort, ?group) and the /history/view fragment.
func TestHistoryEndpoints(t *testing.T) {
	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	require.NoError(t, err)
	_, err = admin.Exec(`CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	setRecordDBPath(testDBPath)
	CreateDBHandle()

	_, err = recordDB.Exec(`DROP TABLE IF EXISTS play_history; DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recording_splits; DROP TABLE IF EXISTS recordings;`)
	require.NoError(t, err)
	require.NoError(t, createSchema(recordDB))

	recID, err := insertRecording("/tmp/history-endpoints.mp3", "TestRadio", time.Now(), 123)
	require.NoError(t, err)
	require.NoError(t, insertSplit(Split{
		RecordingID: recID, SourcePath: "/tmp/history-endpoints.mp3",
		Index: 0, Start: 0, End: 100,
		OutputPath: "split_music/TestRadio/history-endpoints/output_00000.mp3",
	}))
	splits, err := fetchSplitsForRecording(recID)
	require.NoError(t, err)
	require.Len(t, splits, 1)

	server := httptest.NewServer(routes())
	defer server.Close()

	// Playing over the API records history rows.
	for i := 0; i < 2; i++ {
		resp, err := authedClient().Post(server.URL+"/api/splits/"+strconv.FormatInt(splits[0].ID, 10)+"/play", "application/json", nil)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		resp.Body.Close()
	}

	// JSON API, frequency sort.
	resp, err := authedClient().Get(server.URL + "/api/history?sort=frequency")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var entries []HistoryEntry
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&entries))
	require.Len(t, entries, 1)
	require.Equal(t, splits[0].ID, entries[0].Split.ID)
	require.Equal(t, 2, entries[0].Plays)
	require.Equal(t, "TestRadio", entries[0].Radio)
	resp.Body.Close()

	// JSON API, grouped by radio.
	resp, err = authedClient().Get(server.URL + "/api/history?group=radio")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var groups []RadioHistoryGroup
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&groups))
	require.Len(t, groups, 1)
	require.Equal(t, "TestRadio", groups[0].Radio)
	require.Len(t, groups[0].Plays, 1)
	resp.Body.Close()

	// View fragment renders the history table with the radio name.
	resp, err = authedClient().Get(server.URL + "/history/view?sort=frequency")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	frag := string(body)
	require.Contains(t, frag, "Play History")
	require.Contains(t, frag, "history-form")
	require.Contains(t, frag, "TestRadio")
	require.Contains(t, frag, "2")
}