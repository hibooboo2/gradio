package db

import (
	"context"
	"fmt"

	"github.com/hibooboo2/gradio/models"
)

// CreatePlaylist inserts a new playlist and returns the created row.
func CreatePlaylist(ctx context.Context, name string) (models.Playlist, error) {
	if DB == nil {
		return models.Playlist{}, fmt.Errorf("nil db")
	}

	var p models.Playlist
	err := DB.QueryRow(ctx,
		`INSERT INTO playlists (name) VALUES ($1) RETURNING id, name, created_at`,
		name,
	).Scan(&p.ID, &p.Name, &p.CreatedAt)
	if err != nil {
		return models.Playlist{}, err
	}
	return p, nil
}

// DeletePlaylist removes a playlist. Its songs are removed by the ON DELETE
// CASCADE foreign key.
func DeletePlaylist(ctx context.Context, id int64) error {
	if DB == nil {
		return fmt.Errorf("nil db")
	}
	_, err := DB.Exec(ctx, `DELETE FROM playlists WHERE id = $1`, id)
	return err
}

// FetchPlaylist returns a single playlist by id, or an error when it does not
// exist.
func FetchPlaylist(ctx context.Context, id int64) (models.Playlist, error) {
	if DB == nil {
		return models.Playlist{}, fmt.Errorf("nil db")
	}

	var p models.Playlist
	err := DB.QueryRow(ctx,
		`SELECT id, name, created_at FROM playlists WHERE id = $1`,
		id,
	).Scan(&p.ID, &p.Name, &p.CreatedAt)
	if err != nil {
		return models.Playlist{}, err
	}
	return p, nil
}

// FetchAllPlaylists returns every playlist with its track count, ordered by
// name.
func FetchAllPlaylists(ctx context.Context) ([]models.Playlist, error) {
	if DB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := DB.Query(ctx,
		`SELECT p.id, p.name, p.created_at, COUNT(ps.id)
		 FROM playlists p
		 LEFT JOIN playlist_splits ps ON ps.playlist_id = p.id
		 GROUP BY p.id
		 ORDER BY p.name ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var playlists []models.Playlist
	for rows.Next() {
		var p models.Playlist
		if err := rows.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.TrackCount); err != nil {
			return nil, err
		}
		playlists = append(playlists, p)
	}
	return playlists, rows.Err()
}

// FetchPlaylistSongs returns the splits in a playlist in playlist order.
func FetchPlaylistSongs(ctx context.Context, playlistID int64) ([]models.PlaylistSong, error) {
	if DB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := DB.Query(ctx,
		`SELECT ps.playlist_id, ps.split_id, ps.position,
		        s.id, s.recording_id, s.source_path, s.position, s.start_seconds, s.end_seconds, s.output_path, s.classification, s.custom_title,
		        COALESCE(sp.plays, 0), COALESCE(sp.rating, 0)
		 FROM playlist_splits ps
		 JOIN splits s ON s.id = ps.split_id
		 LEFT JOIN song_plays sp ON sp.split_id = s.id
		 WHERE ps.playlist_id = $1
		 ORDER BY ps.position ASC`,
		playlistID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var songs []models.PlaylistSong
	for rows.Next() {
		var song models.PlaylistSong
		if err := rows.Scan(
			&song.PlaylistID, &song.SplitID, &song.Position,
			&song.Split.ID, &song.Split.RecordingID, &song.Split.SourcePath, &song.Split.Index,
			&song.Split.Start, &song.Split.End, &song.Split.OutputPath, &song.Split.Classification, &song.Split.CustomTitle,
			&song.Plays, &song.Rating,
		); err != nil {
			return nil, err
		}
		songs = append(songs, song)
	}
	return songs, rows.Err()
}

// AddSongToPlaylist appends a split to the end of a playlist. Adding the same
// split twice is a no-op.
func AddSongToPlaylist(ctx context.Context, playlistID, splitID int64) error {
	if DB == nil {
		return fmt.Errorf("nil db")
	}

	_, err := DB.Exec(ctx,
		`INSERT INTO playlist_splits (playlist_id, split_id, position)
		 SELECT $1, $2, COALESCE(MAX(position) + 1, 0)
		 FROM playlist_splits WHERE playlist_id = $1
		 ON CONFLICT (playlist_id, split_id) DO NOTHING`,
		playlistID, splitID,
	)
	return err
}

// RemoveSongFromPlaylist removes a split from a playlist.
func RemoveSongFromPlaylist(ctx context.Context, playlistID, splitID int64) error {
	if DB == nil {
		return fmt.Errorf("nil db")
	}

	_, err := DB.Exec(ctx,
		`DELETE FROM playlist_splits WHERE playlist_id = $1 AND split_id = $2`,
		playlistID, splitID,
	)
	return err
}

// FetchAllSongs returns every split that can be added to a playlist, newest
// recording first.
func FetchAllSongs(ctx context.Context) ([]models.Split, error) {
	return FetchAllSplits(ctx)
}
