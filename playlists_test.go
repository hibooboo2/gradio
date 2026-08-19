package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

func TestPlaylistLifecycle(t *testing.T) {
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

	recID, err := insertRecording("/tmp/playlist-test.mp3", "TestRadio", time.Now(), 123)
	require.NoError(t, err)
	require.NoError(t, insertSplit(Split{
		RecordingID: recID, SourcePath: "/tmp/playlist-test.mp3",
		Index: 0, Start: 0, End: 100,
		OutputPath: "split_music/TestRadio/playlist-test/output_00000.mp3",
	}))
	require.NoError(t, insertSplit(Split{
		RecordingID: recID, SourcePath: "/tmp/playlist-test.mp3",
		Index: 1, Start: 100, End: 200,
		OutputPath: "split_music/TestRadio/playlist-test/output_00001.mp3",
	}))

	splits, err := fetchSplitsForRecording(recID)
	require.NoError(t, err)
	require.Len(t, splits, 2)

	p, err := createPlaylist("My Mix")
	require.NoError(t, err)
	require.NotZero(t, p.ID)

	require.NoError(t, addSongToPlaylist(p.ID, splits[0].ID))
	require.NoError(t, addSongToPlaylist(p.ID, splits[1].ID))
	// Adding the same split twice is a no-op.
	require.NoError(t, addSongToPlaylist(p.ID, splits[0].ID))

	songs, err := fetchPlaylistSongs(p.ID)
	require.NoError(t, err)
	require.Len(t, songs, 2)
	require.Equal(t, splits[0].ID, songs[0].SplitID)
	require.Equal(t, 0, songs[0].Position)
	require.Equal(t, splits[1].ID, songs[1].SplitID)
	require.Equal(t, 1, songs[1].Position)

	all, err := fetchAllPlaylists()
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.Equal(t, 2, all[0].TrackCount)

	require.NoError(t, removeSongFromPlaylist(p.ID, splits[0].ID))
	songs, err = fetchPlaylistSongs(p.ID)
	require.NoError(t, err)
	require.Len(t, songs, 1)
	require.Equal(t, splits[1].ID, songs[0].SplitID)

	require.NoError(t, deletePlaylist(p.ID))
	_, err = fetchPlaylist(p.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)

	// Deleting the playlist also removed its song rows.
	var cnt int
	err = recordDB.QueryRow(`SELECT count(*) FROM playlist_splits WHERE playlist_id = $1`, p.ID).Scan(&cnt)
	require.NoError(t, err)
	require.Zero(t, cnt)
}

// TestPlaylistsViewRenders ensures the expanded play lists fragment and the
// player fragment render the added song without template errors.
func TestPlaylistsViewRenders(t *testing.T) {
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

	recID, err := insertRecording("/tmp/render.mp3", "TestRadio", time.Now(), 123)
	require.NoError(t, err)
	require.NoError(t, insertSplit(Split{
		RecordingID: recID, SourcePath: "/tmp/render.mp3",
		Index: 0, Start: 0, End: 100,
		OutputPath: "split_music/TestRadio/render/output_00000.mp3",
	}))
	splits, err := fetchSplitsForRecording(recID)
	require.NoError(t, err)
	require.NotEmpty(t, splits)

	p, err := createPlaylist("Demo")
	require.NoError(t, err)
	require.NoError(t, addSongToPlaylist(p.ID, splits[0].ID))

	mux := routes()

	// Expanded play lists fragment must include the added song and a working
	// remove link (this previously failed with a template error).
	req := httptest.NewRequest(http.MethodGet, "/playlists/view?expand="+strconv.FormatInt(p.ID, 10), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "Demo")
	require.Contains(t, body, "render #1")
	require.Contains(t, body, "/songs/"+strconv.FormatInt(splits[0].ID, 10)+"/delete")

	// Player fragment must render an audio element and the song in the queue.
	req = httptest.NewRequest(http.MethodGet, "/player/view?playlist="+strconv.FormatInt(p.ID, 10), nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	playerBody := rec.Body.String()
	require.Contains(t, playerBody, "data-audio")
	require.Contains(t, playerBody, "render #1")
	require.Contains(t, playerBody, "/music/TestRadio/render/output_00000.mp3")

	// Empty player fragment when no playlist is selected.
	req = httptest.NewRequest(http.MethodGet, "/player/view", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "Nothing is playing")
}
