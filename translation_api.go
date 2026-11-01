package main

import (
	"net/http"
)

func (s *apiServer) registerTranslationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/translations/status", s.translationStatus)
	mux.HandleFunc("POST /api/v1/translations/retry", s.retryTranslations)
}

func (s *apiServer) writeTranslatedJSON(w http.ResponseWriter, r *http.Request, status int, value any) {
	var targets []translationTarget
	var finish []func()
	addMedia := func(media *mediaItem) {
		targets = append(targets, translationTarget{
			source:      translationSource{Kind: "media", Caption: media.Caption, Description: media.Description},
			destination: &media.Translations.English,
		})
	}
	addDive := func(dive *diveSummary) {
		targets = append(targets, translationTarget{
			source:      translationSource{Kind: "dive", Comment: dive.Comment, Highlights: dive.Highlights},
			destination: &dive.Translations.English,
		})
		if dive.CoverPhoto != nil {
			targets = append(targets, translationTarget{
				source:      translationSource{Kind: "media", Caption: dive.CoverPhoto.caption, Description: dive.CoverPhoto.Description},
				destination: &dive.CoverPhoto.Translations.English, cover: true,
			})
		}
	}
	addTrip := func(t *diveTrip) {
		targets = append(targets, translationTarget{source: t.translationSource(), destination: &t.Translations.English})
		if t.CoverPhoto != nil {
			addMedia(t.CoverPhoto)
		}
		finish = append(finish, func() {
			if t.NameEN != "" {
				t.Translations.English.Name = t.NameEN
			} else if t.Translations.English.Status == "ready" {
				t.NameEN = t.Translations.English.Name
			}
		})
	}
	switch response := value.(type) {
	case *diveListResponse:
		for i := range response.Items {
			addDive(&response.Items[i])
		}
	case *diveDetail:
		addDive(&response.Dive)
		for i := range response.Media {
			addMedia(&response.Media[i])
		}
	case *diveSummary:
		addDive(response)
	case *mediaListResponse:
		for i := range response.Items {
			addMedia(&response.Items[i])
		}
	case *mediaItem:
		addMedia(response)
	case *diveTrip:
		addTrip(response)
	case *tripListResponse:
		for i := range response.Items {
			addTrip(&response.Items[i])
		}
	case *tripDetail:
		addTrip(&response.Trip)
		for i := range response.Media {
			addMedia(&response.Media[i])
		}
		for i := range response.FavoriteDiveMedia {
			addMedia(&response.FavoriteDiveMedia[i])
		}
	}
	if err := s.store.translationService().populate(r.Context(), targets); err != nil {
		s.internalError(w, r, err)
		return
	}
	for _, complete := range finish {
		complete()
	}
	writeJSON(w, status, value)
}

type translationStatus struct {
	Enabled         bool   `json:"enabled"`
	Provider        string `json:"provider"`
	SourceLanguage  string `json:"source_language"`
	TargetLanguage  string `json:"target_language"`
	TargetLocale    string `json:"target_locale"`
	Pending         int64  `json:"pending"`
	Ready           int64  `json:"ready"`
	Failed          int64  `json:"failed"`
	CachedRevisions int64  `json:"cached_revisions"`
	BlockedReason   string `json:"blocked_reason,omitempty"`
}

func (s *apiServer) translationStatus(w http.ResponseWriter, r *http.Request) {
	service := s.store.translationService()
	result := translationStatus{Enabled: service.client != nil, Provider: "deepl", SourceLanguage: "fr",
		TargetLanguage: "en", TargetLocale: service.config.locale, BlockedReason: service.blockedReason()}
	err := s.store.db.QueryRowContext(r.Context(), s.store.bind(`SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN active=1 AND status='pending' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN active=1 AND status='ready' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN active=1 AND status='error' THEN 1 ELSE 0 END),0)
		FROM translation_cache WHERE profile = ?`), service.profile).Scan(&result.CachedRevisions, &result.Pending, &result.Ready, &result.Failed)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *apiServer) retryTranslations(w http.ResponseWriter, r *http.Request) {
	service := s.store.translationService()
	if service.client == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "translation_disabled", "configure DEEPL_API_KEY and restart serve to enable translation")
		return
	}
	if err := service.Retry(r.Context()); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}
