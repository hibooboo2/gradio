package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
)

// defaultDBPath is the insecure CockroachDB connection used by the docker
// container started via `make run`. Override with the DATABASE_URL env var or
// setRecordDBPath (used by tests).
const defaultDBPath = "postgres://root@localhost:26257/defaultdb?sslmode=disable"

var (
	recordDB     *sql.DB
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
	// CustomTitle overrides the derived display title (songTitle). It is a
	// display-only rename: the source/output files are never touched.
	CustomTitle string `json:"custom_title"`
	// Plays and Rating are populated only when a query joins the song_plays
	// table (player queues, global shuffle). Rating is the like count.
	Plays  int
	Rating int
}

// Duration returns the length of the split in seconds.
func (s Split) Duration() float64 {
	return s.End - s.Start
}

// contentID returns a stable positive 64-bit id derived from a SHA-256 hash of
// the given string parts. Content-derived ids are deterministic, so a source
// file (or a generated split) keeps the same id across re-processing runs
// instead of receiving a fresh unique_rowid each time.
func contentID(parts ...string) int64 {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return int64(binary.LittleEndian.Uint64(sum[:8]) & 0x7fffffffffffffff)
}

// recordingID derives a recording's primary key from its source file name and
// size. The same file maps to the same row every time it is seen; a re-recorded
// file with a different size becomes a distinct recording.
func recordingID(sourcePath string, sizeBytes int64) int64 {
	return contentID(sourcePath, strconv.FormatInt(sizeBytes, 10))
}

// splitID derives a split's primary key from its source file name and the
// split's boundaries in that stream. The start and end keep two splits of the
// same recording distinct while staying deterministic across re-runs.
func splitID(sourcePath string, start, end float64) int64 {
	return contentID(sourcePath, strconv.FormatFloat(start, 'f', -1, 64), strconv.FormatFloat(end, 'f', -1, 64))
}

// RadioHash returns a deterministic hex-encoded SHA-256 digest of a station
// name. Recordings and split output are stored under this hash instead of the
// raw name so station names containing slashes, spaces, or other path-hostile
// characters never break path construction.
func RadioHash(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

// RecordingsDir returns the directory a station's recordings are stored in.
// The directory is keyed by the hash of the station name rather than the name
// itself, so any station name is safe to use on disk.
func RecordingsDir(radio string) string {
	return filepath.Join("recordings", RadioHash(radio))
}

// isHexHash reports whether s is a 64-character hex string, i.e. a SHA-256
// digest used as a hashed recordings directory.
func isHexHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// radioDisplayName resolves a hashed recordings directory back to the original
// station name by looking it up in the recordings table. When dir is not a
// hash, or the lookup fails, dir is returned unchanged so legacy (un-hashed)
// paths keep working.
func radioDisplayName(dir string) string {
	if !isHexHash(dir) || recordDB == nil {
		return dir
	}

	var radio string
	err := recordDB.QueryRow(
		`SELECT radio FROM recordings WHERE source_path LIKE $1 LIMIT 1`,
		"%"+dir+"/%",
	).Scan(&radio)
	if err != nil {
		return dir
	}
	return radio
}

func CreateDBHandle() *sql.DB {
	dsn := recordDBPath
	if env := os.Getenv("DATABASE_URL"); env != "" {
		dsn = env
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		slog.Error("recordings: open db", "err", err)
		os.Exit(1)
		return nil
	}

	// CockroachDB handles concurrent writers fine, so unlike the old
	// sqlite driver there is no need to serialize access to a single
	// connection.
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)

	if err := createSchema(db); err != nil {
		slog.Error("recordings: create tables", "err", err)
		os.Exit(1)
		return nil
	}

	recordDB = db
	return db
}

