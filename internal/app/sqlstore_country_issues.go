package app

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type missingCountryOptions struct {
	Limit        int
	LookupStatus string
	Cursor       diveCursor
}

// A ready cache result awaiting reconciliation is still pending for this dive.
// No provider call is made from this read-only query.
const missingCountryStatus = `CASE
	WHEN dives.country_lookup_key='' THEN 'missing_coordinates'
	WHEN lookup.status IN ('not_found','error') THEN lookup.status
	ELSE 'pending' END`

func (s *sqlStore) DivesMissingCountry(ctx context.Context, options missingCountryOptions) ([]missingCountryDive, bool, error) {
	where := []string{`COALESCE(dives.country_override,dives.country_code,'')=''`}
	var args []any
	if options.LookupStatus != "" && options.LookupStatus != "all" {
		where = append(where, "("+missingCountryStatus+")=?")
		args = append(args, options.LookupStatus)
	}
	if cursor := options.Cursor; cursor.ID != 0 {
		where = append(where, `(dives.local_date<? OR (dives.local_date=? AND
			(dives.start_time<? OR (dives.start_time=? AND dives.id<?))))`)
		args = append(args, cursor.LocalDate, cursor.LocalDate, cursor.StartTime, cursor.StartTime, cursor.ID)
	}
	args = append(args, options.Limit+1)
	query := `SELECT ` + diveSummaryColumns + `, ` + missingCountryStatus + `,
		lookup.latitude,lookup.longitude,lookup.last_error,lookup.attempts,lookup.updated_at
		` + diveSummaryFrom + ` LEFT JOIN country_lookup_cache AS lookup ON lookup.lookup_key=dives.country_lookup_key
		WHERE ` + strings.Join(where, " AND ") + ` ORDER BY dives.local_date DESC,dives.start_time DESC,dives.id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, s.bind(query), args...)
	if err != nil {
		return nil, false, fmt.Errorf("list dives missing country: %w", err)
	}
	defer rows.Close()
	items := []missingCountryDive{}
	for rows.Next() {
		var item missingCountryDive
		var lat, lon sql.NullFloat64
		var errorCode sql.NullString
		var attempts, checked sql.NullInt64
		dive := newDiveSummaryScan()
		if err := rows.Scan(dive.targets(&item.Lookup.Status, &lat, &lon, &errorCode, &attempts, &checked)...); err != nil {
			return nil, false, fmt.Errorf("read dive missing country: %w", err)
		}
		item.Dive, err = dive.summary()
		if err != nil {
			return nil, false, err
		}
		item.Lookup.Latitude, item.Lookup.Longitude = nullFloatPointer(lat), nullFloatPointer(lon)
		item.Lookup.ErrorCode, item.Lookup.Attempts = errorCode.String, int(attempts.Int64)
		if checked.Valid && checked.Int64 > 0 {
			item.Lookup.CheckedAtMS = nullInt64Pointer(checked)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("list dives missing country: %w", err)
	}
	more := len(items) > options.Limit
	if more {
		items = items[:options.Limit]
	}
	return items, more, nil
}
