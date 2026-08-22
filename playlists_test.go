package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

	_, err = recordDB.Exec(`DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recording_splits; DROP TABLE IF EXISTS recordings;`)
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

	_, err = recordDB.Exec(`DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recording_splits; DROP TABLE IF EXISTS recordings;`)
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
	req.SetBasicAuth(testUsername, testPassword)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "Demo")
	require.Contains(t, body, "render #1")
	require.Contains(t, body, "/songs/"+strconv.FormatInt(splits[0].ID, 10)+"/delete")

	// Player fragment must render an audio element and the song in the queue.
	req = httptest.NewRequest(http.MethodGet, "/player/view?playlist="+strconv.FormatInt(p.ID, 10), nil)
	req.SetBasicAuth(testUsername, testPassword)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	playerBody := rec.Body.String()
	require.Contains(t, playerBody, "data-audio")
	require.Contains(t, playerBody, "render #1")
	require.Contains(t, playerBody, "/music/TestRadio/render/output_00000.mp3")

	// Empty player fragment when no playlist is selected.
	req = httptest.NewRequest(http.MethodGet, "/player/view", nil)
	req.SetBasicAuth(testUsername, testPassword)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "Nothing is playing")
}

// TestRadioPlayback ensures radios are listed and a radio plays a shuffled
// queue of random splits from that radio.
func TestRadioPlayback(t *testing.T) {
	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	require.NoError(t, err)
	_, err = admin.Exec(`CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	setRecordDBPath(testDBPath)
	CreateDBHandle()

	_, err = recordDB.Exec(`DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recording_splits; DROP TABLE IF EXISTS recordings;`)
	require.NoError(t, err)
	require.NoError(t, createSchema(recordDB))

	// Two radios, each with a couple of splits.
	for _, radio := range []string{"RadioA", "RadioB"} {
		recID, err := insertRecording("/tmp/radio-"+radio+".mp3", radio, time.Now(), 123)
		require.NoError(t, err)
		require.NoError(t, insertSplit(Split{
			RecordingID: recID, SourcePath: "/tmp/radio-" + radio + ".mp3",
			Index: 0, Start: 0, End: 100,
			OutputPath: "split_music/" + radio + "/radio/output_00000.mp3",
		}))
		require.NoError(t, insertSplit(Split{
			RecordingID: recID, SourcePath: "/tmp/radio-" + radio + ".mp3",
			Index: 1, Start: 100, End: 200,
			OutputPath: "split_music/" + radio + "/radio/output_00001.mp3",
		}))
	}

	radios, err := fetchRadios()
	require.NoError(t, err)
	require.Len(t, radios, 2)
	byName := map[string]int{}
	for _, r := range radios {
		byName[r.Name] = r.SplitCount
	}
	require.Equal(t, 2, byName["RadioA"])
	require.Equal(t, 2, byName["RadioB"])

	// Radio splits are random but limited to the queue size and only that radio.
	splits, err := fetchRadioSplits("RadioA", 10)
	require.NoError(t, err)
	require.Len(t, splits, 2)
	for _, s := range splits {
		require.True(t, strings.Contains(s.OutputPath, "RadioA"), "unexpected radio split: %s", s.OutputPath)
	}

	mux := routes()

	// Empty player state lists the radios with play buttons.
	req := httptest.NewRequest(http.MethodGet, "/player/view", nil)
	req.SetBasicAuth(testUsername, testPassword)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "radio-play")
	require.Contains(t, body, "RadioA")

	// Radio mode renders a player queue and a radio subtitle.
	req = httptest.NewRequest(http.MethodGet, "/player/view?radio=RadioA", nil)
	req.SetBasicAuth(testUsername, testPassword)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	playerBody := rec.Body.String()
	require.Contains(t, playerBody, "data-audio")
	require.Contains(t, playerBody, "data-player-queue")
	require.Contains(t, playerBody, "Radio · RadioA")

	// JSON endpoint lists radios.
	req = httptest.NewRequest(http.MethodGet, "/api/radios", nil)
	req.SetBasicAuth(testUsername, testPassword)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "RadioA")
}

// TestStationsView covers the Radio Stations tab: the station list fragment
// and the record-and-play endpoint (with its no-songs-yet fallback).
func TestStationsView(t *testing.T) {
	admin, err := sql.Open("pgx", "postgres://root@localhost:26257/defaultdb?sslmode=disable")
	require.NoError(t, err)
	_, err = admin.Exec(`CREATE DATABASE IF NOT EXISTS gradio_test`)
	require.NoError(t, err)
	require.NoError(t, admin.Close())

	setRecordDBPath(testDBPath)
	CreateDBHandle()

	_, err = recordDB.Exec(`DROP TABLE IF EXISTS song_plays; DROP TABLE IF EXISTS playlist_splits; DROP TABLE IF EXISTS playlists; DROP TABLE IF EXISTS splits; DROP TABLE IF EXISTS recording_splits; DROP TABLE IF EXISTS recordings; DROP TABLE IF EXISTS favorites; DROP TABLE IF EXISTS radio_stations;`)
	require.NoError(t, err)
	require.NoError(t, createSchema(recordDB))

	require.NoError(t, upsertRadioStations([]RadioStation{
		{StationUUID: "station-1", Name: "Alpha FM", URLResolved: "https://a.example/stream", Favicon: "https://a.example/icon.png", Tags: "jazz,pop", CountryCode: "US", LanguageCodes: "eng"},
		{StationUUID: "station-2", Name: "Beta Radio", URLResolved: "https://b.example/stream", CountryCode: "DE", LanguageCodes: "ger"},
	}))

	mux := routes()

	// The stations view lists every station with a record button.
	req := authedRequest(t, http.MethodGet, "/stations/view", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "station-table")
	require.Contains(t, body, "Alpha FM")
	require.Contains(t, body, "Beta Radio")
	require.Contains(t, body, "station-1")
	require.Contains(t, body, "Record &amp; Play")

	// Clicking a station with no recorded songs starts recording and shows the
	// no-songs message instead of an empty player.
	req = authedRequest(t, http.MethodPost, "/stations/station-1/record", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body = rec.Body.String()
	require.Contains(t, body, "No songs for Alpha FM yet.")
	require.True(t, recorderManager.isRecording("Alpha FM"), "station should be recording after the click")
	recorderManager.stop("Alpha FM")

	// Recording is idempotent: a second click does not error.
	req = authedRequest(t, http.MethodPost, "/stations/station-1/record", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// An unknown station is a 404.
	req = authedRequest(t, http.MethodPost, "/stations/nope/record", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// Favorite station-1 and verify it shows a star and lands on the Favorites
	// tab.
	req = authedRequest(t, http.MethodPost, "/stations/station-1/favorite?view=stations", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body = rec.Body.String()
	require.Contains(t, body, "station-star")
	require.Contains(t, body, "station-1")

	favs, err := fetchFavoriteUUIDs()
	require.NoError(t, err)
	require.True(t, favs["station-1"])
	require.NotContains(t, favs, "station-2")

	// The Favorites tab lists only favorited stations.
	req = authedRequest(t, http.MethodGet, "/favorites/view", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body = rec.Body.String()
	require.Contains(t, body, "Favorite Radio Stations")
	require.Contains(t, body, "Alpha FM")
	require.NotContains(t, body, "Beta Radio")

	// Unfavoriting removes it from the Favorites tab.
	req = authedRequest(t, http.MethodPost, "/stations/station-1/favorite?view=favorites", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	body = rec.Body.String()
	require.NotContains(t, body, "Alpha FM")
	require.Contains(t, body, "No favorite stations yet.")

	favs, err = fetchFavoriteUUIDs()
	require.NoError(t, err)
	require.Empty(t, favs)

	// Favoriting an unknown station is a 404.
	req = authedRequest(t, http.MethodPost, "/stations/nope/favorite?view=stations", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}
