package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// jwtExpiry reads the exp claim out of an upstream JWT so the plugin can tell
// "the api key needs rotating" (mundane, every 2h) from "the login itself died"
// (needs a human to sign in again). The service signs with HS256 and base64
// pads its segments, which is not RFC 7515 compliant, so every padding variant
// is accepted here.
func jwtExpiry(token string) (time.Time, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	payload, ok := decodeJWTPart(parts[1])
	if !ok {
		return time.Time{}, false
	}
	var claims struct {
		Exp flexInt `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, false
	}
	if int64(claims.Exp) <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(claims.Exp), 0), true
}

func decodeJWTPart(part string) ([]byte, bool) {
	part = strings.TrimSpace(part)
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding,
	} {
		if raw, err := enc.DecodeString(part); err == nil {
			return raw, true
		}
	}
	return nil, false
}

// jwtExpiresAtMillis is the persisted form. Unknown or unparsable tokens report
// zero, which means "no local opinion" rather than "expired".
func jwtExpiresAtMillis(token string) int64 {
	exp, ok := jwtExpiry(token)
	if !ok {
		return 0
	}
	return exp.UnixMilli()
}

// jwtDead reports whether the login token is past its exp claim. A login token
// that cannot be parsed is treated as alive: only the upstream may judge it.
func (c *credential) jwtDead(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.jwtDeadLocked(now)
}

func (c *credential) jwtDeadLocked(now time.Time) bool {
	if c.JWTExpiresAt <= 0 {
		return false
	}
	return now.UnixMilli() >= c.JWTExpiresAt
}

// jwtDeadline reports the expiry instant and whether it has already passed.
func (c *credential) jwtDeadline() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.JWTExpiresAt <= 0 {
		return time.Time{}, false
	}
	exp := time.UnixMilli(c.JWTExpiresAt)
	return exp, !time.Now().Before(exp)
}

// jwtSubject reads the iss claim, which the account service sets to the user id.
// It is used to recognise the same person signing in again.
func jwtSubject(token string) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return ""
	}
	payload, ok := decodeJWTPart(parts[1])
	if !ok {
		return ""
	}
	var claims struct {
		Iss flexString `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return strings.TrimSpace(string(claims.Iss))
}
