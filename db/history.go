package db

import (
	"context"
	"database/sql"
	"fmt"
)

// historySelect is the shared SELECT prefix for the play history queries. It
// joins play_history to splits and recordings so each entry carries the full
// split row, the radio name, the aggregated play count, and the first/last
// played timestamps. The GROUP BY lists every non-aggregate column so the query
// is valid on any PostgreSQL-compatible engine.
const historySelect = `
	SELECT s.id, s.recording_id, s.source_path, s.position, s.start_seconds, s.end_seconds, s.output_path, s.classification, s.custom_title,
	       r.radio,
	       COUNT(*) AS plays,
	       MAX(ph.played_at) AS last_played,
	       MIN(ph.played_at) AS first_played
	FROM play_history ph
	JOIN splits s ON s.id = ph.split_id
	JOIN recordings r ON r.id = s.recording_id
	GROUP BY s.id, s.recording_id, s.source_path, s.position, s.start_seconds, s.end_seconds, s.output_path, s.classification, s.custom_title, r.radio`

// scanHistoryRows scans the shared historySelect result columns into
// HistoryEntry values.
func scanHistoryRows(rows *sql.Rows) ([]HistoryEntry, error) {
	var entries []HistoryEntry
	for rows.Next() {
		var e HistoryEntry
		if err := rows.Scan(
			&e.Split.ID, &e.Split.RecordingID, &e.Split.SourcePath, &e.Split.Index,
			&e.Split.Start, &e.Split.End, &e.Split.OutputPath, &e.Split.Classification, &e.Split.CustomTitle,
			&e.Radio,
			&e.Plays,
			&e.LastPlayed,
			&e.FirstPlayed,
		); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// FetchPlayHistoryRecency returns the distinct songs in the play history,
// ordered by when they were last played (most recent first), limited to limit
// entries.
func FetchPlayHistoryRecency(ctx context.Context, limit int) ([]HistoryEntry, error) {
	if DB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := DB.QueryContext(ctx, historySelect+`
		ORDER BY last_played DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanHistoryRows(rows)
}

// FetchPlayHistoryFrequency returns the distinct songs in the play history,
// ordered by how often they were played (most played first), with the most
// recently played song winning ties. Limited to limit entries.
func FetchPlayHistoryFrequency(ctx context.Context, limit int) ([]HistoryEntry, error) {
	if DB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := DB.QueryContext(ctx, historySelect+`
		ORDER BY plays DESC, last_played DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanHistoryRows(rows)
}

// FetchPlayHistoryGrouped returns the play history grouped by radio, ordered by
// radio name then play count. Limited to limit entries total.
func FetchPlayHistoryGrouped(ctx context.Context, limit int) ([]RadioHistoryGroup, error) {
	if DB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := DB.QueryContext(ctx, historySelect+`
		ORDER BY r.radio ASC, plays DESC, last_played DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	entries, err := scanHistoryRows(rows)
	if err != nil {
		return nil, err
	}

	order := []string{}
	byRadio := map[string][]HistoryEntry{}
	for _, e := range entries {
		if _, ok := byRadio[e.Radio]; !ok {
			order = append(order, e.Radio)
		}
		byRadio[e.Radio] = append(byRadio[e.Radio], e)
	}

	groups := make([]RadioHistoryGroup, 0, len(order))
	for i, radio := range order {
		groups = append(groups, RadioHistoryGroup{
			Radio: radio,
			Color: RadioPalette[i%len(RadioPalette)],
			Plays: byRadio[radio],
		})
	}
	return groups, nil
}

// RadioPalette assigns a stable color to each distinct radio.
var RadioPalette = []string{
	"#6366f1", // indigo
	"#ec4899", // pink
	"#10b981", // emerald
	"#f59e0b", // amber
	"#06b6d4", // cyan
	"#8b5cf6", // violet
	"#ef4444", // red
	"#14b8a6", // teal
}
