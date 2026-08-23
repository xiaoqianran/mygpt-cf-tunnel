package agent

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const serviceVersion = "0.6.0"
const openAPIContractVersion = "0.6.0"
const maxRequestBodyBytes = 16 << 20

type Server struct {
	cfg   Config
	jobs  *jobStore
	files *outputFileStore
}

func NewServer(cfg Config) *Server {
	return &Server{cfg: cfg, jobs: newJobStore(), files: newOutputFileStore()}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /openapi.json", s.handleOpenAPI)
	mux.HandleFunc("GET /v1/output-files/{token}", s.handleOutputFile)
	mux.Handle("POST /v1/command/run", s.CommandEndpoint())
	mux.Handle("POST /v1/command/start", s.auth(http.HandlerFunc(s.handleStartCommand)))
	mux.Handle("GET /v1/command/jobs/{id}", s.auth(http.HandlerFunc(s.handleGetCommandJob)))
	mux.Handle("POST /v1/command/jobs/{id}/cancel", s.auth(http.HandlerFunc(s.handleCancelCommandJob)))
	return requestLogger(mux)
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(started).Round(time.Millisecond))
	})
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := strings.TrimSpace(r.Header.Get("Authorization"))
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			writeError(w, http.StatusUnauthorized, "Bearer API token required")
			return
		}
		got := []byte(strings.TrimSpace(strings.TrimPrefix(header, prefix)))
		want := []byte(s.cfg.APIToken)
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid API token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("invalid JSON body: multiple JSON values are not allowed")
		}
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "mygpt-universal-vps-shell",
		"version": serviceVersion,
	})
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	origin := requestOrigin(r)
	spec := strings.ReplaceAll(openAPISpec, "__SERVER_URL__", origin)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(spec))
}
