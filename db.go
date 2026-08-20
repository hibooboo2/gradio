package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// defaultDBPath is the insecure CockroachDB connection used by the docker
// container started via `make run`. Override with the DATABASE_URL env var or
// setRecordDBPath (used by tests).
const defaultDBPath = "postgres://root@localhost:26257/defaultdb?sslmode=disable"

var (
	recordDB     *sql.DB
	recordDBOnce sync.Once
	recordDBPath = defaultDBPath
)

// setRecordDBPath overrides the database DSN used by the recordings tables. It
// must be called before any DB access (used by tests).
func setRecordDBPath(path string) {
	recordDBPath = path
}

// RecordingStatus tracks where a source recording is in the processing pipeline.
type RecordingStatus string

const (
	StatusPending    RecordingStatus = "pending"
	StatusProcessing RecordingStatus = "processing"
	StatusProcessed  RecordingStatus = "processed"
	StatusError      RecordingStatus = "error"
)

// Recording is one row in the recordings table: a source file produced by the
// recorder that still needs (or already received) silence splitting.
type Recording struct {
	ID         int64
	SourcePath string
	Radio      string
	RecordedAt time.Time
	SizeBytes  int64
	Status     RecordingStatus
}

// Split is one row in the splits table: a single output file produced by
// splitting a source recording, along with the boundary (cutoff) in the
// original source stream and the position of the file within that stream.
type Split struct {
	ID             int64
	RecordingID    int64
	SourcePath     string
	Index          int
	Start          float64
	End            float64
	OutputPath     string
	Classification string
	// Plays and Rating are populated only when a query joins the song_plays
	// table (player queues, global shuffle).
	Plays  int
	Rating string
}

// Duration returns the length of the split in seconds.
func (s Split) Duration() float64 {
	return s.End - s.Start
}

func CreateDBHandle() *sql.DB {
	recordDBOnce.Do(func() {
		dsn := recordDBPath
		if env := os.Getenv("DATABASE_URL"); env != "" {
			dsn = env
		}

		db, err := sql.Open("pgx", dsn)
		if err != nil {
			log.Fatalf("recordings: open db: %v", err)
			return
		}

		// CockroachDB handles concurrent writers fine, so unlike the old
		// sqlite driver there is no need to serialize access to a single
		// connection.
		db.SetMaxOpenConns(10)
		db.SetMaxIdleConns(5)

		recordDB = db

		if err := createSchema(db); err != nil {
			log.Fatalf("recordings: create tables: %v", err)
			return
		}
	})

	return recordDB
}

