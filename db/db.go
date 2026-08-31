// Package db owns every database interaction for gradio: the connection
// handle, the schema, and all queries against the recordings, splits,
// playlists, play history, users, radio stations, favorites and settings
// tables. It is self-contained and never imports the main package.
package db

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hibooboo2/gradio/models"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// defaultDBPath is the insecure CockroachDB connection used by the docker
// container started via `make run`. Override with the DATABASE_URL env var or
// SetRecordDBPath (used by tests).
const defaultDBPath = "postgres://root@localhost:26257/defaultdb?sslmode=disable"

var (
	// DB is the shared database handle used by every query in this package. It
	// is set by CreateDBHandle.
	DB *pgxpool.Pool

	dbPath = defaultDBPath
)

// SetRecordDBPath overrides the database DSN used by the recordings tables. It
// must be called before any DB access (used by tests).
func SetRecordDBPath(path string) {
	dbPath = path
}

// ContentID returns a stable positive 64-bit id derived from a SHA-256 hash of
// the given string parts. Content-derived ids are deterministic, so a source
// file (or a generated split) keeps the same id across re-processing runs
// instead of receiving a fresh unique_rowid each time.
func ContentID(parts ...string) int64 {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return int64(binary.LittleEndian.Uint64(sum[:8]) & 0x7fffffffffffffff)
}

// RecordingID derives a recording's primary key from its source file name. The
// same file maps to the same row every time it is seen, regardless of size.
func RecordingID(sourcePath string) int64 {
	return ContentID(sourcePath)
}

