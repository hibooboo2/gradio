package main

import (
	"database/sql"
	"log"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const dbPath = "file:recordings.db?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"

var (
	recordDB     *sql.DB
	recordDBOnce sync.Once
	recordDBPath = dbPath
)

// setRecordDBPath overrides the sqlite database file used by the recordings
// tables. It must be called before any DB access (used by tests).
func setRecordDBPath(path string) {
	recordDBPath = "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"
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
	ID          int64
	RecordingID int64
	SourcePath  string
	Index       int
	Start       float64
	End         float64
	OutputPath  string
}

func recordDBHandle() *sql.DB {
	recordDBOnce.Do(func() {
		db, err := sql.Open("sqlite", recordDBPath)
		if err != nil {
			log.Fatalf("recordings: open db: %v", err)
			return
		}

		// modernc.org/sqlite + database/sql does not tolerate multiple
		// connections writing to the same file concurrently; serializing all
		// access through a single connection avoids "database is locked"
		// errors when the recorder and the split watcher write at the same
		// time.
		db.SetMaxOpenConns(1)

		if _, err := db.Exec(`
			CREATE TABLE IF NOT EXISTS recordings (
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				source_path TEXT    NOT NULL,
				radio       TEXT    NOT NULL,
				recorded_at TEXT    NOT NULL,
				size_bytes  INTEGER NOT NULL,
				status      TEXT    NOT NULL DEFAULT 'pending',
				created_at  TEXT    NOT NULL DEFAULT (datetime('now'))
			);
			CREATE TABLE IF NOT EXISTS splits (
				id            INTEGER PRIMARY KEY AUTOINCREMENT,
				recording_id  INTEGER NOT NULL REFERENCES recordings(id) ON DELETE CASCADE,
				source_path   TEXT    NOT NULL,
				position      INTEGER NOT NULL,
				start_seconds REAL    NOT NULL,
				end_seconds   REAL    NOT NULL,
				output_path   TEXT    NOT NULL,
				created_at    TEXT    NOT NULL DEFAULT (datetime('now'))
			);
			CREATE INDEX IF NOT EXISTS idx_recordings_status ON recordings(status);
			CREATE INDEX IF NOT EXISTS idx_splits_recording ON splits(recording_id);
		`); err != nil {
			log.Fatalf("recordings: create tables: %v", err)
			return
		}

		recordDB = db
	})

	return recordDB
}

// insertRecording records that a source file was produced and saved by the
// recorder. It returns the recording id, or 0 if the file was already present
// (same source path) so rotating/restarting does not create duplicates.
func insertRecording(sourcePath, radio string, recordedAt time.Time, sizeBytes int64) (int64, error) {
	db := recordDBHandle()

	var existing int64
	err := db.QueryRow(`SELECT id FROM recordings WHERE source_path = ?`, sourcePath).Scan(&existing)
	switch {
	case err == nil:
		return 0, nil
	case err != sql.ErrNoRows:
		return 0, err
	}

	res, err := db.Exec(
		`INSERT INTO recordings (source_path, radio, recorded_at, size_bytes)
		 VALUES (?, ?, ?, ?)`,
		sourcePath, radio, recordedAt.UTC().Format(time.RFC3339), sizeBytes,
	)
	if err != nil {
		return 0, err
	}

	return res.LastInsertId()
}

// fetchPendingRecordings returns recordings that still need to be split,
// oldest first.
func fetchPendingRecordings() ([]Recording, error) {
	db := recordDBHandle()

	rows, err := db.Query(
		`SELECT id, source_path, radio, recorded_at, size_bytes, status
		 FROM recordings
		 WHERE status = ? OR status = ?
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
	db := recordDBHandle()
	_, err := db.Exec(`UPDATE recordings SET status = ? WHERE id = ?`, status, id)
	return err
}

// insertSplit stores one output file produced by splitting a source recording.
func insertSplit(s Split) error {
	db := recordDBHandle()
	_, err := db.Exec(
		`INSERT INTO splits (recording_id, source_path, position, start_seconds, end_seconds, output_path)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		s.RecordingID, s.SourcePath, s.Index, s.Start, s.End, s.OutputPath,
	)
	return err
}

// fetchSplitsForRecording returns the split files for a source recording in
// their original stream order. Files are adjacent in the original stream when
// their Index values are consecutive.
func fetchSplitsForRecording(recordingID int64) ([]Split, error) {
	db := recordDBHandle()

	rows, err := db.Query(
		`SELECT id, recording_id, source_path, position, start_seconds, end_seconds, output_path
		 FROM splits
		 WHERE recording_id = ?
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
		if err := rows.Scan(&s.ID, &s.RecordingID, &s.SourcePath, &s.Index, &s.Start, &s.End, &s.OutputPath); err != nil {
			return nil, err
		}
		splits = append(splits, s)
	}

	return splits, rows.Err()
}
