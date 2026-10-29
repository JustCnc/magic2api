package main

import (
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type logEntry struct {
	At      int64  `json:"at"`
	Level   string `json:"level"`
	Scope   string `json:"scope"`
	Message string `json:"message"`
}

type logRing struct {
	mu      sync.Mutex
	entries []logEntry
	max     int
}

func newLogRing(max int) *logRing {
	if max <= 0 {
		max = 400
	}
	return &logRing{max: max}
}

func (l *logRing) add(level, scope, message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, logEntry{At: time.Now().UnixMilli(), Level: level, Scope: scope, Message: message})
	if len(l.entries) > l.max {
		l.entries = append([]logEntry(nil), l.entries[len(l.entries)-l.max:]...)
	}
}

func (l *logRing) info(scope, message string)  { l.add("info", scope, message) }
func (l *logRing) warn(scope, message string)  { l.add("warn", scope, message) }
func (l *logRing) error(scope, message string) { l.add("error", scope, message) }

// debugDump records a payload fragment only when debug_dump is on; callers
// check the flag so the string is not built otherwise.
func (l *logRing) debugDump(scope, message string) { l.add("debug", scope, message) }

// recent returns entries newest first.
func (l *logRing) recent(limit int) []logEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 || limit > len(l.entries) {
		limit = len(l.entries)
	}
	out := make([]logEntry, 0, limit)
	for i := len(l.entries) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, l.entries[i])
	}
	return out
}

type metrics struct {
	requests   atomic.Int64
	success    atomic.Int64
	failures   atomic.Int64
	streamed   atomic.Int64
	localLimit atomic.Int64
	authRetry  atomic.Int64
	tokensIn   atomic.Int64
	tokensOut  atomic.Int64
	points     atomic.Int64
}

func (m *metrics) record(ok bool) {
	m.requests.Add(1)
	if ok {
		m.success.Add(1)
	} else {
		m.failures.Add(1)
	}
}

func (m *metrics) snapshot() map[string]any {
	return map[string]any{
		"requests":     m.requests.Load(),
		"success":      m.success.Load(),
		"failures":     m.failures.Load(),
		"streamed":     m.streamed.Load(),
		"local_limits": m.localLimit.Load(),
		"auth_retries": m.authRetry.Load(),
		"tokens_in":    m.tokensIn.Load(),
		"tokens_out":   m.tokensOut.Load(),
		"points":       m.points.Load(),
	}
}

type requestEntry struct {
	Seq           int64  `json:"seq"`
	At            int64  `json:"at"`
	Account       string `json:"account"`
	AccountID     string `json:"account_id"`
	Model         string `json:"model"`
	UpstreamModel string `json:"upstream_model"`
	Stream        bool   `json:"stream"`
	Status        int    `json:"status"`
	TTFTms        int64  `json:"ttft_ms"`
	TotalMs       int64  `json:"total_ms"`
	PromptTokens  int64  `json:"prompt_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	CachedTokens  int64  `json:"cached_tokens"`
	Points        int64  `json:"points"`
	Error         string `json:"error,omitempty"`
}

type requestLog struct {
	mu      sync.Mutex
	entries []requestEntry
	max     int
	seq     int64
}

func newRequestLog(max int) *requestLog {
	if max <= 0 {
		max = 200
	}
	return &requestLog{max: max}
}

func (r *requestLog) add(entry requestEntry) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	entry.Seq = r.seq
	r.entries = append(r.entries, entry)
	if len(r.entries) > r.max {
		r.entries = append([]requestEntry(nil), r.entries[len(r.entries)-r.max:]...)
	}
	return entry.Seq
}

func (r *requestLog) setPoints(seq int64, points int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.entries {
		if r.entries[i].Seq == seq {
			r.entries[i].Points = points
			return
		}
	}
}

func (r *requestLog) recent(limit int) []requestEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || limit > len(r.entries) {
		limit = len(r.entries)
	}
	out := make([]requestEntry, 0, limit)
	for i := len(r.entries) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, r.entries[i])
	}
	return out
}

// usageInfo is the token accounting extracted from an upstream payload.
type usageInfo struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
}

func parseUsage(payload []byte) *usageInfo {
	if len(payload) == 0 {
		return nil
	}
	var probe struct {
		Usage *struct {
			PromptTokens     flexInt `json:"prompt_tokens"`
			CompletionTokens flexInt `json:"completion_tokens"`
			TotalTokens      flexInt `json:"total_tokens"`
			PromptDetails    *struct {
				CachedTokens flexInt `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &probe) != nil || probe.Usage == nil {
		return nil
	}
	out := &usageInfo{
		PromptTokens:     probe.Usage.PromptTokens.Int64(),
		CompletionTokens: probe.Usage.CompletionTokens.Int64(),
		TotalTokens:      probe.Usage.TotalTokens.Int64(),
	}
	if probe.Usage.PromptDetails != nil {
		out.CachedTokens = probe.Usage.PromptDetails.CachedTokens.Int64()
	}
	return out
}

// payloadModel reads the model field of a chat payload without decoding it all.
func payloadModel(payload []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(payload, &probe) != nil {
		return ""
	}
	return strings.TrimSpace(probe.Model)
}

func truncateText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
