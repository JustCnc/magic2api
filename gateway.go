package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// gatewayToken is the credential pair minted by POST/PUT
// /api/v1/model-gateway/tokens. The api key lives 2h, the refresh token 7d, and
// a refresh rotates both — the previous api key stops working immediately.
type gatewayToken struct {
	APIKey                string `json:"api_key"`
	RefreshToken          string `json:"refresh_token"`
	APIKeyExpiresAt       string `json:"api_key_expires_at"`
	RefreshTokenExpiresAt string `json:"refresh_token_expires_at"`
}

// serverZone is the timezone the asset API prints timestamps in.
var serverZone = time.FixedZone("CST", 8*3600)

func parseServerTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, value, serverZone); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// ttlFrom turns an absolute server timestamp into a duration from now.
func ttlFrom(value string, fallback time.Duration) time.Duration {
	t, ok := parseServerTime(value)
	if !ok {
		return fallback
	}
	ttl := t.Sub(time.Now().In(serverZone))
	if ttl < time.Minute {
		ttl = time.Minute
	}
	if ttl > 7*24*time.Hour {
		ttl = 7 * 24 * time.Hour
	}
	return ttl
}

// mintGatewayToken exchanges the user JWT for a model-gateway api key.
func (u *upstream) mintGatewayToken(ctx context.Context, authorization, org string) (gatewayToken, error) {
	var out gatewayToken
	data, err := u.callAsset(ctx, http.MethodPost, "/api/v1/model-gateway/tokens", authorization, org, nil)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, &failure{Status: http.StatusBadGateway, Message: "解析模型网关令牌失败：" + err.Error(), Retryable: true}
	}
	if out.APIKey == "" {
		return out, &failure{Status: http.StatusBadGateway, Message: "模型网关未返回 api_key", Retryable: true}
	}
	return out, nil
}

// refreshGatewayToken rotates the pair. The old api key is invalidated.
func (u *upstream) refreshGatewayToken(ctx context.Context, authorization, org, refreshToken string) (gatewayToken, error) {
	var out gatewayToken
	if strings.TrimSpace(refreshToken) == "" {
		return out, &failure{Status: http.StatusUnauthorized, Message: "缺少 refresh_token", Auth: true}
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": refreshToken})
	data, err := u.callAsset(ctx, http.MethodPut, "/api/v1/model-gateway/tokens", authorization, org, body)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, &failure{Status: http.StatusBadGateway, Message: "解析模型网关令牌失败：" + err.Error(), Retryable: true}
	}
	if out.APIKey == "" {
		return out, &failure{Status: http.StatusBadGateway, Message: "刷新后未返回 api_key", Retryable: true}
	}
	return out, nil
}

type gatewayModel struct {
	ID      string  `json:"id"`
	Object  string  `json:"object"`
	Created flexInt `json:"created_at"`
	OwnerBy string  `json:"owner_by"`
}

type gatewayModelList struct {
	Object string         `json:"object"`
	Data   []gatewayModel `json:"data"`
}

// listModels reads the authoritative catalogue for one api key.
func (u *upstream) listModels(ctx context.Context, apiKey string) ([]gatewayModel, error) {
	status, header, payload, err := u.call(ctx, http.MethodGet, u.cfg.BaseURL+"/v1/models", u.gatewayHeaders(apiKey), nil)
	if err != nil {
		return nil, &failure{Status: http.StatusBadGateway, Message: "GET /v1/models: " + err.Error(), Retryable: true}
	}
	if f := gatewayAuthFailure(status, header, payload); f != nil {
		return nil, f
	}
	if status < 200 || status >= 300 {
		return nil, &failure{Status: status, Message: describeRawResponse("GET /v1/models", status, header.Get("Content-Type"), payload), Retryable: status >= 500}
	}
	var list gatewayModelList
	if err := json.Unmarshal(payload, &list); err != nil {
		return nil, &failure{Status: http.StatusBadGateway, Message: describeRawResponse("GET /v1/models", status, header.Get("Content-Type"), payload), Retryable: true}
	}
	return list.Data, nil
}

// gatewayAuthFailure recognises the gateway's habit of answering an auth error
// with HTTP 200 and a {"code":403} body.
func gatewayAuthFailure(status int, header http.Header, payload []byte) *failure {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return &failure{Status: http.StatusUnauthorized, Message: describeRawResponse("gateway", status, header.Get("Content-Type"), payload), Auth: true}
	}
	trimmed := strings.TrimSpace(string(payload))
	if status == http.StatusOK && strings.HasPrefix(trimmed, "{") && !strings.Contains(trimmed, `"choices"`) && !strings.Contains(trimmed, `"object"`) {
		var probe struct {
			Code    flexInt `json:"code"`
			Message string  `json:"message"`
		}
		if json.Unmarshal(payload, &probe) == nil && probe.Code.Int64() == 403 {
			return &failure{Status: http.StatusUnauthorized, Message: "gateway: " + probe.Message, Auth: true}
		}
	}
	return nil
}

// openChat posts a chat-completions payload and returns the live response so the
// caller can stream it.
func (u *upstream) openChat(ctx context.Context, apiKey string, body []byte, stream bool) (*http.Response, error) {
	headers := u.gatewayHeaders(apiKey)
	if stream {
		headers["Accept"] = "text/event-stream"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.cfg.BaseURL+"/v1/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		return nil, &failure{Status: http.StatusBadRequest, Message: err.Error()}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, &failure{Status: http.StatusBadGateway, Message: "POST /v1/chat/completions: " + err.Error(), Retryable: true}
	}
	return resp, nil
}

// chatOnce performs a non-streaming call and returns the raw body.
func (u *upstream) chatOnce(ctx context.Context, apiKey string, body []byte) ([]byte, http.Header, error) {
	resp, err := u.openChat(ctx, apiKey, body, false)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	payload, errRead := readAllLimited(resp.Body, 64<<20)
	if errRead != nil {
		return nil, resp.Header, &failure{Status: http.StatusBadGateway, Message: "读取上游响应失败：" + errRead.Error(), Retryable: true}
	}
	if f := gatewayAuthFailure(resp.StatusCode, resp.Header, payload); f != nil {
		return nil, resp.Header, f
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.Header, classifyChatError(resp.StatusCode, resp.Header, payload)
	}
	return payload, resp.Header, nil
}

// classifyChatError maps an upstream chat error onto a client-facing failure.
// The gateway answers real HTTP errors with OpenAI-style {"error":{...}} and
// business errors with {"code":N,"message":...}.
func classifyChatError(status int, header http.Header, payload []byte) *failure {
	message := ""
	var oai struct {
		Error struct {
			Message string  `json:"message"`
			Code    flexInt `json:"code"`
			Type    string  `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &oai) == nil && oai.Error.Message != "" {
		message = oai.Error.Message
	} else {
		var env struct {
			Code    flexInt `json:"code"`
			Message string  `json:"message"`
		}
		if json.Unmarshal(payload, &env) == nil && env.Message != "" {
			message = env.Message
		}
	}
	if message == "" {
		message = describeRawResponse("POST /v1/chat/completions", status, header.Get("Content-Type"), payload)
	} else {
		message = fmt.Sprintf("POST /v1/chat/completions: HTTP %d %s", status, message)
	}
	f := &failure{Status: status, Message: message}
	switch {
	case status == http.StatusTooManyRequests:
		f.Retryable = true
		f.RateLimit = true
	case status >= 500:
		f.Retryable = true
	case status == http.StatusRequestTimeout || status == http.StatusConflict:
		f.Retryable = true
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		f.Auth = true
	}
	return f
}
