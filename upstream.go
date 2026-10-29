package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// apiEnvelope is the asset/account API reply. Success is code 1000 — and it
// arrives with HTTP 200 even for business failures, so callers must look at
// Code, not at the status line.
type apiEnvelope struct {
	Code    flexInt         `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
	Error   json.RawMessage `json:"error"`
}

const apiSuccessCode = 1000

// failure describes an upstream problem in the shape the host understands.
type failure struct {
	Status    int
	Message   string
	Retryable bool
	// Auth marks a rejected credential: refresh the token and try again.
	Auth bool
	// RateLimit marks upstream throttling.
	RateLimit bool
	// Dead marks an account that must not be retried at all.
	Dead bool
}

func (f *failure) Error() string { return f.Message }

func asFailure(err error) *failure {
	if err == nil {
		return nil
	}
	if f, ok := err.(*failure); ok {
		return f
	}
	return &failure{Status: http.StatusBadGateway, Message: err.Error(), Retryable: true}
}

type upstream struct {
	cfg    Config
	client *http.Client
	// stream has no whole-request timeout: a long answer must not be cut in the
	// middle. Liveness comes from the idle watchdog in executor.go.
	stream *http.Client
}

func newUpstream(cfg Config) *upstream {
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if cfg.ProxyURL != "" {
		if proxy, err := url.Parse(cfg.ProxyURL); err == nil {
			transport.Proxy = http.ProxyURL(proxy)
		}
	}
	checkRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &upstream{
		cfg: cfg,
		client: &http.Client{
			Transport:     transport,
			Timeout:       time.Duration(cfg.requestTimeout()) * time.Second,
			CheckRedirect: checkRedirect,
		},
		// No whole-request timeout: a long stream must not be cut mid-answer.
		stream: &http.Client{
			Transport:     transport,
			CheckRedirect: checkRedirect,
		},
	}
}

func (u *upstream) close() {
	if u == nil || u.client == nil {
		return
	}
	if tr, ok := u.client.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

// call performs one request and returns status, content type and body.
func (u *upstream) call(ctx context.Context, method, target string, headers map[string]string, body []byte) (int, http.Header, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", defaultUserAgent)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	payload, errRead := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if errRead != nil {
		return resp.StatusCode, resp.Header, nil, errRead
	}
	return resp.StatusCode, resp.Header, payload, nil
}

// assetHeaders are the headers every /api/v1 request carries.
func (u *upstream) assetHeaders(token, org, language string) map[string]string {
	if language == "" {
		language = "zh_CN"
	}
	h := map[string]string{
		"Content-Type":      "application/json",
		"Accept":            "application/json",
		"language":          language,
		"request-id":        requestID(""),
		"organization-code": org,
	}
	if token != "" {
		h["authorization"] = token
	}
	return h
}

// gatewayHeaders authenticate against the model gateway.
func (u *upstream) gatewayHeaders(apiKey string) map[string]string {
	return map[string]string{
		"Content-Type": "application/json",
		"api-key":      apiKey,
	}
}

// callAsset performs an asset/account call and unwraps the envelope.
func (u *upstream) callAsset(ctx context.Context, method, path, token, org string, body []byte) (json.RawMessage, error) {
	status, header, payload, err := u.call(ctx, method, u.cfg.BaseURL+path, u.assetHeaders(token, org, ""), body)
	if err != nil {
		return nil, &failure{Status: http.StatusBadGateway, Message: fmt.Sprintf("%s %s: %v", method, path, err), Retryable: true}
	}
	return u.unwrapAsset(status, header, payload, method+" "+path)
}

func (u *upstream) unwrapAsset(status int, header http.Header, payload []byte, label string) (json.RawMessage, error) {
	contentType := ""
	if header != nil {
		contentType = header.Get("Content-Type")
	}
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		return nil, &failure{
			Status:    http.StatusBadGateway,
			Message:   describeRawResponse(label, status, contentType, payload),
			Retryable: true,
		}
	}
	var env apiEnvelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return nil, &failure{Status: http.StatusBadGateway, Message: describeRawResponse(label, status, contentType, payload), Retryable: true}
	}
	code := env.Code.Int64()
	if code == apiSuccessCode {
		return env.Data, nil
	}
	msg := env.Message
	if msg == "" {
		msg = strings.TrimSpace(string(trimmed))
	}
	f := &failure{
		Status:  http.StatusBadGateway,
		Message: fmt.Sprintf("%s: code=%d %s", label, code, msg),
	}
	switch code {
	case 2000, 2152, 2168, 2154:
		// Bad credentials or bad verification code: the caller sent something
		// wrong, so do not report it as an upstream outage.
		f.Status = http.StatusBadRequest
	case 3103, 2179, 3102:
		f.Status = http.StatusUnauthorized
		f.Auth = true
		f.Message = fmt.Sprintf("%s: code=%d %s", label, code, msg)
	case 2155, 3109:
		// 2155 用户不存在 / 3109 账号已禁用: the credential will never work again,
		// so stop burning attempts on it instead of cooling it.
		f.Status = http.StatusForbidden
		f.Auth = true
		f.Dead = true
		f.Message = fmt.Sprintf("%s: code=%d %s", label, code, msg)
	case 9307:
		f.Status = http.StatusTooManyRequests
		f.Retryable = true
		f.RateLimit = true
	case 9306:
		f.Retryable = true
	}
	if status == http.StatusTooManyRequests {
		f.Status = http.StatusTooManyRequests
		f.Retryable = true
		f.RateLimit = true
	}
	return nil, f
}

// describeRawResponse builds the diagnostic string required for every non-JSON
// answer: status, content type and a body fragment.
func describeRawResponse(label string, status int, contentType string, payload []byte) string {
	fragment := strings.TrimSpace(string(payload))
	fragment = strings.Join(strings.Fields(fragment), " ")
	if len(fragment) > 300 {
		fragment = fragment[:300] + "…"
	}
	if fragment == "" {
		fragment = "<empty body>"
	}
	if contentType == "" {
		contentType = "<no content-type>"
	}
	return fmt.Sprintf("%s: HTTP %d, content-type=%s, body=%s", label, status, contentType, fragment)
}
