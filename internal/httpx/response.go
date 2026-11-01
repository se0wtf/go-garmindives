package httpx

import (
	"encoding/json"
	"net/http"
)

type ErrorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func WriteAPIError(w http.ResponseWriter, status int, code, message string) {
	response := ErrorResponse{}
	response.Error.Code = code
	response.Error.Message = message
	WriteJSON(w, status, response)
}

func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
