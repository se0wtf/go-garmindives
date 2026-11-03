package app

import (
	"net/http"

	httpx "github.com/se0wtf/go-garmindives/internal/httpx"
)

type diveYearsResponse struct {
	Items []int `json:"items"`
}

func (s *apiServer) listDiveYears(w http.ResponseWriter, r *http.Request) {
	years, err := s.store.DiveYears(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, diveYearsResponse{Items: years})
}
