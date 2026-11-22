package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"

	"github.com/Sniphs98/CatConnect/internal/host"
	"github.com/Sniphs98/CatConnect/internal/presets"
)

//go:embed web
var webFS embed.FS

// handler serves the admin UI and its JSON API behind HTTP basic auth.
func (s *server) handler(password string) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(static))

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		state := struct {
			Version  string            `json:"version"`
			Name     string            `json:"name"`
			Link     string            `json:"link"`
			Services []host.Service    `json:"services"`
			Clients  []host.ClientInfo `json:"clients"`
			Presets  []presets.Preset  `json:"presets"`
			UDPDrops int64             `json:"udpDrops"`
		}{
			Version:  version,
			Name:     s.cfg.Name,
			Link:     s.h.Link(),
			Services: s.cfg.Services,
			Clients:  s.h.Clients(),
			Presets:  presets.All(),
			UDPDrops: s.h.UDPDrops(),
		}
		s.mu.Unlock()
		writeJSON(w, state)
	})

	mux.HandleFunc("PUT /api/config", func(w http.ResponseWriter, r *http.Request) {
		var c Config
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&c); err != nil {
			httpError(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := s.setConfig(c); err != nil {
			httpError(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("POST /api/block", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Key     string `json:"key"`
			Blocked bool   `json:"blocked"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			httpError(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := s.setBlocked(req.Key, req.Blocked); err != nil {
			httpError(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("POST /api/reset-link", func(w http.ResponseWriter, r *http.Request) {
		if err := s.resetLink(r.Context()); err != nil {
			httpError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	return basicAuth(password, securityHeaders(mux))
}

func basicAuth(password string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pw, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pw), []byte(password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="CatConnect (any user name, admin password)"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; img-src 'self' data:")
		// State-changing requests must come from the UI itself.
		if r.Method != http.MethodGet && r.Header.Get("X-CatConnect") != "1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
