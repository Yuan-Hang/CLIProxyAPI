package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

type hostHTTPStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

type upstreamStream struct {
	StatusCode   int
	Headers      http.Header
	HostStreamID string
	Body         io.ReadCloser
}

type hostHTTPStreamReadRequest struct {
	StreamID string `json:"stream_id"`
}

type hostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type hostHTTPStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
}

type pluginStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type pluginStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func executeStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode streaming executor request: %w", errUnmarshal)
	}
	if strings.TrimSpace(req.StreamID) == "" {
		return errorEnvelope("executor_error", "stream_id is required", 500, false), nil
	}
	protocol := executorProtocol(req.SourceFormat, req.Format)
	if protocol == "" {
		return errorEnvelope("unsupported_format", "unsupported executor format", 400, false), nil
	}
	payload, errPayload := prepareUpstreamPayload(req.Payload, req.Model, protocol)
	if errPayload != nil {
		return errorEnvelope("unsupported_model", errPayload.Error(), 400, false), nil
	}
	req.Payload = payload
	upstream, errOpen := openUpstreamStream(req, protocol, true)
	if errOpen != nil {
		return executorErrorEnvelope(errOpen), nil
	}
	go func() {
		errForward := forwardUpstreamStream(context.Background(), upstream, req.StreamID)
		closePluginStream(req.StreamID, errForward)
	}()
	headers := upstream.Headers
	if headers == nil {
		headers = make(http.Header)
	}
	if headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", "text/event-stream")
	}
	return okEnvelope(map[string]any{"headers": headers})
}

func openUpstreamStream(req rpcExecutorRequest, protocol string, retryUnauthorized bool) (upstreamStream, error) {
	token, errCredential := credentials.credential(nil, req.AuthID, false)
	if errCredential != nil {
		return upstreamStream{}, errCredential
	}
	targetURL, errEndpoint := endpointForProtocol(protocol)
	if errEndpoint != nil {
		return upstreamStream{}, errEndpoint
	}
	request := hostHTTPRequest{
		Method:  http.MethodPost,
		URL:     targetURL,
		Headers: upstreamHeaders(req.Headers, token, protocol, true),
		Body:    append([]byte(nil), req.Payload...),
	}
	proxyURL, errProxy := authProxyURL(req.StorageJSON)
	if errProxy != nil {
		return upstreamStream{}, errProxy
	}
	var resp upstreamStream
	if proxyURL == "" {
		request.HostCallbackID = req.HostCallbackID
		raw, errCall := hostCall(pluginabi.MethodHostHTTPDoStream, request)
		if errCall != nil {
			return upstreamStream{}, errCall
		}
		var hostResp hostHTTPStreamResponse
		if errUnmarshal := json.Unmarshal(raw, &hostResp); errUnmarshal != nil {
			return upstreamStream{}, fmt.Errorf("decode host HTTP stream response: %w", errUnmarshal)
		}
		resp = upstreamStream{
			StatusCode:   hostResp.StatusCode,
			Headers:      hostResp.Headers,
			HostStreamID: hostResp.StreamID,
		}
	} else {
		httpResp, errDo := doPluginHTTPRequest(context.Background(), proxyURL, request)
		if errDo != nil {
			return upstreamStream{}, errDo
		}
		resp = upstreamStream{
			StatusCode: httpResp.StatusCode,
			Headers:    httpResp.Header.Clone(),
			Body:       httpResp.Body,
		}
	}
	if resp.StatusCode == http.StatusUnauthorized && retryUnauthorized {
		closeUpstreamStream(resp)
		credentials.invalidate(req.AuthID)
		if _, errRefresh := credentials.credential(nil, req.AuthID, true); errRefresh != nil {
			return upstreamStream{}, errRefresh
		}
		return openUpstreamStream(req, protocol, false)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body := readUpstreamStreamBody(resp, 512)
		closeUpstreamStream(resp)
		return upstreamStream{}, newUpstreamStatusError(resp.StatusCode, body)
	}
	if strings.TrimSpace(resp.HostStreamID) == "" && resp.Body == nil {
		return upstreamStream{}, fmt.Errorf("upstream HTTP stream is empty")
	}
	return resp, nil
}

func forwardUpstreamStream(ctx context.Context, upstream upstreamStream, pluginStreamID string) error {
	defer closeUpstreamStream(upstream)
	if upstream.Body != nil {
		buf := make([]byte, 32*1024)
		for {
			n, errRead := upstream.Body.Read(buf)
			if n > 0 {
				if errEmit := emitPluginStreamChunk(pluginStreamID, buf[:n]); errEmit != nil {
					return errEmit
				}
			}
			if errRead != nil {
				if errRead == io.EOF {
					return nil
				}
				return fmt.Errorf("read upstream stream: %w", errRead)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
	}
	for {
		raw, errCall := hostCall(pluginabi.MethodHostHTTPStreamRead, hostHTTPStreamReadRequest{StreamID: upstream.HostStreamID})
		if errCall != nil {
			return errCall
		}
		var chunk hostHTTPStreamReadResponse
		if errUnmarshal := json.Unmarshal(raw, &chunk); errUnmarshal != nil {
			return fmt.Errorf("decode host HTTP stream chunk: %w", errUnmarshal)
		}
		if chunk.Error != "" {
			return fmt.Errorf("upstream stream: %s", chunk.Error)
		}
		if len(chunk.Payload) > 0 {
			if errEmit := emitPluginStreamChunk(pluginStreamID, chunk.Payload); errEmit != nil {
				return errEmit
			}
		}
		if chunk.Done {
			return nil
		}
	}
}

func emitPluginStreamChunk(pluginStreamID string, payload []byte) error {
	_, errEmit := hostCall(pluginabi.MethodHostStreamEmit, pluginStreamEmitRequest{
		StreamID: pluginStreamID,
		Payload:  append([]byte(nil), payload...),
	})
	return errEmit
}

func readUpstreamStreamBody(upstream upstreamStream, limit int) []byte {
	if limit <= 0 {
		return nil
	}
	if upstream.Body != nil {
		body, _ := io.ReadAll(io.LimitReader(upstream.Body, int64(limit)))
		return body
	}
	if strings.TrimSpace(upstream.HostStreamID) == "" {
		return nil
	}
	body := make([]byte, 0, limit)
	for len(body) < limit {
		raw, errCall := hostCall(pluginabi.MethodHostHTTPStreamRead, hostHTTPStreamReadRequest{StreamID: upstream.HostStreamID})
		if errCall != nil {
			break
		}
		var chunk hostHTTPStreamReadResponse
		if errUnmarshal := json.Unmarshal(raw, &chunk); errUnmarshal != nil {
			break
		}
		remaining := limit - len(body)
		if len(chunk.Payload) > remaining {
			body = append(body, chunk.Payload[:remaining]...)
		} else {
			body = append(body, chunk.Payload...)
		}
		if chunk.Done || chunk.Error != "" {
			break
		}
	}
	return body
}

func closeUpstreamStream(upstream upstreamStream) {
	if upstream.Body != nil {
		_ = upstream.Body.Close()
	}
	if strings.TrimSpace(upstream.HostStreamID) != "" {
		_, _ = hostCall(pluginabi.MethodHostHTTPStreamClose, hostHTTPStreamCloseRequest{StreamID: upstream.HostStreamID})
	}
}

func closePluginStream(streamID string, err error) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	message := ""
	if err != nil {
		message = err.Error()
	}
	_, _ = hostCall(pluginabi.MethodHostStreamClose, pluginStreamCloseRequest{
		StreamID: streamID,
		Error:    message,
	})
}
