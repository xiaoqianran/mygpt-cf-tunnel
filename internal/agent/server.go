package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/xiaoqianran/mygpt-cf-tunnel/internal/audit"
)

//go:embed openapi.json
var openAPITemplate []byte

type Server struct {
	cfg             Config
	log             *slog.Logger
	sessions        *sessionStore
	artifacts       *artifactStore
	audit           *audit.Recorder
	commandCache    *commandCache
	uploadTransport http.RoundTripper
	mux             *http.ServeMux
}

type requestMeta struct {
	requestID, conversationID, userID, gptID, baseURL string
}

type requestIDKey struct{}
type auditTraceKey struct{}

type responseStats struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *responseStats) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseStats) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += n
	return n, err
}

func New(cfg Config, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	sessions, err := newSessionStore(cfg.StateDir, cfg.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	artifacts, err := newArtifactStore(cfg.StateDir, cfg.APIToken, cfg.ArtifactTTL, cfg.MaxArtifactBytes)
	if err != nil {
		return nil, err
	}
	var sink audit.Sink = audit.NopSink{}
	if cfg.AuditEnabled {
		sink, err = audit.NewFileSink(cfg.AuditDir, cfg.AuditRetentionDays, cfg.AuditFsync)
		if err != nil {
			return nil, fmt.Errorf("initialize audit sink: %w", err)
		}
	}
	s := &Server{
		cfg: cfg, log: log, sessions: sessions, artifacts: artifacts,
		audit: audit.NewRecorder(sink), commandCache: newCommandCache(),
		uploadTransport: http.DefaultTransport, mux: http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /health", s.health)
	s.mux.HandleFunc("GET /openapi.json", s.openapi)
	s.mux.Handle("/v1/files/download/", artifacts)
	s.mux.HandleFunc("POST /v1/command/run", s.runCommand)
	return s, nil
}

func (s *Server) Close() error {
	return s.audit.Close()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID := trustedRequestID(r)
	if requestID == "" {
		var err error
		requestID, err = randomID()
		if err != nil {
			requestID = "unavailable"
		}
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Request-Id", requestID)
	stats := &responseStats{ResponseWriter: w}
	ctx := context.WithValue(r.Context(), requestIDKey{}, requestID)
	var trace *audit.Trace
	if r.URL.Path != "/health" {
		trace = s.audit.NewTrace(requestID)
	}
	ctx = context.WithValue(ctx, auditTraceKey{}, trace)
	s.auditEvent(ctx, "request.received", "started", map[string]any{
		"method": r.Method, "path": r.URL.Path, "host": r.Host,
		"remote_ip": clientIP(r), "content_length": r.ContentLength,
		"authorization_present":    r.Header.Get("Authorization") != "",
		"conversation_id_sha256":   hashIdentifier(r.Header.Get("Openai-Conversation-Id")),
		"ephemeral_user_id_sha256": hashIdentifier(r.Header.Get("Openai-Ephemeral-User-Id")),
		"gpt_id":                   strings.TrimSpace(r.Header.Get("Openai-Gpt-Id")),
		"user_agent":               truncateText(r.UserAgent(), 256),
	})
	s.mux.ServeHTTP(stats, r.WithContext(ctx))
	if stats.status == 0 {
		stats.status = http.StatusOK
	}
	s.auditEvent(ctx, "request.completed", outcomeForStatus(stats.status), map[string]any{
		"status": stats.status, "response_bytes": stats.bytes,
		"duration_ms": time.Since(started).Milliseconds(),
	})
	if r.URL.Path == "/health" && stats.status < http.StatusBadRequest {
		return
	}
	fields := []any{
		"request_id", requestID,
		"method", r.Method,
		"path", r.URL.Path,
		"status", stats.status,
		"bytes", stats.bytes,
		"duration_ms", time.Since(started).Milliseconds(),
		"authorization_present", r.Header.Get("Authorization") != "",
		"gpt_id_present", r.Header.Get("Openai-Gpt-Id") != "",
	}
	if stats.status >= http.StatusBadRequest {
		s.log.Warn("http request", fields...)
	} else {
		s.log.Info("http request", fields...)
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "mygpt-cf-tunnel", "version": Version})
}

func (s *Server) openapi(w http.ResponseWriter, r *http.Request) {
	baseURL, err := s.publicBaseURL(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "public URL is not configured")
		return
	}
	doc := strings.ReplaceAll(string(openAPITemplate), "{{ACTION_BASE_URL}}", baseURL)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, doc)
}

