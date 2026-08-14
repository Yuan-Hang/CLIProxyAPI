package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/sjson"
)

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcExecutorHTTPRequest struct {
	pluginapi.ExecutorHTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type hostHTTPRequest struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	Method         string      `json:"method,omitempty"`
	URL            string      `json:"url,omitempty"`
	Headers        http.Header `json:"headers,omitempty"`
	Body           []byte      `json:"body,omitempty"`
}

type upstreamStatusError struct {
	status  int
	message string
}

func (e *upstreamStatusError) Error() string { return e.message }

func execute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode executor request: %w", errUnmarshal)
	}
	resp, errExecute := executeNonStream(req)
	if errExecute != nil {
		return executorErrorEnvelope(errExecute), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{Payload: resp.Body, Headers: resp.Headers})
}

func executeNonStream(req rpcExecutorRequest) (pluginapi.HTTPResponse, error) {
	protocol := executorProtocol(req.SourceFormat, req.Format)
	if protocol == "" {
		return pluginapi.HTTPResponse{}, fmt.Errorf("unsupported executor format %q", req.SourceFormat)
	}
	payload, errPayload := prepareUpstreamPayload(req.Payload, req.Model, protocol)
	if errPayload != nil {
		return pluginapi.HTTPResponse{}, errPayload
	}
	return performNonStreamRequest(req.AuthID, req.HostCallbackID, req.StorageJSON, protocol, req.Headers, payload, "", true)
}

func performNonStreamRequest(authID, callbackID string, storageJSON []byte, protocol string, headers http.Header, body []byte, explicitURL string, retryUnauthorized bool) (pluginapi.HTTPResponse, error) {
	token, errCredential := credentials.credential(nil, authID, false)
	if errCredential != nil {
		return pluginapi.HTTPResponse{}, errCredential
	}
	targetURL := explicitURL
	if targetURL == "" {
		var errEndpoint error
		targetURL, errEndpoint = endpointForProtocol(protocol)
		if errEndpoint != nil {
			return pluginapi.HTTPResponse{}, errEndpoint
		}
	}
	resp, errDo := doUpstreamHTTPRequest(context.Background(), callbackID, storageJSON, hostHTTPRequest{
		Method:  http.MethodPost,
		URL:     targetURL,
		Headers: upstreamHeaders(headers, token, protocol, false),
		Body:    append([]byte(nil), body...),
	})
	if errDo != nil {
		return pluginapi.HTTPResponse{}, errDo
	}
	if resp.StatusCode == http.StatusUnauthorized && retryUnauthorized {
		credentials.invalidate(authID)
		if _, errRefresh := credentials.credential(nil, authID, true); errRefresh != nil {
			return pluginapi.HTTPResponse{}, errRefresh
		}
		return performNonStreamRequest(authID, callbackID, storageJSON, protocol, headers, body, explicitURL, false)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pluginapi.HTTPResponse{}, newUpstreamStatusError(resp.StatusCode, resp.Body)
	}
	return resp, nil
}

func executeHTTPRequest(raw []byte) ([]byte, error) {
	var req rpcExecutorHTTPRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode executor HTTP request: %w", errUnmarshal)
	}
	protocol, okAllowed := protocolForAllowedURL(req.URL)
	if !okAllowed {
		return errorEnvelope("invalid_upstream_url", "URL is outside configured protocol base URLs", 400, false), nil
	}
	body := append([]byte(nil), req.Body...)
	var selector struct {
		Model string `json:"model"`
	}
	if len(body) > 0 && json.Unmarshal(body, &selector) == nil && strings.TrimSpace(selector.Model) != "" {
		var errPayload error
		body, errPayload = prepareUpstreamPayload(body, selector.Model, protocol)
		if errPayload != nil {
			return errorEnvelope("unsupported_model", errPayload.Error(), 400, false), nil
		}
	}
	token, errCredential := credentials.credential(nil, req.AuthID, false)
	if errCredential != nil {
		return errorEnvelope("credential_command_failed", errCredential.Error(), 503, true), nil
	}
	resp, errDo := doUpstreamHTTPRequest(context.Background(), req.HostCallbackID, req.StorageJSON, hostHTTPRequest{
		Method:  req.Method,
		URL:     req.URL,
		Headers: upstreamHeaders(req.Headers, token, protocol, false),
		Body:    body,
	})
	if errDo != nil {
		return errorEnvelope("upstream_request_failed", errDo.Error(), 502, true), nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		credentials.invalidate(req.AuthID)
	}
	return okEnvelope(pluginapi.ExecutorHTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Headers,
		Body:       resp.Body,
	})
}

func prepareUpstreamPayload(payload []byte, requestedModel, protocol string) ([]byte, error) {
	upstreamModel, ok := configuredUpstreamModel(requestedModel, protocol, loadedConfig().Models)
	if !ok {
		return nil, fmt.Errorf("model %q does not support %s", requestedModel, protocol)
	}
	if !json.Valid(payload) {
		return nil, fmt.Errorf("request payload is not valid JSON")
	}
	out, errSet := sjson.SetBytes(payload, "model", upstreamModel)
	if errSet != nil {
		return nil, fmt.Errorf("rewrite upstream model: %w", errSet)
	}
	return out, nil
}

