package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

var errInvalidDivePatch = errors.New("invalid dive metadata")

type divePatch struct {
	CoverMediaID json.RawMessage `json:"cover_media_id"`
	Country      json.RawMessage `json:"country_code"`
	TripID       json.RawMessage `json:"trip_id"`
	TripMode     *string         `json:"trip_mode"`
}

func (s *sqlStore) patchDiveTravel(ctx context.Context, id int64, p divePatch) (diveSummary, error) {
	invalid := func(message string) (diveSummary, error) {
		return diveSummary{}, fmt.Errorf("%w: %s", errInvalidDivePatch, message)
	}
	if len(p.CoverMediaID) == 0 && len(p.Country) == 0 && len(p.TripID) == 0 && p.TripMode == nil {
		return invalid("provide cover_media_id, country_code, trip_id, or trip_mode")
	}
	var cover, code, trip *string
	if len(p.CoverMediaID) > 0 {
		if json.Unmarshal(p.CoverMediaID, &cover) != nil || (cover != nil && *cover == "") {
			return invalid("cover_media_id must be a photo ID or null")
		}
	}
	if len(p.Country) > 0 {
		if json.Unmarshal(p.Country, &code) != nil {
			return invalid("country_code must be a country or null to restore automatic lookup")
		}
		if code != nil {
			c, err := resolveCountry(*code)
			if err != nil {
				return invalid(err.Error())
			}
			code = &c.Code
		}
	}
	if len(p.TripID) > 0 {
		if json.Unmarshal(p.TripID, &trip) != nil || (trip != nil && !validTripID(*trip)) {
			return invalid("trip_id must be a trip ID or null to exclude this dive")
		}
	}
	if p.TripMode != nil && (*p.TripMode != "auto" || len(p.TripID) > 0) {
		return invalid("trip_mode must be auto and cannot be combined with trip_id")
	}
	tx, err := s.beginTravelTx(ctx)
	if err != nil {
		return diveSummary{}, err
	}
	defer tx.Rollback()
	var existing int64
	if err := tx.QueryRowContext(ctx, s.bind(`SELECT id FROM dives WHERE id=?`), id).Scan(&existing); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return diveSummary{}, errDiveNotFound
		}
		return diveSummary{}, err
	}
	if len(p.CoverMediaID) > 0 {
		if cover != nil {
			var count int
			if err := tx.QueryRowContext(ctx, s.bind(`SELECT COUNT(*) FROM dive_media WHERE id=? AND dive_id=? AND kind='photo'`), *cover, id).Scan(&count); err != nil {
				return diveSummary{}, err
			}
			if count != 1 {
				return diveSummary{}, errInvalidCover
			}
		}
		if _, err := tx.ExecContext(ctx, s.bind(`UPDATE dives SET cover_media_id=? WHERE id=?`), cover, id); err != nil {
			return diveSummary{}, err
		}
	}
	if len(p.Country) > 0 {
		if _, err := tx.ExecContext(ctx, s.bind(`UPDATE dives SET country_override=? WHERE id=?`), code, id); err != nil {
			return diveSummary{}, err
		}
	}
	if len(p.TripID) > 0 {
		mode := "excluded"
		if trip != nil {
			var count int
			if err := tx.QueryRowContext(ctx, s.bind(`SELECT COUNT(*) FROM dive_trips WHERE id=? AND deleted=0`), *trip).Scan(&count); err != nil {
				return diveSummary{}, err
			}
			if count != 1 {
				return diveSummary{}, errTripNotFound
			}
			mode = "manual"
		}
		if _, err := tx.ExecContext(ctx, s.bind(`UPDATE dives SET trip_id=?,trip_assignment=? WHERE id=?`), trip, mode, id); err != nil {
			return diveSummary{}, err
		}
	}
	if p.TripMode != nil {
		if _, err := tx.ExecContext(ctx, s.bind(`UPDATE dives SET trip_id=NULL,trip_assignment='auto' WHERE id=?`), id); err != nil {
			return diveSummary{}, err
		}
	}
	if err := reconcileCountriesTx(ctx, s, tx); err != nil {
		return diveSummary{}, err
	}
	if err := reconcileTripsTx(ctx, s, tx); err != nil {
		return diveSummary{}, err
	}
	if err := tx.Commit(); err != nil {
		return diveSummary{}, err
	}
	s.translationsChanged()
	return scanDiveSummary(s.db.QueryRowContext(ctx, s.bind(`SELECT `+diveSummaryColumns+` `+diveSummaryFrom+` WHERE dives.id=?`), id))
}
