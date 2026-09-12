package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/mirainya/muxapi/internal/update"
)

func (s *Server) adminUpdates(w http.ResponseWriter, r *http.Request) {
	if s.updateSvc == nil {
		http.Error(w, "update service unavailable", http.StatusNotImplemented)
		return
	}
	switch r.Method {
	case http.MethodGet:
		catalog, err := s.updateSvc.Catalog(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, catalog)
	case http.MethodPost:
		var payload struct {
			Version string `json:"version"`
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
		if err != nil {
			http.Error(w, "读取更新请求失败", http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "更新版本参数无效", http.StatusBadRequest)
			return
		}
		payload.Version = strings.TrimSpace(payload.Version)
		if payload.Version == "" || strings.EqualFold(payload.Version, "dev") {
			http.Error(w, "更新版本不能为空", http.StatusBadRequest)
			return
		}
		result, err := s.updateSvc.Apply(r.Context(), payload.Version)
		if err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, update.ErrInProgress) {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(result)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