// createSchema ensures the recordings and splits tables (and their indexes)
// exist. It is safe to call multiple times and is re-used by tests to reset a
// clean schema after dropping tables.
func createSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS recordings (
			id          INT PRIMARY KEY,
			source_path STRING NOT NULL,
			radio       STRING NOT NULL,
			recorded_at STRING NOT NULL,
			size_bytes  INT    NOT NULL,
			status      STRING NOT NULL DEFAULT 'pending',
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE TABLE IF NOT EXISTS splits (
			id            INT PRIMARY KEY,
			recording_id  INT    NOT NULL REFERENCES recordings(id) ON DELETE CASCADE,
			source_path   STRING NOT NULL,
			position      INT    NOT NULL,
			start_seconds DOUBLE PRECISION NOT NULL,
			end_seconds   DOUBLE PRECISION NOT NULL,
			output_path   STRING NOT NULL,
			classification STRING NOT NULL DEFAULT '',
			custom_title  STRING NOT NULL DEFAULT '',
			created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_recordings_status ON recordings(status);
		CREATE INDEX IF NOT EXISTS idx_splits_recording ON splits(recording_id);

		CREATE TABLE IF NOT EXISTS recording_splits (
			recording_id INT PRIMARY KEY REFERENCES recordings(id) ON DELETE CASCADE,
			split_folder STRING NOT NULL,
			created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
		);

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

		CREATE TABLE IF NOT EXISTS users (
			id         INT PRIMARY KEY DEFAULT unique_rowid(),
			name       STRING NOT NULL UNIQUE,
			password   STRING NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);

		CREATE TABLE IF NOT EXISTS radio_stations (
			stationuuid    STRING PRIMARY KEY,
			name           STRING NOT NULL,
			url_resolved   STRING NOT NULL,
			favicon        STRING NOT NULL DEFAULT '',
			tags           STRING NOT NULL DEFAULT '',
			countrycode    STRING NOT NULL DEFAULT '',
			languagecodes  STRING NOT NULL DEFAULT '',
			created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
		);

		CREATE TABLE IF NOT EXISTS favorites (
			stationuuid  STRING PRIMARY KEY REFERENCES radio_stations(stationuuid) ON DELETE CASCADE,
			favorited_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
	`)
	if err != nil {
		return err
	}

	// Migration for databases created before custom_title existed. Fresh
	// databases already have the column from the CREATE TABLE above, so this
	// is a no-op for them.
	_, err = db.Exec(`ALTER TABLE splits ADD COLUMN IF NOT EXISTS custom_title STRING NOT NULL DEFAULT ''`)
	return err
}

// insertRecording records that a source file was produced and saved by the
// recorder. The id is a hash of the source path and file size, so re-seeing the
// same file returns the same id instead of creating a duplicate row. A file
// that was re-recorded with a different size is a distinct recording.
func insertRecording(sourcePath, radio string, recordedAt time.Time, sizeBytes int64) (int64, error) {
	if recordDB == nil {
		return 0, fmt.Errorf("nil db")
	}

	id := recordingID(sourcePath, sizeBytes)

	var existing int64
	err := recordDB.QueryRow(
		`SELECT id FROM recordings WHERE source_path = $1 AND size_bytes = $2`,
		sourcePath, sizeBytes,
	).Scan(&existing)
	switch {
	case err == nil:
		return existing, nil
	case err != sql.ErrNoRows:
		return 0, err
	}

	_, err = recordDB.Exec(
		`INSERT INTO recordings (id, source_path, radio, recorded_at, size_bytes)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (id) DO NOTHING`,
		id, sourcePath, radio, recordedAt.UTC().Format(time.RFC3339), sizeBytes,
	)
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

// markRecordingSplit records that a source recording has been fully split and
// where its output files were written. It is idempotent: re-running on the same
// recording just refreshes the stored folder.
func markRecordingSplit(recordingID int64, splitFolder string) error {
	if recordDB == nil {
		return fmt.Errorf("nil db")
	}

	_, err := recordDB.Exec(
		`INSERT INTO recording_splits (recording_id, split_folder) VALUES ($1, $2)
		 ON CONFLICT (recording_id) DO UPDATE SET split_folder = excluded.split_folder`,
		recordingID, splitFolder,
	)
	return err
}

// recordingSplitFolder returns the folder a recording's splits were written
// to, and whether the recording has been split (done). done is false when the
// recording has no row in the recording_splits table.
func recordingSplitFolder(recordingID int64) (folder string, done bool, err error) {
	if recordDB == nil {
		return "", false, fmt.Errorf("nil db")
	}

	err = recordDB.QueryRow(
		`SELECT split_folder FROM recording_splits WHERE recording_id = $1`,
		recordingID,
	).Scan(&folder)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return folder, true, nil
}

// insertSplit stores one output file produced by splitting a source recording.
// The id is a hash of the source file name and the split's boundaries (start
// and end in the source stream), so re-processing the same recording produces
// the same split ids.
func insertSplit(s Split) error {
	if recordDB == nil {
		return fmt.Errorf("nil db")
	}

	if s.ID == 0 {
		s.ID = splitID(s.SourcePath, s.Start, s.End)
	}

	_, err := recordDB.Exec(
		`INSERT INTO splits (id, recording_id, source_path, position, start_seconds, end_seconds, output_path, classification, custom_title)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 ON CONFLICT (id) DO NOTHING`,
		s.ID, s.RecordingID, s.SourcePath, s.Index, s.Start, s.End, s.OutputPath, s.Classification, s.CustomTitle,
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
		`SELECT id, recording_id, source_path, position, start_seconds, end_seconds, output_path, classification, custom_title
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
		if err := rows.Scan(&s.ID, &s.RecordingID, &s.SourcePath, &s.Index, &s.Start, &s.End, &s.OutputPath, &s.Classification, &s.CustomTitle); err != nil {
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
		`SELECT s.id, s.recording_id, s.source_path, s.position, s.start_seconds, s.end_seconds, s.output_path, s.classification, s.custom_title
		 FROM splits s
		 ORDER BY s.created_at DESC, s.id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var splits []Split
	for rows.Next() {
		var s Split
		if err := rows.Scan(&s.ID, &s.RecordingID, &s.SourcePath, &s.Index, &s.Start, &s.End, &s.OutputPath, &s.Classification, &s.CustomTitle); err != nil {
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
		`SELECT id, recording_id, source_path, position, start_seconds, end_seconds, output_path, classification, custom_title
		 FROM splits
		 WHERE id = $1`,
		id,
	).Scan(&s.ID, &s.RecordingID, &s.SourcePath, &s.Index, &s.Start, &s.End, &s.OutputPath, &s.Classification, &s.CustomTitle)
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
		 SET start_seconds = $1, end_seconds = $2, classification = $3, custom_title = $4
		 WHERE id = $5`,
		s.Start, s.End, s.Classification, s.CustomTitle, s.ID,
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

// fetchRecordingByID returns the recording row for a recording id.
func fetchRecordingByID(id int64) (Recording, error) {
	if recordDB == nil {
		return Recording{}, fmt.Errorf("nil db")
	}

	var r Recording
	var recordedAt string
	err := recordDB.QueryRow(
		`SELECT id, source_path, radio, recorded_at, size_bytes, status
		 FROM recordings
		 WHERE id = $1`,
		id,
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
		 ORDER BY created_at DESC, id`,
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
		`SELECT s.id, s.recording_id, s.source_path, s.position, s.start_seconds, s.end_seconds, s.output_path, s.classification, s.custom_title,
		        COALESCE(sp.plays, 0), COALESCE(sp.rating, 0)
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
		if err := rows.Scan(&s.ID, &s.RecordingID, &s.SourcePath, &s.Index, &s.Start, &s.End, &s.OutputPath, &s.Classification, &s.CustomTitle, &s.Plays, &s.Rating); err != nil {
			return nil, err
		}
		splits = append(splits, s)
	}

	return splits, rows.Err()
}

// fetchGlobalShuffleBatch returns up to limit splits for the global shuffle,
// ordered least-listened first (fewest plays) with a random tiebreak so
// repeated shuffles keep surfacing music the user has not heard yet. Splits
// marked as commercials or re_split are skipped, as are any splits whose ids
// appear in exclude (already played in the current shuffle session).
func fetchGlobalShuffleBatch(limit int, exclude []int64) ([]Split, error) {
	if recordDB == nil {
		return nil, fmt.Errorf("nil db")
	}
	if exclude == nil {
		exclude = []int64{}
	}

	// Pass the exclusion list as a jsonb/array literal so an empty slice still
	// has a determinable type.
	excludeArg := "{}"
	if len(exclude) > 0 {
		parts := make([]string, 0, len(exclude))
		for _, id := range exclude {
			parts = append(parts, strconv.FormatInt(id, 10))
		}
		excludeArg = "{" + strings.Join(parts, ",") + "}"
	}

	rows, err := recordDB.Query(
		`SELECT s.id, s.recording_id, s.source_path, s.position, s.start_seconds, s.end_seconds, s.output_path, s.classification, s.custom_title,
		        COALESCE(sp.plays, 0), COALESCE(sp.rating, 0)
		 FROM splits s
		 LEFT JOIN song_plays sp ON sp.split_id = s.id
		 WHERE s.classification != $1 AND s.classification != $2 AND s.id != ALL($3::INT[])
		 ORDER BY COALESCE(sp.plays, 0) ASC, random()
		 LIMIT $4`,
		classificationCommercial, classificationReSplit, excludeArg, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch shuffle: %w", err)
	}
	defer rows.Close()

	var splits []Split
	for rows.Next() {
		var s Split
		if err := rows.Scan(&s.ID, &s.RecordingID, &s.SourcePath, &s.Index, &s.Start, &s.End, &s.OutputPath, &s.Classification, &s.CustomTitle, &s.Plays, &s.Rating); err != nil {
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

// setRating records a like or dislike for a split. A like (wasLiked=true)
// increments the rating counter and a dislike (wasLiked=false) decrements it.
// Existing play counts are preserved.
func setRating(splitID int64, wasLiked bool) error {
	if recordDB == nil {
		return fmt.Errorf("nil db")
	}

	delta := 1
	if !wasLiked {
		delta = -1
	}
	_, err := recordDB.Exec(
		`INSERT INTO song_plays (split_id, plays, rating) VALUES ($1, 0, $2)
		 ON CONFLICT (split_id) DO UPDATE SET rating = song_plays.rating + $2, updated_at = now()`,
		splitID, delta,
	)
	return err
}

// fetchSongStats returns the play count and rating (like count) for a split,
// or zero values when the split has never been played or rated.
func fetchSongStats(splitID int64) (plays int, rating int, err error) {
	if recordDB == nil {
		return 0, 0, fmt.Errorf("nil db")
	}

	err = recordDB.QueryRow(
		`SELECT plays, rating FROM song_plays WHERE split_id = $1`,
		splitID,
	).Scan(&plays, &rating)
	if err == sql.ErrNoRows {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	return plays, rating, nil
}

// User is one row in the users table. Password holds the bcrypt hash of the
// user's basic-auth password; it is never stored in plaintext.
type User struct {
	ID       int64
	Name     string
	Password string
}

// fetchUserByName returns the user with the given name, or sql.ErrNoRows when
// no such user exists.
func fetchUserByName(name string) (User, error) {
	if recordDB == nil {
		return User{}, fmt.Errorf("nil db")
	}

	var u User
	err := recordDB.QueryRow(
		`SELECT id, name, password FROM users WHERE name = $1`,
		name,
	).Scan(&u.ID, &u.Name, &u.Password)
	if err != nil {
		return User{}, err
	}
	return u, nil
}

// createUser stores a new user with the given name and password. The password
// is bcrypt-hashed before being written. It is an error if a user with the
// same name already exists (the name column is unique).
func createUser(name, password string) error {
	if recordDB == nil {
		return fmt.Errorf("nil db")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	_, err = recordDB.Exec(
		`INSERT INTO users (name, password) VALUES ($1, $2)`,
		name, string(hash),
	)
	return err
}

// userPasswordMatches reports whether the given plaintext password matches the
// stored bcrypt hash for the user.
func userPasswordMatches(u User, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(u.Password), []byte(password)) == nil
}

// RadioStation is one row in the radio_stations table: a stream resolvable on
// the internet with identifying metadata pulled from radio-browser.info.
type RadioStation struct {
	StationUUID   string
	Name          string
	URLResolved   string
	Favicon       string
	Tags          string
	CountryCode   string
	LanguageCodes string
}

// upsertRadioStations inserts or refreshes the given radio stations in bulk.
// The stationuuid is the primary key, so re-syncing the same station replaces
// its metadata instead of creating a duplicate row.
func upsertRadioStations(stations []RadioStation) error {
	if recordDB == nil {
		return fmt.Errorf("nil db")
	}
	if len(stations) == 0 {
		return nil
	}

	// Build one multi-row statement so the whole batch is applied in a single
	// round trip. CockroachDB handles this fine and it is much faster than
	// inserting stations one at a time for a full 50k-station sync.
	var sb strings.Builder
	sb.WriteString(`INSERT INTO radio_stations (stationuuid, name, url_resolved, favicon, tags, countrycode, languagecodes) VALUES `)
	args := make([]any, 0, len(stations)*7)
	for i, s := range stations {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d,$%d)", i*7+1, i*7+2, i*7+3, i*7+4, i*7+5, i*7+6, i*7+7)
		args = append(args, s.StationUUID, s.Name, s.URLResolved, s.Favicon, s.Tags, s.CountryCode, s.LanguageCodes)
	}
	sb.WriteString(` ON CONFLICT (stationuuid) DO UPDATE SET
		name = excluded.name,
		url_resolved = excluded.url_resolved,
		favicon = excluded.favicon,
		tags = excluded.tags,
		countrycode = excluded.countrycode,
		languagecodes = excluded.languagecodes`)

	_, err := recordDB.Exec(sb.String(), args...)
	return err
}

// fetchRadioStations returns every station in the radio_stations table.
func fetchRadioStations() ([]RadioStation, error) {
	if recordDB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := recordDB.Query(
		`SELECT stationuuid, name, url_resolved, favicon, tags, countrycode, languagecodes
			FROM radio_stations
			order by random()
			LIMIT 20
		 `,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stations []RadioStation
	for rows.Next() {
		var s RadioStation
		if err := rows.Scan(&s.StationUUID, &s.Name, &s.URLResolved, &s.Favicon, &s.Tags, &s.CountryCode, &s.LanguageCodes); err != nil {
			return nil, err
		}
		stations = append(stations, s)
	}

	return stations, rows.Err()
}

// fetchRadioStationURLs returns every station url_resolved keyed by its name,
// so recording can pick a station by name.
func fetchRadioStationURLs() (map[string]string, error) {
	stations, err := fetchRadioStations()
	if err != nil {
		return nil, err
	}

	urls := make(map[string]string, len(stations))
	for _, s := range stations {
		if s.URLResolved == "" {
			continue
		}
		urls[s.Name] = s.URLResolved
	}
	return urls, nil
}

// fetchRadioStationByUUID returns the station with the given uuid, or
// sql.ErrNoRows when it does not exist.
func fetchRadioStationByUUID(uuid string) (RadioStation, error) {
	if recordDB == nil {
		return RadioStation{}, fmt.Errorf("nil db")
	}

	var s RadioStation
	err := recordDB.QueryRow(
		`SELECT stationuuid, name, url_resolved, favicon, tags, countrycode, languagecodes
		 FROM radio_stations
		 WHERE stationuuid = $1`,
		uuid,
	).Scan(&s.StationUUID, &s.Name, &s.URLResolved, &s.Favicon, &s.Tags, &s.CountryCode, &s.LanguageCodes)
	if err != nil {
		return RadioStation{}, err
	}
	return s, nil
}

// addFavorite marks a station as a favorite. It is idempotent: re-favoriting
// an already-favorited station keeps the original favorited_at time.
func addFavorite(stationuuid string) error {
	if recordDB == nil {
		return fmt.Errorf("nil db")
	}
	_, err := recordDB.Exec(
		`INSERT INTO favorites (stationuuid) VALUES ($1)
		 ON CONFLICT (stationuuid) DO NOTHING`,
		stationuuid,
	)
	return err
}

// removeFavorite unmarks a station as a favorite. It is a no-op when the
// station was not favorited.
func removeFavorite(stationuuid string) error {
	if recordDB == nil {
		return fmt.Errorf("nil db")
	}
	_, err := recordDB.Exec(`DELETE FROM favorites WHERE stationuuid = $1`, stationuuid)
	return err
}

// fetchFavoriteUUIDs returns the set of station uuids that are favorited.
func fetchFavoriteUUIDs() (map[string]struct{}, error) {
	if recordDB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := recordDB.Query(`SELECT stationuuid FROM favorites`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	favs := map[string]struct{}{}
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			return nil, err
		}
		favs[uuid] = struct{}{}
	}
	return favs, rows.Err()
}

// fetchFavoriteStations returns the favorited stations, ordered by when they
// were favorited, newest first.
func fetchFavoriteStations() ([]RadioStation, error) {
	if recordDB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := recordDB.Query(
		`SELECT s.stationuuid, s.name, s.url_resolved, s.favicon, s.tags, s.countrycode, s.languagecodes
		 FROM favorites f
		 JOIN radio_stations s ON s.stationuuid = f.stationuuid
		 ORDER BY f.favorited_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stations []RadioStation
	for rows.Next() {
		var s RadioStation
		if err := rows.Scan(&s.StationUUID, &s.Name, &s.URLResolved, &s.Favicon, &s.Tags, &s.CountryCode, &s.LanguageCodes); err != nil {
			return nil, err
		}
		stations = append(stations, s)
	}
	return stations, rows.Err()
}