// createSchema ensures the recordings and splits tables (and their indexes)
// exist. It is safe to call multiple times and is re-used by tests to reset a
// clean schema after dropping tables.
func createSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS recordings (
			id          INT PRIMARY KEY DEFAULT unique_rowid(),
			source_path STRING NOT NULL,
			radio       STRING NOT NULL,
			recorded_at STRING NOT NULL,
			size_bytes  INT    NOT NULL,
			status      STRING NOT NULL DEFAULT 'pending',
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE TABLE IF NOT EXISTS splits (
			id            INT PRIMARY KEY DEFAULT unique_rowid(),
			recording_id  INT    NOT NULL REFERENCES recordings(id) ON DELETE CASCADE,
			source_path   STRING NOT NULL,
			position      INT    NOT NULL,
			start_seconds DOUBLE PRECISION NOT NULL,
			end_seconds   DOUBLE PRECISION NOT NULL,
			output_path   STRING NOT NULL,
			classification STRING NOT NULL DEFAULT '',
			created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_recordings_status ON recordings(status);
		CREATE INDEX IF NOT EXISTS idx_splits_recording ON splits(recording_id);

		CREATE TABLE IF NOT EXISTS playlists (
			id         INT PRIMARY KEY DEFAULT unique_rowid(),
			name       STRING NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE TABLE IF NOT EXISTS playlist_splits (
			id          INT PRIMARY KEY DEFAULT unique_rowid(),
			playlist_id INT NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
			split_id    INT NOT NULL REFERENCES splits(id) ON DELETE CASCADE,
			position    INT NOT NULL,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
			UNIQUE (playlist_id, split_id)
		);
		CREATE INDEX IF NOT EXISTS idx_playlist_splits_playlist ON playlist_splits(playlist_id);

		CREATE TABLE IF NOT EXISTS song_plays (
			split_id   INT PRIMARY KEY REFERENCES splits(id) ON DELETE CASCADE,
			plays      INT NOT NULL DEFAULT 0,
			rating     INT NOT NULL DEFAULT 0,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_song_plays_plays ON song_plays(plays);
	`)
	return err
}

// insertRecording records that a source file was produced and saved by the
// recorder. It returns the recording id, or 0 if the file was already present
// (same source path) so rotating/restarting does not create duplicates.
func insertRecording(sourcePath, radio string, recordedAt time.Time, sizeBytes int64) (int64, error) {
	if recordDB == nil {
		return 0, fmt.Errorf("nil db")
	}

	var existing int64
	err := recordDB.QueryRow(`SELECT id FROM recordings WHERE source_path = $1`, sourcePath).Scan(&existing)
	switch {
	case err == nil:
		return 0, nil
	case err != sql.ErrNoRows:
		return 0, err
	}

	var id int64
	err = recordDB.QueryRow(
		`INSERT INTO recordings (source_path, radio, recorded_at, size_bytes)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		sourcePath, radio, recordedAt.UTC().Format(time.RFC3339), sizeBytes,
	).Scan(&id)
	if err != nil {
		return 0, err
	}

	return id, nil
}

// fetchPendingRecordings returns recordings that still need to be split,
// oldest first.
func fetchPendingRecordings() ([]Recording, error) {
	if recordDB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := recordDB.Query(
		`SELECT id, source_path, radio, recorded_at, size_bytes, status
		 FROM recordings
		 WHERE status = $1 OR status = $2
		 ORDER BY recorded_at ASC`,
		StatusPending, StatusError,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var recs []Recording
	for rows.Next() {
		var r Recording
		var recordedAt string
		if err := rows.Scan(&r.ID, &r.SourcePath, &r.Radio, &recordedAt, &r.SizeBytes, &r.Status); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, recordedAt); err == nil {
			r.RecordedAt = t
		}
		recs = append(recs, r)
	}

	return recs, rows.Err()
}

// setRecordingStatus updates the pipeline status of a source recording.
func setRecordingStatus(id int64, status RecordingStatus) error {
	if recordDB == nil {
		return fmt.Errorf("nil db")
	}

	_, err := recordDB.Exec(`UPDATE recordings SET status = $1 WHERE id = $2`, status, id)
	return err
}

// insertSplit stores one output file produced by splitting a source recording.
func insertSplit(s Split) error {
	if recordDB == nil {
		return fmt.Errorf("nil db")
	}

	_, err := recordDB.Exec(
		`INSERT INTO splits (recording_id, source_path, position, start_seconds, end_seconds, output_path, classification)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		s.RecordingID, s.SourcePath, s.Index, s.Start, s.End, s.OutputPath, s.Classification,
	)
	return err
}

// fetchSplitsForRecording returns the split files for a source recording in
// their original stream order. Files are adjacent in the original stream when
// their Index values are consecutive.
func fetchSplitsForRecording(recordingID int64) ([]Split, error) {
	if recordDB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := recordDB.Query(
		`SELECT id, recording_id, source_path, position, start_seconds, end_seconds, output_path, classification
		 FROM splits
		 WHERE recording_id = $1
		 ORDER BY position ASC`,
		recordingID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var splits []Split
	for rows.Next() {
		var s Split
		if err := rows.Scan(&s.ID, &s.RecordingID, &s.SourcePath, &s.Index, &s.Start, &s.End, &s.OutputPath, &s.Classification); err != nil {
			return nil, err
		}
		splits = append(splits, s)
	}

	return splits, rows.Err()
}

// fetchAllSplits returns every split, newest recording first, joined with the
// radio name of the source recording for convenient display.
func fetchAllSplits() ([]Split, error) {
	if recordDB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := recordDB.Query(
		`SELECT s.id, s.recording_id, s.source_path, s.position, s.start_seconds, s.end_seconds, s.output_path, s.classification
		 FROM splits s
		 ORDER BY s.id DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var splits []Split
	for rows.Next() {
		var s Split
		if err := rows.Scan(&s.ID, &s.RecordingID, &s.SourcePath, &s.Index, &s.Start, &s.End, &s.OutputPath, &s.Classification); err != nil {
			return nil, err
		}
		splits = append(splits, s)
	}

	return splits, rows.Err()
}

// fetchSplit returns a single split by id, or an error when it does not exist.
func fetchSplit(id int64) (Split, error) {
	if recordDB == nil {
		return Split{}, fmt.Errorf("nil db")
	}

	var s Split
	err := recordDB.QueryRow(
		`SELECT id, recording_id, source_path, position, start_seconds, end_seconds, output_path, classification
		 FROM splits
		 WHERE id = $1`,
		id,
	).Scan(&s.ID, &s.RecordingID, &s.SourcePath, &s.Index, &s.Start, &s.End, &s.OutputPath, &s.Classification)
	if err != nil {
		return Split{}, err
	}

	return s, nil
}

// updateSplit persists changes to a split's boundaries and classification.
func updateSplit(s Split) error {
	if recordDB == nil {
		return fmt.Errorf("nil db")
	}

	_, err := recordDB.Exec(
		`UPDATE splits
		 SET start_seconds = $1, end_seconds = $2, classification = $3
		 WHERE id = $4`,
		s.Start, s.End, s.Classification, s.ID,
	)
	return err
}

// fetchRecordingByPath returns the recording row for a source file.
func fetchRecordingByPath(sourcePath string) (Recording, error) {
	if recordDB == nil {
		return Recording{}, fmt.Errorf("nil db")
	}

	var r Recording
	var recordedAt string
	err := recordDB.QueryRow(
		`SELECT id, source_path, radio, recorded_at, size_bytes, status
		 FROM recordings
		 WHERE source_path = $1`,
		sourcePath,
	).Scan(&r.ID, &r.SourcePath, &r.Radio, &recordedAt, &r.SizeBytes, &r.Status)
	if err != nil {
		return Recording{}, err
	}
	if t, err := time.Parse(time.RFC3339, recordedAt); err == nil {
		r.RecordedAt = t
	}

	return r, nil
}

// fetchAllRecordings returns every recording, newest first.
func fetchAllRecordings() ([]Recording, error) {
	if recordDB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := recordDB.Query(
		`SELECT id, source_path, radio, recorded_at, size_bytes, status
		 FROM recordings
		 ORDER BY id DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var recs []Recording
	for rows.Next() {
		var r Recording
		var recordedAt string
		if err := rows.Scan(&r.ID, &r.SourcePath, &r.Radio, &recordedAt, &r.SizeBytes, &r.Status); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, recordedAt); err == nil {
			r.RecordedAt = t
		}
		recs = append(recs, r)
	}

	return recs, rows.Err()
}

// Radio is one distinct radio that has split files, plus the number of splits
// available for it.
type Radio struct {
	Name       string
	SplitCount int
}

// fetchRadios returns the distinct radios that have at least one split, with
// their split counts, ordered by name. The radio name comes from the
// recordings table, which is set when a source stream is saved.
func fetchRadios() ([]Radio, error) {
	if recordDB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := recordDB.Query(
		`SELECT r.radio, count(s.id)
		 FROM recordings r
		 JOIN splits s ON s.recording_id = r.id
		 GROUP BY r.radio
		 ORDER BY r.radio ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var radios []Radio
	for rows.Next() {
		var radio Radio
		if err := rows.Scan(&radio.Name, &radio.SplitCount); err != nil {
			return nil, err
		}
		radios = append(radios, radio)
	}

	return radios, rows.Err()
}

// fetchRadioSplits returns up to limit random splits belonging to the given
// radio, in random order, so a radio can be "played" as a shuffled queue.
func fetchRadioSplits(radio string, limit int) ([]Split, error) {
	if recordDB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := recordDB.Query(
		`SELECT s.id, s.recording_id, s.source_path, s.position, s.start_seconds, s.end_seconds, s.output_path, s.classification,
		        COALESCE(sp.plays, 0), COALESCE(sp.rating, '')
		 FROM splits s
		 JOIN recordings r ON r.id = s.recording_id
		 LEFT JOIN song_plays sp ON sp.split_id = s.id
		 WHERE r.radio = $1
		 ORDER BY random()
		 LIMIT $2`,
		radio, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var splits []Split
	for rows.Next() {
		var s Split
		if err := rows.Scan(&s.ID, &s.RecordingID, &s.SourcePath, &s.Index, &s.Start, &s.End, &s.OutputPath, &s.Classification, &s.Plays, &s.Rating); err != nil {
			return nil, err
		}
		splits = append(splits, s)
	}

	return splits, rows.Err()
}

// fetchGlobalShuffleBatch returns up to limit splits for the global shuffle,
// ordered least-listened first (fewest plays) with a random tiebreak so
// repeated shuffles keep surfacing music the user has not heard yet. Splits
// marked as commercials are skipped, as are any splits whose ids appear in
// exclude (already played in the current shuffle session).
func fetchGlobalShuffleBatch(limit int, exclude []int64) ([]Split, error) {
	if recordDB == nil {
		return nil, fmt.Errorf("nil db")
	}
	if exclude == nil {
		exclude = []int64{}
	}

	rows, err := recordDB.Query(
		`SELECT s.id, s.recording_id, s.source_path, s.position, s.start_seconds, s.end_seconds, s.output_path, s.classification,
		        COALESCE(sp.plays, 0), COALESCE(sp.rating, 0)
		 FROM splits s
		 LEFT JOIN song_plays sp ON sp.split_id = s.id
		 WHERE s.classification != $1 AND s.id != ALL($2::INT[])
		 ORDER BY COALESCE(sp.plays, 0) ASC, random()
		 LIMIT $3`,
		classificationCommercial, exclude, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch shuffle: %w", err)
	}
	defer rows.Close()

	var splits []Split
	for rows.Next() {
		var s Split
		if err := rows.Scan(&s.ID, &s.RecordingID, &s.SourcePath, &s.Index, &s.Start, &s.End, &s.OutputPath, &s.Classification, &s.Plays, &s.Rating); err != nil {
			return nil, err
		}
		splits = append(splits, s)
	}

	return splits, rows.Err()
}

// recordPlay increments the play counter for a split, inserting a row the
// first time it is heard.
func recordPlay(splitID int64) error {
	if recordDB == nil {
		return fmt.Errorf("nil db")
	}

	_, err := recordDB.Exec(
		`INSERT INTO song_plays (split_id, plays) VALUES ($1, 1)
		 ON CONFLICT (split_id) DO UPDATE SET plays = song_plays.plays + 1, updated_at = now()`,
		splitID,
	)
	return err
}

// setRating records a like, dislike, or (when rating is "") clears the rating
// for a split. Existing play counts are preserved.
func setRating(splitID int64, wasLiked bool) error {
	if recordDB == nil {
		return fmt.Errorf("nil db")
	}

	liked := 0
	if wasLiked {
		liked = 1
	}
	_, err := recordDB.Exec(
		`INSERT INTO song_plays (split_id, plays, rating) VALUES ($1, 1, $2)
		 ON CONFLICT (split_id) DO UPDATE SET rating = song_plays.rating + 1, updated_at = now()`,
		splitID, liked,
	)
	return err
}

// fetchSongStats returns the play count and rating for a split, or zero values
// when the split has never been played or rated.
func fetchSongStats(splitID int64) (plays int, rating string, err error) {
	if recordDB == nil {
		return 0, "", fmt.Errorf("nil db")
	}

	err = recordDB.QueryRow(
		`SELECT plays, rating FROM song_plays WHERE split_id = $1`,
		splitID,
	).Scan(&plays, &rating)
	if err == sql.ErrNoRows {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", err
	}
	return plays, rating, nil
}
