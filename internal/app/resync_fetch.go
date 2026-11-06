package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	garmin "github.com/se0wtf/go-garminconnect"
)

// This interface deliberately has no FIT download or activity-list operation.
type resyncClient interface {
	GetDiveDetails(context.Context, int64) (json.RawMessage, error)
}

type resyncFailure struct {
	code, message string
	fatal         bool
}

func (e *resyncFailure) Error() string { return e.message }

func classifyResyncError(err error) *resyncFailure {
	var failure *resyncFailure
	if errors.As(err, &failure) {
		return failure
	}
	var remote *garmin.HTTPError
	switch {
	case errors.Is(err, garmin.ErrSessionExpired), errors.Is(err, garmin.ErrInvalidCredentials), errors.Is(err, garmin.ErrMFARequired), errors.Is(err, garmin.ErrNoTokenFile):
		return &resyncFailure{"garmin_authentication_failed", "Garmin credentials are unavailable or expired; configure the Personal API key or renew the server's token cache", true}
	case errors.As(err, &remote):
		switch remote.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return &resyncFailure{"garmin_authentication_failed", "Garmin rejected access; check the server's Garmin credentials", true}
		case http.StatusTooManyRequests:
			return &resyncFailure{"garmin_rate_limited", "Garmin rate limit persisted after paced retries; remaining activities were not requested", true}
		case http.StatusNotFound:
			return &resyncFailure{"garmin_not_found", "Garmin did not return this activity; the stored dive was retained", false}
		}
	}
	// Never expose provider response bodies, URLs, tokens or SQL connection text.
	return &resyncFailure{"garmin_unavailable", "The Garmin activity could not be retrieved; the stored dive was retained", false}
}

func fetchResyncMetadata(ctx context.Context, client resyncClient, id int64) (DiveRecord, error) {
	invalid := &resyncFailure{"invalid_garmin_data", "Garmin returned invalid activity data; the stored dive was retained", false}
	rawDive, err := client.GetDiveDetails(ctx, id)
	if err != nil {
		return DiveRecord{}, err
	}
	var personal personalDiveDocument
	if err := json.Unmarshal(rawDive, &personal); err != nil {
		return DiveRecord{}, invalid
	}
	rawActivity := rawDive
	if personal.ID == 0 {
		var association struct {
			ConnectID int64 `json:"connectId"`
		}
		if json.Unmarshal(rawDive, &association) != nil || association.ConnectID != 0 && association.ConnectID != id {
			return DiveRecord{}, invalid
		}
		getter, ok := client.(activityGetter)
		if !ok {
			return DiveRecord{}, invalid
		}
		rawActivity, err = getter.GetActivity(ctx, id)
		if err != nil {
			return DiveRecord{}, err
		}
	}
	var activity struct {
		ID           int64               `json:"activityId"`
		Type         garmin.ActivityType `json:"activityType"`
		PersonalType garmin.ActivityType `json:"activityTypeDTO"`
	}
	if json.Unmarshal(rawActivity, &activity) != nil || activity.ID != id || (!isDive(activity.Type) && !isDive(activity.PersonalType)) {
		return DiveRecord{}, invalid
	}
	activityDocument, err := rawDocument(rawActivity)
	if err != nil {
		return DiveRecord{}, invalid
	}
	diveDocument, err := rawDocument(rawDive)
	if err != nil || diveDocument == nil {
		return DiveRecord{}, invalid
	}
	return DiveRecord{ID: id, Activity: activityDocument, DiveDetails: diveDocument}, nil
}