// SplitID derives a split's primary key from its source file name and the
// split's boundaries in that stream. The start and end keep two splits of the
// same recording distinct while staying deterministic across re-runs.
func SplitID(sourcePath string, start, end float64) int64 {
	return ContentID(sourcePath, strconv.FormatFloat(start, 'f', -1, 64), strconv.FormatFloat(end, 'f', -1, 64))
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

// IsHexHash reports whether s is a 64-character hex string, i.e. a SHA-256
// digest used as a hashed recordings directory.
func IsHexHash(s string) bool {
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

// RadioDisplayName resolves a hashed recordings directory back to the original
// station name. A row in the recordings table is used first, but only when it
// holds a real display name: a stored hash (written while the recordings table
// was wiped) falls through to the radio_stations table so the station name
// still wins. When dir is not a hash, or neither lookup succeeds, dir is
// returned unchanged so legacy (un-hashed) paths keep working.
func RadioDisplayName(ctx context.Context, dir string) string {
	if !IsHexHash(dir) || DB == nil {
		return dir
	}

	var radio string
	err := DB.QueryRow(ctx,
		`SELECT radio FROM recordings WHERE source_path LIKE $1 LIMIT 1`,
		"%"+dir+"/%",
	).Scan(&radio)
	if err == nil && !IsHexHash(radio) {
		return radio
	}

	if name := RadioNameByHash(ctx, dir); name != "" {
		return name
	}
	return dir
}

// CreateDBHandle opens the database connection, ensures the schema exists, and
// stores the handle in DB. It returns the handle for callers that want it.
func CreateDBHandle() *pgxpool.Pool {
	dsn := dbPath
	if env := os.Getenv("DATABASE_URL"); env != "" {
		dsn = env
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		slog.Error("recordings: parse db config", "err", err)
		os.Exit(1)
		return nil
	}

	// CockroachDB handles concurrent writers fine, so unlike the old
	// sqlite driver there is no need to serialize access to a single
	// connection.
	cfg.MaxConns = 10
	cfg.MinConns = 5

	db, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		slog.Error("recordings: open db", "err", err)
		os.Exit(1)
		return nil
	}

	if err := CreateSchema(context.Background(), db); err != nil {
		slog.Error("recordings: create tables", "err", err)
		os.Exit(1)
		return nil
	}

	DB = db
	return db
}

// CreateSchema ensures the recordings and splits tables (and their indexes)
// exist. It is safe to call multiple times and is re-used by tests to reset a
// clean schema after dropping tables.
func CreateSchema(ctx context.Context, db *pgxpool.Pool) error {
	_, err := db.Exec(ctx, `
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
		CREATE INDEX IF NOT EXISTS idx_splits_output ON splits(output_path);

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

		CREATE TABLE IF NOT EXISTS play_history (
			id        INT PRIMARY KEY DEFAULT unique_rowid(),
			split_id  INT NOT NULL REFERENCES splits(id) ON DELETE CASCADE,
			played_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_play_history_split ON play_history(split_id);
		CREATE INDEX IF NOT EXISTS idx_play_history_played_at ON play_history(played_at);

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

		CREATE TABLE IF NOT EXISTS settings (
			key        STRING PRIMARY KEY,
			value      JSONB NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
	`)
	if err != nil {
		return err
	}

	// Migration for databases created before custom_title existed. Fresh
	// databases already have the column from the CREATE TABLE above, so this
	// is a no-op for them.
	_, err = db.Exec(ctx, `ALTER TABLE splits ADD COLUMN IF NOT EXISTS custom_title STRING NOT NULL DEFAULT ''`)
	return err
}

// SetSetting stores a value under the given key in the settings table. The
// value is JSON-marshaled before being written, so any JSON-serializable type
// can be stored. Setting an existing key replaces its value in place.
func SetSetting[T any](ctx context.Context, key string, value T) error {
	if DB == nil {
		return fmt.Errorf("nil db")
	}

	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal setting %q: %w", key, err)
	}

	_, err = DB.Exec(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES ($1, $2::JSONB, now())
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = now()`,
		key, string(data),
	)
	if err != nil {
		return fmt.Errorf("set setting %q: %w", key, err)
	}
	return nil
}

// GetSetting reads a value from the settings table and unmarshals it into T.
// If the key has not been set, it persists fallback under key and returns it.
// Any error other than a missing key is returned as-is.
func GetSetting[T any](ctx context.Context, key string, fallback T) (T, error) {
	var zero T
	if DB == nil {
		return fallback, fmt.Errorf("nil db")
	}

	var raw []byte
	err := DB.QueryRow(ctx,
		`SELECT value FROM settings WHERE key = $1`,
		key,
	).Scan(&raw)
	if err == pgx.ErrNoRows {
		if serr := SetSetting(ctx, key, fallback); serr != nil {
			return fallback, fmt.Errorf("setting fallback %q: %w", key, serr)
		}
		return fallback, nil
	}
	if err != nil {
		return zero, err
	}

	if err := json.Unmarshal(raw, &zero); err != nil {
		return zero, fmt.Errorf("unmarshal setting %q: %w", key, err)
	}
	return zero, nil
}

// GetSettingOrDefault reads a value from the settings table, returning def when
// the key has not been set. Note that the missing key is persisted with def.
func GetSettingOrDefault[T any](ctx context.Context, key string, def T) (T, error) {
	return GetSetting(ctx, key, def)
}

// MaxDownloadsKey is the settings key for the maximum number of concurrent
// music file downloads. A value of 0 (or a missing key) means unlimited.
const MaxDownloadsKey = "max_downloads"

// GetMaxDownloads returns the configured maximum number of concurrent music
// downloads. A missing or zero value means unlimited.
func GetMaxDownloads(ctx context.Context) (int, error) {
	return GetSetting(ctx, MaxDownloadsKey, 5)
}

// SettingEntry is one row of the settings table: a key, its raw JSON value,
// and the time it was last updated.
type SettingEntry struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// ListSettings returns every setting, ordered by key. Values are returned as
// raw JSON so callers can present them without re-marshaling.
func ListSettings(ctx context.Context) ([]SettingEntry, error) {
	if DB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := DB.Query(ctx,
		`SELECT key, value, updated_at FROM settings ORDER BY key ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []SettingEntry
	for rows.Next() {
		var e SettingEntry
		if err := rows.Scan(&e.Key, &e.Value, &e.UpdatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}

	return entries, rows.Err()
}

// DeleteSetting removes a setting by key. It returns pgx.ErrNoRows when the
// key does not exist.
func DeleteSetting(ctx context.Context, key string) error {
	if DB == nil {
		return fmt.Errorf("nil db")
	}

	res, err := DB.Exec(ctx, `DELETE FROM settings WHERE key = $1`, key)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// GetSettingRaw returns a setting's value as raw JSON without unmarshaling it.
// It returns pgx.ErrNoRows when the key has not been set.
func GetSettingRaw(ctx context.Context, key string) (json.RawMessage, error) {
	var raw json.RawMessage
	if DB == nil {
		return nil, fmt.Errorf("nil db")
	}

	err := DB.QueryRow(ctx,
		`SELECT value FROM settings WHERE key = $1`,
		key,
	).Scan(&raw)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// InsertRecording records that a source file was produced and saved by the
// recorder. The id is a hash of the source path only, so re-seeing the same
// file returns the same id regardless of size, enabling deterministic
// re-processing instead of creating a duplicate row.
func InsertRecording(ctx context.Context, sourcePath, radio string, recordedAt time.Time, sizeBytes int64) (int64, error) {
	if DB == nil {
		return 0, fmt.Errorf("nil db")
	}

	id := RecordingID(sourcePath)

	var existing int64
	err := DB.QueryRow(ctx,
		`SELECT id FROM recordings WHERE source_path = $1`,
		sourcePath,
	).Scan(&existing)
	switch {
	case err == nil:
		return existing, nil
	case err != pgx.ErrNoRows:
		return 0, err
	}

	_, err = DB.Exec(ctx,
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

// FetchPendingRecordings returns recordings that still need to be split,
// oldest first.
func FetchPendingRecordings(ctx context.Context) ([]models.Recording, error) {
	if DB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := DB.Query(ctx,
		`SELECT id, source_path, radio, recorded_at, size_bytes, status
		 FROM recordings
		 WHERE status = $1 OR status = $2
		 ORDER BY recorded_at ASC`,
		models.StatusPending, models.StatusError,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var recs []models.Recording
	for rows.Next() {
		var r models.Recording
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

// SetRecordingStatus updates the pipeline status of a source recording.
func SetRecordingStatus(ctx context.Context, id int64, status models.RecordingStatus) error {
	if DB == nil {
		return fmt.Errorf("nil db")
	}

	_, err := DB.Exec(ctx, `UPDATE recordings SET status = $1 WHERE id = $2`, status, id)
	return err
}

// MarkRecordingSplit records that a source recording has been fully split and
// where its output files were written. It is idempotent: re-running on the same
// recording just refreshes the stored folder.
func MarkRecordingSplit(ctx context.Context, recordingID int64, splitFolder string) error {
	if DB == nil {
		return fmt.Errorf("nil db")
	}

	_, err := DB.Exec(ctx,
		`INSERT INTO recording_splits (recording_id, split_folder) VALUES ($1, $2)
		 ON CONFLICT (recording_id) DO UPDATE SET split_folder = excluded.split_folder`,
		recordingID, splitFolder,
	)
	return err
}

// RecordingSplitFolder returns the folder a recording's splits were written
// to, and whether the recording has been split (done). done is false when the
// recording has no row in the recording_splits table.
func RecordingSplitFolder(ctx context.Context, recordingID int64) (folder string, done bool, err error) {
	if DB == nil {
		return "", false, fmt.Errorf("nil db")
	}

	err = DB.QueryRow(ctx,
		`SELECT split_folder FROM recording_splits WHERE recording_id = $1`,
		recordingID,
	).Scan(&folder)
	if err == pgx.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return folder, true, nil
}

// InsertSplit stores one output file produced by splitting a source recording.
// The id is a hash of the source file name and the split's boundaries (start
// and end in the source stream), so re-processing the same recording produces
// the same split ids.
func InsertSplit(ctx context.Context, s models.Split) error {
	if DB == nil {
		return fmt.Errorf("nil db")
	}

	if s.ID == 0 {
		s.ID = SplitID(s.SourcePath, s.Start, s.End)
	}

	_, err := DB.Exec(ctx,
		`INSERT INTO splits (id, recording_id, source_path, position, start_seconds, end_seconds, output_path, classification, custom_title)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 ON CONFLICT (id) DO NOTHING`,
		s.ID, s.RecordingID, s.SourcePath, s.Index, s.Start, s.End, s.OutputPath, s.Classification, s.CustomTitle,
	)
	return err
}

// FetchSplitsForRecording returns the split files for a source recording in
// their original stream order. Files are adjacent in the original stream when
// their Index values are consecutive.
func FetchSplitsForRecording(ctx context.Context, recordingID int64) ([]models.Split, error) {
	if DB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := DB.Query(ctx,
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

	var splits []models.Split
	for rows.Next() {
		var s models.Split
		if err := rows.Scan(&s.ID, &s.RecordingID, &s.SourcePath, &s.Index, &s.Start, &s.End, &s.OutputPath, &s.Classification, &s.CustomTitle); err != nil {
			return nil, err
		}
		splits = append(splits, s)
	}

	return splits, rows.Err()
}

// FetchAllSplits returns every split, newest recording first, joined with the
// radio name of the source recording for convenient display.
func FetchAllSplits(ctx context.Context) ([]models.Split, error) {
	if DB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := DB.Query(ctx,
		`SELECT s.id, s.recording_id, s.source_path, s.position, s.start_seconds, s.end_seconds, s.output_path, s.classification, s.custom_title
		 FROM splits s
		 ORDER BY s.created_at DESC, s.id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var splits []models.Split
	for rows.Next() {
		var s models.Split
		if err := rows.Scan(&s.ID, &s.RecordingID, &s.SourcePath, &s.Index, &s.Start, &s.End, &s.OutputPath, &s.Classification, &s.CustomTitle); err != nil {
			return nil, err
		}
		splits = append(splits, s)
	}

	return splits, rows.Err()
}

// FetchSplit returns a single split by id, or an error when it does not exist.
func FetchSplit(ctx context.Context, id int64) (models.Split, error) {
	if DB == nil {
		return models.Split{}, fmt.Errorf("nil db")
	}

	var s models.Split
	err := DB.QueryRow(ctx,
		`SELECT id, recording_id, source_path, position, start_seconds, end_seconds, output_path, classification, custom_title
		 FROM splits
		 WHERE id = $1`,
		id,
	).Scan(&s.ID, &s.RecordingID, &s.SourcePath, &s.Index, &s.Start, &s.End, &s.OutputPath, &s.Classification, &s.CustomTitle)
	if err != nil {
		return models.Split{}, err
	}

	return s, nil
}

// FetchSplitByOutputPath returns the split whose output file is stored at the
// given path, or an error when no split row references it.
func FetchSplitByOutputPath(ctx context.Context, outputPath string) (models.Split, error) {
	if DB == nil {
		return models.Split{}, fmt.Errorf("nil db")
	}

	var s models.Split
	err := DB.QueryRow(ctx,
		`SELECT id, recording_id, source_path, position, start_seconds, end_seconds, output_path, classification, custom_title
		 FROM splits
		 WHERE output_path = $1`,
		outputPath,
	).Scan(&s.ID, &s.RecordingID, &s.SourcePath, &s.Index, &s.Start, &s.End, &s.OutputPath, &s.Classification, &s.CustomTitle)
	if err != nil {
		return models.Split{}, err
	}

	return s, nil
}

// UpdateSplit persists changes to a split's boundaries and classification.
func UpdateSplit(ctx context.Context, s models.Split) error {
	if DB == nil {
		return fmt.Errorf("nil db")
	}

	_, err := DB.Exec(ctx,
		`UPDATE splits
		 SET start_seconds = $1, end_seconds = $2, classification = $3, custom_title = $4
		 WHERE id = $5`,
		s.Start, s.End, s.Classification, s.CustomTitle, s.ID,
	)
	return err
}

// FetchRecordingByPath returns the recording row for a source file.
func FetchRecordingByPath(ctx context.Context, sourcePath string) (models.Recording, error) {
	if DB == nil {
		return models.Recording{}, fmt.Errorf("nil db")
	}

	var r models.Recording
	var recordedAt string
	err := DB.QueryRow(ctx,
		`SELECT id, source_path, radio, recorded_at, size_bytes, status
		 FROM recordings
		 WHERE source_path = $1`,
		sourcePath,
	).Scan(&r.ID, &r.SourcePath, &r.Radio, &recordedAt, &r.SizeBytes, &r.Status)
	if err != nil {
		return models.Recording{}, err
	}
	if t, err := time.Parse(time.RFC3339, recordedAt); err == nil {
		r.RecordedAt = t
	}

	return r, nil
}

// FetchRecordingByID returns the recording row for a recording id.
func FetchRecordingByID(ctx context.Context, id int64) (models.Recording, error) {
	if DB == nil {
		return models.Recording{}, fmt.Errorf("nil db")
	}

	var r models.Recording
	var recordedAt string
	err := DB.QueryRow(ctx,
		`SELECT id, source_path, radio, recorded_at, size_bytes, status
		 FROM recordings
		 WHERE id = $1`,
		id,
	).Scan(&r.ID, &r.SourcePath, &r.Radio, &recordedAt, &r.SizeBytes, &r.Status)
	if err != nil {
		return models.Recording{}, err
	}
	if t, err := time.Parse(time.RFC3339, recordedAt); err == nil {
		r.RecordedAt = t
	}

	return r, nil
}

// FetchAllRecordings returns every recording, newest first.
func FetchAllRecordings(ctx context.Context) ([]models.Recording, error) {
	if DB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := DB.Query(ctx,
		`SELECT id, source_path, radio, recorded_at, size_bytes, status
		 FROM recordings
		 ORDER BY created_at DESC, id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var recs []models.Recording
	for rows.Next() {
		var r models.Recording
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

// FetchRadios returns the distinct radios that have at least one split, with
// their split counts, ordered by name. The radio name comes from the
// recordings table, which is set when a source stream is saved. Hashed radio
// values are resolved back to their display name via the radio_stations table,
// and groups that resolve to the same display name (name rows plus legacy hash
// rows of the same station) are merged with their split counts summed.
func FetchRadios(ctx context.Context) ([]models.Radio, error) {
	if DB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := DB.Query(ctx,
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

	var radios []models.Radio
	for rows.Next() {
		var radio models.Radio
		if err := rows.Scan(&radio.Name, &radio.SplitCount); err != nil {
			return nil, err
		}
		if IsHexHash(radio.Name) {
			if name := RadioNameByHash(ctx, radio.Name); name != "" {
				radio.Name = name
			}
		}
		radios = append(radios, radio)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Merge duplicate display names, summing split counts and preserving
	// first-seen order.
	merged := make([]models.Radio, 0, len(radios))
	seen := make(map[string]int, len(radios))
	for _, r := range radios {
		if i, ok := seen[r.Name]; ok {
			merged[i].SplitCount += r.SplitCount
			continue
		}
		seen[r.Name] = len(merged)
		merged = append(merged, r)
	}

	return merged, nil
}

// FetchRadioSplits returns up to limit random splits belonging to the given
// radio, in random order, so a radio can be "played" as a shuffled queue. The
// match covers both the display name and its SHA-256 hash, so legacy
// hash-named rows still play even before RepairHashedRadioNames has run.
func FetchRadioSplits(ctx context.Context, radio string, limit int) ([]models.Split, error) {
	if DB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := DB.Query(ctx,
		`SELECT s.id, s.recording_id, s.source_path, s.position, s.start_seconds, s.end_seconds, s.output_path, s.classification, s.custom_title,
		        COALESCE(sp.plays, 0), COALESCE(sp.rating, 0)
		 FROM splits s
		 JOIN recordings r ON r.id = s.recording_id
		 LEFT JOIN song_plays sp ON sp.split_id = s.id
		 WHERE (r.radio = $1 OR r.radio = $2)
		 ORDER BY random()
		 LIMIT $3`,
		radio, RadioHash(radio), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var splits []models.Split
	for rows.Next() {
		var s models.Split
		if err := rows.Scan(&s.ID, &s.RecordingID, &s.SourcePath, &s.Index, &s.Start, &s.End, &s.OutputPath, &s.Classification, &s.CustomTitle, &s.Plays, &s.Rating); err != nil {
			return nil, err
		}
		splits = append(splits, s)
	}

	return splits, rows.Err()
}

// FetchAllRadioSplits returns every split belonging to the given radio,
// newest recording first with each recording's songs in stream order, so the
// station's full library can be browsed (or played) as one ordered queue. The
// match covers both the display name and its SHA-256 hash, like
// FetchRadioSplits.
func FetchAllRadioSplits(ctx context.Context, radio string) ([]models.Split, error) {
	if DB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := DB.Query(ctx,
		`SELECT s.id, s.recording_id, s.source_path, s.position, s.start_seconds, s.end_seconds, s.output_path, s.classification, s.custom_title,
		        COALESCE(sp.plays, 0), COALESCE(sp.rating, 0)
		 FROM splits s
		 JOIN recordings r ON r.id = s.recording_id
		 LEFT JOIN song_plays sp ON sp.split_id = s.id
		 WHERE (r.radio = $1 OR r.radio = $2)
		 ORDER BY r.created_at DESC, s.position ASC`,
		radio, RadioHash(radio),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var splits []models.Split
	for rows.Next() {
		var s models.Split
		if err := rows.Scan(&s.ID, &s.RecordingID, &s.SourcePath, &s.Index, &s.Start, &s.End, &s.OutputPath, &s.Classification, &s.CustomTitle, &s.Plays, &s.Rating); err != nil {
			return nil, err
		}
		splits = append(splits, s)
	}

	return splits, rows.Err()
}

// minShuffleDuration and maxShuffleDuration bound the splits served by the
// global shuffle: only clips longer than 1m30s and shorter than 6m30s are
// eligible, so short clips and long shows stay out of Shuffle All Music.
const (
	minShuffleDuration = 90  // 1m30s
	maxShuffleDuration = 390 // 6m30s
)

// FetchGlobalShuffleBatch returns up to limit splits for the global shuffle,
// ordered least-listened first (fewest plays) with a random tiebreak so
// repeated shuffles keep surfacing music the user has not heard yet. Splits
// marked as commercials or re_split are skipped, as are any splits whose ids
// appear in exclude (already played in the current shuffle session). Only
// splits longer than minShuffleDuration and shorter than maxShuffleDuration
// are returned.
func FetchGlobalShuffleBatch(ctx context.Context, limit int, exclude []int64) ([]models.Split, error) {
	if DB == nil {
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

	rows, err := DB.Query(ctx,
		`SELECT s.id, s.recording_id, s.source_path, s.position, s.start_seconds, s.end_seconds, s.output_path, s.classification, s.custom_title,
		        COALESCE(sp.plays, 0), COALESCE(sp.rating, 0)
		 FROM splits s
		 LEFT JOIN song_plays sp ON sp.split_id = s.id
		 WHERE s.classification != $1 AND s.classification != $2 AND s.id != ALL($3::INT[])
		   AND s.end_seconds - s.start_seconds > $5
		   AND s.end_seconds - s.start_seconds < $6
		 ORDER BY COALESCE(sp.plays, 0) ASC, random()
		 LIMIT $4`,
		models.ClassificationCommercial, models.ClassificationReSplit, excludeArg, limit,
		minShuffleDuration, maxShuffleDuration,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch shuffle: %w", err)
	}
	defer rows.Close()

	var splits []models.Split
	for rows.Next() {
		var s models.Split
		if err := rows.Scan(&s.ID, &s.RecordingID, &s.SourcePath, &s.Index, &s.Start, &s.End, &s.OutputPath, &s.Classification, &s.CustomTitle, &s.Plays, &s.Rating); err != nil {
			return nil, err
		}
		splits = append(splits, s)
	}

	return splits, rows.Err()
}

// RecordPlay increments the play counter for a split, inserting a row the
// first time it is heard. Every play also appends a row to the play_history
// table so the history view can show what was played and when.
func RecordPlay(ctx context.Context, splitID int64) error {
	if DB == nil {
		return fmt.Errorf("nil db")
	}

	_, err := DB.Exec(ctx,
		`INSERT INTO song_plays (split_id, plays) VALUES ($1, 1)
		 ON CONFLICT (split_id) DO UPDATE SET plays = song_plays.plays + 1, updated_at = now()`,
		splitID,
	)
	if err != nil {
		return err
	}

	return InsertPlayHistory(ctx, splitID)
}

// InsertPlayHistory appends one row to the play_history table recording that a
// split was played at the current time. Every play event gets its own row so
// the history can later be aggregated by frequency or recency.
func InsertPlayHistory(ctx context.Context, splitID int64) error {
	if DB == nil {
		return fmt.Errorf("nil db")
	}

	_, err := DB.Exec(ctx,
		`INSERT INTO play_history (split_id) VALUES ($1)`,
		splitID,
	)
	return err
}

// SetRating records a like or dislike for a split. A like (wasLiked=true)
// increments the rating counter and a dislike (wasLiked=false) decrements it.
// Existing play counts are preserved.
func SetRating(ctx context.Context, splitID int64, wasLiked bool) error {
	if DB == nil {
		return fmt.Errorf("nil db")
	}

	delta := 1
	if !wasLiked {
		delta = -1
	}
	_, err := DB.Exec(ctx,
		`INSERT INTO song_plays (split_id, plays, rating) VALUES ($1, 0, $2)
		 ON CONFLICT (split_id) DO UPDATE SET rating = song_plays.rating + $2, updated_at = now()`,
		splitID, delta,
	)
	return err
}

// FetchSongStats returns the play count and rating (like count) for a split,
// or zero values when the split has never been played or rated.
func FetchSongStats(ctx context.Context, splitID int64) (plays int, rating int, err error) {
	if DB == nil {
		return 0, 0, fmt.Errorf("nil db")
	}

	err = DB.QueryRow(ctx,
		`SELECT plays, rating FROM song_plays WHERE split_id = $1`,
		splitID,
	).Scan(&plays, &rating)
	if err == pgx.ErrNoRows {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	return plays, rating, nil
}

// NextSplitPositions returns n positions not yet used by any split of a
// recording, so newly created splits never collide with existing ones.
func NextSplitPositions(ctx context.Context, recordingID int64, n int) ([]int, error) {
	if DB == nil {
		return nil, fmt.Errorf("nil db")
	}

	var maxPos int64
	if err := DB.QueryRow(ctx,
		`SELECT COALESCE(MAX(position), 0) FROM splits WHERE recording_id = $1`,
		recordingID,
	).Scan(&maxPos); err != nil {
		return nil, err
	}

	pos := make([]int, n)
	next := int(maxPos)
	for i := range pos {
		next++
		pos[i] = next
	}
	return pos, nil
}

// FetchUserByName returns the user with the given name, or pgx.ErrNoRows when
// no such user exists.
func FetchUserByName(ctx context.Context, name string) (models.User, error) {
	if DB == nil {
		return models.User{}, fmt.Errorf("nil db")
	}

	var u models.User
	err := DB.QueryRow(ctx,
		`SELECT id, name, password FROM users WHERE name = $1`,
		name,
	).Scan(&u.ID, &u.Name, &u.Password)
	if err != nil {
		return models.User{}, err
	}
	return u, nil
}

// CreateUser stores a new user with the given name and password. The password
// is bcrypt-hashed before being written. It is an error if a user with the
// same name already exists (the name column is unique).
func CreateUser(ctx context.Context, name, password string) error {
	if DB == nil {
		return fmt.Errorf("nil db")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	_, err = DB.Exec(ctx,
		`INSERT INTO users (name, password) VALUES ($1, $2)`,
		name, string(hash),
	)
	return err
}

// UserPasswordMatches reports whether the given plaintext password matches the
// stored bcrypt hash for the user.
func UserPasswordMatches(u models.User, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(u.Password), []byte(password)) == nil
}
