package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

const (
	maxCommandCacheTTL     = 60 * time.Second
	maxCommandCacheEntries = 256
)

type commandCacheEntry struct {
	storedAt  time.Time
	expiresAt time.Time
	response  commandResponse
}

type commandCache struct {
	mu         sync.Mutex
	entries    map[string]commandCacheEntry
	generation uint64
}

func newCommandCache() *commandCache {
	return &commandCache{entries: make(map[string]commandCacheEntry)}
}

// get returns the cache generation observed atomically with the lookup. A miss
// can later be stored only if no uncached command invalidated the cache in the
// meantime.
func (c *commandCache) get(sessionKey, workdir string, req commandRequest, now time.Time) (commandResponse, time.Duration, uint64, bool) {
	if c == nil || sessionKey == "" {
		return commandResponse{}, 0, 0, false
	}
	key := commandCacheKey(sessionKey, workdir, req)
	c.mu.Lock()
	defer c.mu.Unlock()
	generation := c.generation
	entry, ok := c.entries[key]
	if !ok {
		return commandResponse{}, 0, generation, false
	}
	if !now.Before(entry.expiresAt) {
		delete(c.entries, key)
		return commandResponse{}, 0, generation, false
	}
	return cloneCommandResponse(entry.response), now.Sub(entry.storedAt), generation, true
}

// putIfGeneration stores a result only when no invalidating command ran after
// the corresponding cache lookup started.
func (c *commandCache) putIfGeneration(sessionKey, workdir string, req commandRequest, resp commandResponse, ttl time.Duration, generation uint64, now time.Time) bool {
	if c == nil || sessionKey == "" || ttl <= 0 {
		return false
	}
	if ttl > maxCommandCacheTTL {
		ttl = maxCommandCacheTTL
	}
	key := commandCacheKey(sessionKey, workdir, req)
	entry := commandCacheEntry{
		storedAt:  now,
		expiresAt: now.Add(ttl),
		response:  cloneCommandResponse(resp),
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation != generation {
		return false
	}
	c.pruneExpiredLocked(now)
	if _, exists := c.entries[key]; !exists && len(c.entries) >= maxCommandCacheEntries {
		c.evictOldestLocked()
	}
	c.entries[key] = entry
	return true
}

// invalidateAll increments the generation even when the cache is empty. That
// prevents a read that started before a write from publishing a stale result
// after the write has already invalidated existing entries.
func (c *commandCache) invalidateAll() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := len(c.entries)
	c.entries = make(map[string]commandCacheEntry)
	c.generation++
	return removed
}

func (c *commandCache) pruneExpiredLocked(now time.Time) {
	for key, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
}

func (c *commandCache) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for key, entry := range c.entries {
		if oldestKey == "" || entry.storedAt.Before(oldest) {
			oldestKey = key
			oldest = entry.storedAt
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

func commandCacheKey(sessionKey, workdir string, req commandRequest) string {
	payload, _ := json.Marshal(struct {
		SessionKey      string `json:"session_key"`
		Workdir         string `json:"workdir"`
		Command         string `json:"command"`
		Stdin           string `json:"stdin"`
		TimeoutSeconds  int    `json:"timeout_seconds"`
		CacheTTLSeconds int    `json:"cache_ttl_seconds"`
	}{
		SessionKey: sessionKey, Workdir: workdir, Command: req.Command,
		Stdin: req.Stdin, TimeoutSeconds: req.TimeoutSeconds, CacheTTLSeconds: req.CacheTTLSeconds,
	})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func cloneCommandResponse(resp commandResponse) commandResponse {
	cloned := resp
	cloned.InputFiles = append([]savedFile(nil), resp.InputFiles...)
	cloned.OpenAIFileResponse = append([]string(nil), resp.OpenAIFileResponse...)
	return cloned
}
