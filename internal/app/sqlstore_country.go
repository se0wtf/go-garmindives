package app

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"

	appconfig "github.com/se0wtf/go-garmindives/internal/config"
	countrydata "github.com/se0wtf/go-garmindives/internal/country"
)

// DiveCountries lists effective destinations in the stored logbook. Overrides
// replace detected countries, and unresolved dives contribute no choice.
func (s *sqlStore) DiveCountries(ctx context.Context) ([]countrydata.Country, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT COALESCE(country_override,country_code)
		FROM dives WHERE COALESCE(country_override,country_code,'')<>''`)
	if err != nil {
		return nil, fmt.Errorf("list dive countries: %w", err)
	}
	defer rows.Close()
	items := []countrydata.Country{}
	seen := map[string]bool{}
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, fmt.Errorf("read dive country: %w", err)
		}
		if c := countrydata.ByCode(code); c != nil && !seen[c.Code] {
			items = append(items, *c)
			seen[c.Code] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list dive countries: %w", err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Code < items[j].Code })
	return items, nil
}

func coordinatesKey(lat, lon float64) string { return fmt.Sprintf("country-v1:%.6f,%.6f", lat, lon) }
func validCoordinates(lat, lon sql.NullFloat64) bool {
	return lat.Valid && lon.Valid && !math.IsNaN(lat.Float64) && !math.IsNaN(lon.Float64) && !math.IsInf(lat.Float64, 0) && !math.IsInf(lon.Float64, 0) && lat.Float64 >= -90 && lat.Float64 <= 90 && lon.Float64 >= -180 && lon.Float64 <= 180
}

// Populate/reuse a persistent queue without contacting the geocoder. This runs
// during migration, sync and background reconciliation, including GPS edits.
func reconcileCountriesTx(ctx context.Context, s *sqlStore, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,entry_latitude,entry_longitude,exit_latitude,exit_longitude,country_lookup_key,country_override FROM dives`)
	if err != nil {
		return err
	}
	type position struct {
		id       int64
		lat, lon sql.NullFloat64
		key      string
		override sql.NullString
	}
	var positions []position
	for rows.Next() {
		var p position
		var exitLat, exitLon sql.NullFloat64
		if err := rows.Scan(&p.id, &p.lat, &p.lon, &exitLat, &exitLon, &p.key, &p.override); err != nil {
			rows.Close()
			return err
		}
		if !validCoordinates(p.lat, p.lon) {
			p.lat, p.lon = exitLat, exitLon
		}
		positions = append(positions, p)
	}
	readErr, closeErr := rows.Err(), rows.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	for _, p := range positions {
		key := ""
		if validCoordinates(p.lat, p.lon) {
			key = coordinatesKey(p.lat.Float64, p.lon.Float64)
		}
		if key != p.key {
			if _, err := tx.ExecContext(ctx, s.bind(`UPDATE dives SET country_lookup_key=?,country_code=NULL WHERE id=?`), key, p.id); err != nil {
				return err
			}
		}
		if key != "" && !p.override.Valid {
			if _, err := tx.ExecContext(ctx, s.bind(`INSERT INTO country_lookup_cache (lookup_key,latitude,longitude) VALUES (?,?,?) ON CONFLICT (lookup_key) DO NOTHING`), key, p.lat.Float64, p.lon.Float64); err != nil {
				return err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE dives SET country_code = (SELECT country_code FROM country_lookup_cache WHERE lookup_key=dives.country_lookup_key)
		WHERE EXISTS (SELECT 1 FROM country_lookup_cache c WHERE c.lookup_key=dives.country_lookup_key AND c.status IN ('ready','not_found') AND COALESCE(c.country_code,'')<>COALESCE(dives.country_code,''))`)
	return err
}

func (s *sqlStore) reconcileTravel(ctx context.Context) error {
	tx, err := s.beginTravelTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := reconcileCountriesTx(ctx, s, tx); err != nil {
		return err
	}
	if err := reconcileTripsTx(ctx, s, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Serialise related country/residence/trip changes across PostgreSQL server
// processes as well as SQLite's single writer, so overlap checks and grouping
// operate on one committed snapshot. Never hold this lock during HTTP calls.
func (s *sqlStore) beginTravelTx(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if s.dialect == appconfig.Postgres {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(1734439538)`); err != nil {
			tx.Rollback()
			return nil, err
		}
	}
	return tx, nil
}