func countTokens(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode token count request: %w", errUnmarshal)
	}
	count := estimateInputTokens(req.Payload)
	protocol := executorProtocol(req.SourceFormat, req.Format)
	if protocol == "anthropic" {
		payload := []byte(fmt.Sprintf(`{"input_tokens":%d}`, count))
		return okEnvelope(pluginapi.ExecutorResponse{Payload: payload})
	}
	if protocol == "chat-completions" {
		payload := []byte(fmt.Sprintf(`{"usage":{"prompt_tokens":%d,"completion_tokens":0,"total_tokens":%d}}`, count, count))
		return okEnvelope(pluginapi.ExecutorResponse{Payload: payload})
	}
	payload := []byte(fmt.Sprintf(`{"response":{"usage":{"input_tokens":%d,"output_tokens":0,"total_tokens":%d}}}`, count, count))
	return okEnvelope(pluginapi.ExecutorResponse{Payload: payload})
}

func estimateInputTokens(payload []byte) int64 {
	if len(payload) == 0 {
		return 0
	}
	runes := utf8.RuneCount(payload)
	count := int64((runes + 3) / 4)
	if count == 0 {
		return 1
	}
	return count
}

func doHostHTTPRequest(req hostHTTPRequest) (pluginapi.HTTPResponse, error) {
	raw, errCall := hostCall(pluginabi.MethodHostHTTPDo, req)
	if errCall != nil {
		return pluginapi.HTTPResponse{}, errCall
	}
	var resp pluginapi.HTTPResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("decode host HTTP response: %w", errUnmarshal)
	}
	return resp, nil
}

func executorProtocol(sourceFormat, responseFormat string) string {
	for _, value := range []string{sourceFormat, responseFormat} {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "openai-response", "openai-responses", "responses", "codex":
			return "responses"
		case "claude", "anthropic":
			return "anthropic"
		case "openai", "chat-completions", "openai-chat-completions":
			return "chat-completions"
		}
	}
	return ""
}

func endpointForProtocol(protocol string) (string, error) {
	cfg := loadedConfig()
	protocol = normalizeProtocol(protocol)
	protocolCfg, ok := cfg.Protocols[protocol]
	if !ok {
		return "", fmt.Errorf("protocol %q is not configured", protocol)
	}
	return strings.TrimRight(protocolCfg.BaseURL, "/") + "/" + strings.TrimLeft(protocolCfg.Path, "/"), nil
}

func upstreamHeaders(source http.Header, token, protocol string, stream bool) http.Header {
	headers := make(http.Header)
	for key, values := range source {
		if shouldDropForwardedHeader(key) {
			continue
		}
		for _, value := range values {
			headers.Add(key, value)
		}
	}
	if protocolCfg, ok := loadedConfig().Protocols[normalizeProtocol(protocol)]; ok {
		for key, value := range protocolCfg.Headers {
			headers.Set(key, value)
		}
	}
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Content-Type", "application/json")
	if stream {
		headers.Set("Accept", "text/event-stream")
	} else if headers.Get("Accept") == "" {
		headers.Set("Accept", "application/json")
	}
	if protocol == "anthropic" && headers.Get("Anthropic-Version") == "" {
		headers.Set("Anthropic-Version", "2023-06-01")
	}
	return headers
}

func shouldDropForwardedHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authorization", "x-api-key", "proxy-authorization", "cookie", "host", "content-length", "connection", "proxy-connection", "keep-alive", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func protocolForAllowedURL(raw string) (string, bool) {
	target, errTarget := url.Parse(strings.TrimSpace(raw))
	if errTarget != nil || target.Scheme == "" || target.Host == "" {
		return "", false
	}
	bestProtocol := ""
	bestPathLength := -1
	for protocol := range loadedConfig().Protocols {
		endpointRaw, errEndpoint := endpointForProtocol(protocol)
		if errEndpoint != nil {
			continue
		}
		endpoint, errParse := url.Parse(endpointRaw)
		if errParse != nil || !strings.EqualFold(target.Scheme, endpoint.Scheme) || !strings.EqualFold(target.Host, endpoint.Host) {
			continue
		}
		path := strings.TrimRight(endpoint.Path, "/")
		if target.Path != path && !strings.HasPrefix(target.Path, path+"/") {
			continue
		}
		if len(path) > bestPathLength {
			bestProtocol = protocol
			bestPathLength = len(path)
		}
	}
	return bestProtocol, bestProtocol != ""
}

func newUpstreamStatusError(status int, body []byte) error {
	message := fmt.Sprintf("upstream returned HTTP %d", status)
	if detail := sanitizedErrorDetail(body); detail != "" {
		message += ": " + detail
	}
	return &upstreamStatusError{status: status, message: message}
}

func sanitizedErrorDetail(body []byte) string {
	if len(body) > 512 {
		body = body[:512]
	}
	value := strings.Join(strings.Fields(string(body)), " ")
	return strings.TrimSpace(value)
}

func executorErrorEnvelope(err error) []byte {
	if err == nil {
		return errorEnvelope("executor_error", "unknown executor error", 500, false)
	}
	if statusErr, ok := err.(*upstreamStatusError); ok {
		retryable := statusErr.status == 408 || statusErr.status == 429 || statusErr.status >= 500
		return errorEnvelope("upstream_error", statusErr.message, statusErr.status, retryable)
	}
	return errorEnvelope("executor_error", err.Error(), 502, true)
}
