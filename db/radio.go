package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/hibooboo2/gradio/models"
)

// UpsertRadioStations inserts or refreshes the given radio stations in bulk.
// The stationuuid is the primary key, so re-syncing the same station replaces
// its metadata instead of creating a duplicate row.
func UpsertRadioStations(ctx context.Context, stations []models.RadioStation) error {
	if DB == nil {
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

	_, err := DB.Exec(ctx, sb.String(), args...)
	return err
}

// FetchRadioStations returns stations from the radio_stations table. When q is
// non-empty it is a name search (case-insensitive substring, LIKE wildcards
// escaped) ordered by name, limited to 50 rows. Otherwise it returns 20 random
// stations.
func FetchRadioStations(ctx context.Context, q string) ([]models.RadioStation, error) {
	if DB == nil {
		return nil, fmt.Errorf("nil db")
	}

	q = strings.TrimSpace(q)
	var query string
	var args []any
	if q != "" {
		query = `SELECT stationuuid, name, url_resolved, favicon, tags, countrycode, languagecodes
			FROM radio_stations
			WHERE name ILIKE '%' || $1 || '%' ESCAPE '\'
			ORDER BY name
			LIMIT 50`
		args = append(args, escapeLike(q))
	} else {
		query = `SELECT stationuuid, name, url_resolved, favicon, tags, countrycode, languagecodes
			FROM radio_stations
			order by random()
			LIMIT 20`
	}

	rows, err := DB.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stations []models.RadioStation
	for rows.Next() {
		var s models.RadioStation
		if err := rows.Scan(&s.StationUUID, &s.Name, &s.URLResolved, &s.Favicon, &s.Tags, &s.CountryCode, &s.LanguageCodes); err != nil {
			return nil, err
		}
		stations = append(stations, s)
	}

	return stations, rows.Err()
}

// escapeLike escapes LIKE wildcard characters so user input is matched
// literally.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// FetchRadioStationURLs returns every station url_resolved keyed by its name,
// so recording can pick a station by name.
func FetchRadioStationURLs(ctx context.Context) (map[string]string, error) {
	stations, err := FetchRadioStations(ctx, "")
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

// FetchRadioStationByUUID returns the station with the given uuid, or
// pgx.ErrNoRows when it does not exist.
func FetchRadioStationByUUID(ctx context.Context, uuid string) (models.RadioStation, error) {
	if DB == nil {
		return models.RadioStation{}, fmt.Errorf("nil db")
	}

	var s models.RadioStation
	err := DB.QueryRow(ctx,
		`SELECT stationuuid, name, url_resolved, favicon, tags, countrycode, languagecodes
		 FROM radio_stations
		 WHERE stationuuid = $1`,
		uuid,
	).Scan(&s.StationUUID, &s.Name, &s.URLResolved, &s.Favicon, &s.Tags, &s.CountryCode, &s.LanguageCodes)
	if err != nil {
		return models.RadioStation{}, err
	}
	return s, nil
}

// AddFavorite marks a station as a favorite. It is idempotent: re-favoriting
// an already-favorited station keeps the original favorited_at time.
func AddFavorite(ctx context.Context, stationuuid string) error {
	if DB == nil {
		return fmt.Errorf("nil db")
	}
	_, err := DB.Exec(ctx,
		`INSERT INTO favorites (stationuuid) VALUES ($1)
		 ON CONFLICT (stationuuid) DO NOTHING`,
		stationuuid,
	)
	return err
}

// RemoveFavorite unmarks a station as a favorite. It is a no-op when the
// station was not favorited.
func RemoveFavorite(ctx context.Context, stationuuid string) error {
	if DB == nil {
		return fmt.Errorf("nil db")
	}
	_, err := DB.Exec(ctx, `DELETE FROM favorites WHERE stationuuid = $1`, stationuuid)
	return err
}

// FetchFavoriteUUIDs returns the set of station uuids that are favorited.
func FetchFavoriteUUIDs(ctx context.Context) (map[string]struct{}, error) {
	if DB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := DB.Query(ctx, `SELECT stationuuid FROM favorites`)
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

// FetchFavoriteStations returns the favorited stations, ordered by when they
// were favorited, newest first.
func FetchFavoriteStations(ctx context.Context) ([]models.RadioStation, error) {
	if DB == nil {
		return nil, fmt.Errorf("nil db")
	}

	rows, err := DB.Query(ctx,
		`SELECT s.stationuuid, s.name, s.url_resolved, s.favicon, s.tags, s.countrycode, s.languagecodes
		 FROM favorites f
		 JOIN radio_stations s ON s.stationuuid = f.stationuuid
		 ORDER BY f.favorited_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stations []models.RadioStation
	for rows.Next() {
		var s models.RadioStation
		if err := rows.Scan(&s.StationUUID, &s.Name, &s.URLResolved, &s.Favicon, &s.Tags, &s.CountryCode, &s.LanguageCodes); err != nil {
			return nil, err
		}
		stations = append(stations, s)
	}
	return stations, rows.Err()
}