func (s *Server) runCommand(w http.ResponseWriter, r *http.Request) {
	if ok, reason := s.authorized(r); !ok {
		s.auditEvent(r.Context(), "authentication", "failed", map[string]any{"reason": reason})
		w.Header().Set("WWW-Authenticate", `Bearer realm="mygpt-cf-tunnel"`)
		writeError(w, http.StatusUnauthorized, "missing or invalid Bearer token")
		return
	}
	s.auditEvent(r.Context(), "authentication", "succeeded", nil)
	gptID := strings.TrimSpace(r.Header.Get("Openai-Gpt-Id"))
	if len(s.cfg.AllowedGPTIDs) > 0 {
		if _, ok := s.cfg.AllowedGPTIDs[strings.ToLower(gptID)]; !ok {
			s.auditEvent(r.Context(), "gpt_authorization", "failed", map[string]any{"gpt_id": gptID})
			writeError(w, http.StatusUnauthorized, "GPT is not allowed")
			return
		}
	}
	s.auditEvent(r.Context(), "gpt_authorization", "succeeded", map[string]any{"gpt_id": gptID})
	if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])); mediaType != "application/json" {
		s.auditEvent(r.Context(), "request.validation", "failed", map[string]any{"reason": "invalid_content_type", "content_type": mediaType})
		writeError(w, http.StatusBadRequest, "Content-Type must be application/json")
		return
	}
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	var req commandRequest
	if err := decoder.Decode(&req); err != nil {
		s.auditEvent(r.Context(), "request.validation", "failed", map[string]any{"reason": "invalid_json", "error": err.Error()})
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		s.auditEvent(r.Context(), "request.validation", "failed", map[string]any{"reason": "multiple_json_values"})
		writeError(w, http.StatusBadRequest, "request body must contain one JSON object")
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		s.auditEvent(r.Context(), "request.validation", "failed", map[string]any{"reason": "empty_command"})
		writeError(w, http.StatusBadRequest, "command is required")
		return
	}
	if req.CacheTTLSeconds < 0 || time.Duration(req.CacheTTLSeconds)*time.Second > maxCommandCacheTTL {
		s.auditEvent(r.Context(), "request.validation", "failed", map[string]any{"reason": "invalid_cache_ttl"})
		writeError(w, http.StatusBadRequest, "cache_ttl_seconds must be between 0 and 60")
		return
	}
	if req.CacheTTLSeconds > 0 && len(req.OpenAIFileIDRefs) > 0 {
		s.auditEvent(r.Context(), "request.validation", "failed", map[string]any{"reason": "cache_with_input_files"})
		writeError(w, http.StatusBadRequest, "cache_ttl_seconds cannot be used with openaiFileIdRefs")
		return
	}
	requestID, _ := r.Context().Value(requestIDKey{}).(string)
	if requestID == "" {
		generatedID, generateErr := randomID()
		if generateErr != nil {
			writeError(w, http.StatusInternalServerError, "cannot create request ID")
			return
		}
		requestID = generatedID
	}
	s.auditEvent(r.Context(), "request.validation", "succeeded", map[string]any{
		"command": req.Command, "command_bytes": len(req.Command), "command_sha256": hashText(req.Command),
		"stdin_bytes": len(req.Stdin), "stdin_sha256": hashText(req.Stdin),
		"requested_workdir": req.Workdir, "requested_timeout_seconds": req.TimeoutSeconds,
		"cache_ttl_seconds": req.CacheTTLSeconds, "file_ref_count": len(req.OpenAIFileIDRefs),
	})
	baseURL, err := s.publicBaseURL(r)
	if err != nil {
		s.auditEvent(r.Context(), "public_url", "failed", map[string]any{"error": err.Error()})
		writeError(w, http.StatusInternalServerError, "public URL is not configured")
		return
	}
	s.auditEvent(r.Context(), "public_url", "resolved", map[string]any{"base_url": baseURL})
	meta := requestMeta{
		requestID: requestID, conversationID: r.Header.Get("Openai-Conversation-Id"),
		userID: r.Header.Get("Openai-Ephemeral-User-Id"), gptID: gptID, baseURL: baseURL,
	}
	key := sessionKey(meta.conversationID, meta.userID)
	cacheScope := key
	if cacheScope != "" && meta.gptID != "" {
		cacheScope = meta.gptID + "\x00" + cacheScope
	}
	s.auditEvent(r.Context(), "session.lock", "waiting", map[string]any{"session_key_sha256": hashIdentifier(key)})
	unlock := s.sessions.lock(key)
	defer unlock()
	s.auditEvent(r.Context(), "session.lock", "acquired", map[string]any{"session_key_sha256": hashIdentifier(key)})

	startDir := s.sessions.get(key)
	cacheWorkdir, err := resolveWorkdir(startDir, req.Workdir)
	if err != nil {
		s.auditEvent(r.Context(), "execution.prepare", "failed", map[string]any{"requested_workdir": req.Workdir, "error": err.Error()})
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cacheGeneration := uint64(0)
	if req.CacheTTLSeconds > 0 && cacheScope != "" {
		if cached, age, generation, ok := s.commandCache.get(cacheScope, cacheWorkdir, req, time.Now()); ok {
			cached.CacheHit = true
			cached.CacheAgeMS = age.Milliseconds()
			cached.DurationMS = 0
			s.auditEvent(r.Context(), "command.cache", "hit", map[string]any{
				"cache_age_ms": cached.CacheAgeMS, "cache_ttl_seconds": req.CacheTTLSeconds,
			})
			s.log.Info("command cache hit", "request_id", requestID, "cache_age_ms", cached.CacheAgeMS)
			writeJSON(w, http.StatusOK, cached)
			return
		} else {
			cacheGeneration = generation
		}
		s.auditEvent(r.Context(), "command.cache", "miss", map[string]any{"cache_ttl_seconds": req.CacheTTLSeconds})
	} else if req.CacheTTLSeconds > 0 {
		s.auditEvent(r.Context(), "command.cache", "bypassed", map[string]any{"reason": "missing_session_identity"})
	} else {
		removed := s.commandCache.invalidateAll()
		s.auditEvent(r.Context(), "command.cache", "invalidated", map[string]any{"entry_count": removed, "scope": "global", "phase": "before_execution"})
	}

	timeout := requestTimeout(req.TimeoutSeconds, s.cfg.CommandTimeout)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	files, inputDir, err := s.downloadFiles(ctx, req.OpenAIFileIDRefs, requestID)
	if inputDir != "" {
		defer func() {
			err := os.RemoveAll(inputDir)
			s.auditEvent(r.Context(), "upload.cleanup", outcomeFromError(err), map[string]any{"file_count": len(files), "error": errorText(err)})
		}()
	}
	if err != nil {
		s.auditEvent(r.Context(), "upload.batch", "failed", map[string]any{"error": err.Error()})
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, err := s.execute(ctx, req, startDir, inputDir, files, meta)
	if req.CacheTTLSeconds == 0 {
		removed := s.commandCache.invalidateAll()
		s.auditEvent(r.Context(), "command.cache", "invalidated", map[string]any{"entry_count": removed, "scope": "global", "phase": "after_execution"})
	}
	if err != nil {
		s.auditEvent(r.Context(), "execution", "failed", map[string]any{"error": err.Error()})
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.sessions.set(key, resp.Workdir); err != nil {
		s.auditEvent(r.Context(), "session.persist", "failed", map[string]any{"workdir": resp.Workdir, "error": err.Error()})
		s.log.Error("persist session", "request_id", requestID, "error", err)
	} else {
		s.auditEvent(r.Context(), "session.persist", "succeeded", map[string]any{"workdir": resp.Workdir})
	}
	if req.CacheTTLSeconds > 0 && cacheScope != "" && resp.ExitCode == 0 && !resp.TimedOut && !resp.OutputTruncated && len(resp.OpenAIFileResponse) == 0 && resp.Workdir == cacheWorkdir {
		ttl := time.Duration(req.CacheTTLSeconds) * time.Second
		if s.commandCache.putIfGeneration(cacheScope, cacheWorkdir, req, resp, ttl, cacheGeneration, time.Now()) {
			s.auditEvent(r.Context(), "command.cache", "stored", map[string]any{"cache_ttl_seconds": req.CacheTTLSeconds})
		} else {
			s.auditEvent(r.Context(), "command.cache", "store_skipped", map[string]any{"reason": "generation_changed"})
		}
	} else if req.CacheTTLSeconds > 0 && resp.Workdir != cacheWorkdir {
		s.auditEvent(r.Context(), "command.cache", "store_skipped", map[string]any{"reason": "workdir_changed"})
	}
	s.log.Info("command finished", "request_id", requestID, "exit_code", resp.ExitCode,
		"timed_out", resp.TimedOut, "duration_ms", resp.DurationMS, "attachments", len(resp.OpenAIFileResponse))
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) authorized(r *http.Request) (bool, string) {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return false, "missing_or_invalid_scheme"
	}
	provided := []byte(parts[1])
	expected := []byte(s.cfg.APIToken)
	if len(provided) != len(expected) || subtle.ConstantTimeCompare(provided, expected) != 1 {
		return false, "invalid_token"
	}
	return true, ""
}

func (s *Server) auditEvent(ctx context.Context, stage, outcome string, data map[string]any) {
	if err := appendAudit(ctx, stage, outcome, data); err != nil {
		trace, _ := ctx.Value(auditTraceKey{}).(*audit.Trace)
		s.log.Error("append audit event", "request_id", trace.ID(), "stage", stage, "error", err)
	}
}

func appendAudit(ctx context.Context, stage, outcome string, data map[string]any) error {
	trace, _ := ctx.Value(auditTraceKey{}).(*audit.Trace)
	if trace == nil {
		return nil
	}
	return trace.Event(stage, outcome, compactData(data))
}

func trustedRequestID(r *http.Request) string {
	// 仅当请求来自可信本地源（loopback TCP 或 Unix Domain Socket）时才信任
	// 传入的 X-Request-Id / CF-Ray，防止公网伪造请求 ID。
	if !isLocalRequest(r) {
		return ""
	}
	id := strings.TrimSpace(r.Header.Get("X-Request-Id"))
	if id == "" {
		id = strings.TrimSpace(r.Header.Get("CF-Ray"))
	}
	if len(id) < 8 || len(id) > 128 {
		return ""
	}
	for _, char := range id {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == ':') {
			return ""
		}
	}
	return id
}

