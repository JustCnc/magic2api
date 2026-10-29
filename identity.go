package main

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
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
	// The seed participates in the hash so two aliases never collide. It is not
	// part of the ua field: only the digest leaves the machine, and the client
	// never transmits the parser result itself.
	fingerprint := payload
	fingerprint.UA = payload.UA + "|" + seed
	raw, _ := json.Marshal(fingerprint)
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

// The client stamps every request with a 28-character request-id:
//
//	base36(now in ms) last 8 | 12 hex random | 8 hex session fingerprint
//
// all upper case (createRequestId in the web bundle). The fingerprint is a
// 32-bit rolling hash of the browser's stable properties, so it is constant for
// one machine and changes with the environment, which is why it is computed
// once here.
const (
	sessionUserAgent   = defaultUserAgent
	sessionCPUThreads  = "8"
	sessionScreenWidth = "1920"
	sessionTimeZone    = "Asia/Shanghai"
)

var (
	fingerprintOnce sync.Once
	fingerprintHex  string
)

func sessionFingerprint() string {
	fingerprintOnce.Do(func() {
		joined := strings.Join([]string{sessionUserAgent, sessionCPUThreads, sessionScreenWidth, sessionTimeZone}, "|")
		var acc int32
		for _, b := range []byte(joined) {
			acc = acc*33 + int32(b)
		}
		if acc < 0 {
			acc = -acc
		}
		value := strconv.FormatInt(int64(acc), 16)
		if len(value) > 8 {
			value = value[:8]
		}
		if value == "" {
			value = "0"
		}
		fingerprintHex = strings.ToUpper(value)
	})
	return fingerprintHex
}

// requestID reproduces the client's request-id header. session, when given,
// overrides the process fingerprint.
func requestID(session string) string {
	stamp := strconv.FormatInt(time.Now().UnixMilli(), 36)
	if len(stamp) > 8 {
		stamp = stamp[len(stamp)-8:]
	}
	stamp = strings.Repeat("0", 8-len(stamp)) + stamp
	if session == "" {
		session = sessionFingerprint()
	}
	session = strings.ToUpper(session)
	if len(session) > 8 {
		session = session[:8]
	}
	return strings.ToUpper(stamp) + strings.ToUpper(randomHex(6)) + session
}
