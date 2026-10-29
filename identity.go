package main

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// The desktop client identifies a login device with a four-field object built
// from a UA-Parser fingerprint:
//
//	{ id: MD5(JSON.stringify(uaParser.getResult())), name, os, os_version }
//
// Nothing else (no machine id, no serial) leaves the machine. The server hashes
// id again into the JWT's cus.device_id, so the exact input is not validated;
// what matters is that every account keeps a stable, distinct id.
const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"

type deviceInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	OS        string `json:"os"`
	OSVersion string `json:"os_version"`
}

// fingerprintPayload mirrors ua-parser's getResult() field order, which is what
// the client hashes.
type fingerprintPayload struct {
	UA      string            `json:"ua"`
	Browser map[string]string `json:"browser"`
	Engine  map[string]string `json:"engine"`
	OS      map[string]string `json:"os"`
	Device  map[string]string `json:"device"`
	CPU     map[string]string `json:"cpu"`
}

// newDevice builds a device identity. seed makes the id deterministic for an
// alias, so a restart keeps the same device unless the caller asks for a fresh
// one (imported accounts with a duplicated device).
func newDevice(seed string) deviceInfo {
	if seed == "" {
		seed = randomHex(8)
	}
	payload := fingerprintPayload{
		UA:      defaultUserAgent,
		Browser: map[string]string{"name": "Chrome", "version": "138.0.0.0", "major": "138"},
		Engine:  map[string]string{"name": "Blink", "version": "138.0.0.0"},
		OS:      map[string]string{"name": "Windows", "version": "10"},
		Device:  map[string]string{"vendor": "", "model": "", "type": ""},
		CPU:     map[string]string{"architecture": "amd64"},
	}
	// The seed participates in the hash so two aliases never collide.
	payload.UA = payload.UA + " " + seed
	raw, _ := json.Marshal(payload)
	sum := md5.Sum(raw)
	return deviceInfo{
		ID:        hex.EncodeToString(sum[:]),
		Name:      "Chrome 138.0.0.0",
		OS:        "Windows",
		OSVersion: "10",
	}
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprint(err))))
	}
	return hex.EncodeToString(buf)
}

// requestID reproduces the client's request-id header: an 8-char base36 time
// prefix, 6 random bytes and an 8-char session fingerprint, all upper case.
func requestID(session string) string {
	var b strings.Builder
	b.WriteString(strings.ToUpper(randomHex(12)))
	if len(session) >= 8 {
		b.WriteString(strings.ToUpper(session[:8]))
	} else {
		b.WriteString(strings.ToUpper(randomHex(4)))
	}
	return b.String()
}
