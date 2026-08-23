package agent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// GPT Actions can return at most 10 files and each file may be up to 10 MB.
	// Keep a safety margin below the documented per-file ceiling.
	maxActionOutputFiles = 10
	maxActionFileBytes   = int64(9_500_000)
	outputFileURLTTL     = 15 * time.Minute
	// Keep ordinary JSON action responses comfortably below the documented
	// 100,000-character payload ceiling. File attachments carry larger output.
	maxActionJSONBytes = 95_000
)

type capturePart struct {
	Path string
	Size int64
}

type capturePool struct {
	mu        sync.Mutex
	partsUsed int
	maxParts  int
}

func newCapturePool() *capturePool {
	return &capturePool{maxParts: maxActionOutputFiles}
}

func (p *capturePool) newPart() (*os.File, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.partsUsed >= p.maxParts {
		return nil, false
	}
	f, err := os.CreateTemp("", "mygpt-command-output-*.txt")
	if err != nil {
		return nil, false
	}
	p.partsUsed++
	return f, true
}

type outputFileRef struct {
	Path           string
	Filename       string
	ExpiresAt      time.Time
	DeleteOnExpiry bool
}

type outputFileStore struct {
	mu    sync.Mutex
	files map[string]outputFileRef
}

func newOutputFileStore() *outputFileStore {
	return &outputFileStore{files: make(map[string]outputFileRef)}
}

func newOutputFileToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *outputFileStore) register(path, filename string, deleteOnExpiry bool) (string, error) {
	token, err := newOutputFileToken()
	if err != nil {
		return "", err
	}
	ref := outputFileRef{
		Path:           path,
		Filename:       filepath.Base(filename),
		ExpiresAt:      time.Now().Add(outputFileURLTTL),
		DeleteOnExpiry: deleteOnExpiry,
	}
	s.mu.Lock()
	s.files[token] = ref
	s.mu.Unlock()

	time.AfterFunc(outputFileURLTTL, func() {
		s.mu.Lock()
		current, ok := s.files[token]
		if ok {
			delete(s.files, token)
		}
		s.mu.Unlock()
		if ok && current.DeleteOnExpiry {
			_ = os.Remove(current.Path)
		}
	})
	return token, nil
}

func (s *outputFileStore) get(token string) (outputFileRef, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := s.files[token]
	if !ok {
		return outputFileRef{}, false
	}
	if time.Now().After(ref.ExpiresAt) {
		delete(s.files, token)
		if ref.DeleteOnExpiry {
			_ = os.Remove(ref.Path)
		}
		return outputFileRef{}, false
	}
	return ref, true
}

func (s *Server) handleOutputFile(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.files.get(strings.TrimSpace(r.PathValue("token")))
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(ref.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": ref.Filename}))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, ref.Filename, info.ModTime(), f)
}

func requestOrigin(r *http.Request) string {
	scheme := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + r.Host
}

func outputFileURL(origin, token string) string {
	return strings.TrimRight(origin, "/") + "/v1/output-files/" + token
}

func commandOutputFilename(stream string, part int) string {
	return fmt.Sprintf("command-%s-part-%02d.txt", stream, part+1)
}