// isLocalRequest 判断请求是否来自可信的本地回源：Unix Domain Socket 或 loopback 地址。
// cloudflared 通过 Unix Socket 或 127.0.0.1 回源时视为可信。
func isLocalRequest(r *http.Request) bool {
	if r.RemoteAddr == "" || r.RemoteAddr == "@" { // Unix socket
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// 无端口的裸地址（可能为 Unix socket 的抽象命名空间）
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func clientIP(r *http.Request) string {
	// 优先信任 Cloudflare 回源注入的真实客户端地址。
	if cfIP := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cfIP != "" {
		return cfIP
	}
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); forwarded != "" {
		return forwarded
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func hashIdentifier(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return hashText(value)
}

func hashText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func truncateText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

func compactData(data map[string]any) map[string]any {
	for key, value := range data {
		if value == nil {
			delete(data, key)
			continue
		}
		if text, ok := value.(string); ok && text == "" {
			delete(data, key)
		}
	}
	if len(data) == 0 {
		return nil
	}
	return data
}

func outcomeForStatus(status int) string {
	if status >= 200 && status < 400 {
		return "succeeded"
	}
	return "failed"
}

func outcomeFromError(err error) string {
	if err == nil {
		return "succeeded"
	}
	return "failed"
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *Server) publicBaseURL(r *http.Request) (string, error) {
	if s.cfg.ActionBaseURL != "" {
		return s.cfg.ActionBaseURL, nil
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	if proto == "" && r.TLS != nil {
		proto = "https"
	}
	if proto != "https" || host == "" {
		return "", errors.New("no HTTPS public origin")
	}
	u := &url.URL{Scheme: "https", Host: host}
	if u.Hostname() == "" {
		return "", errors.New("invalid public origin")
	}
	return u.String(), nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}
