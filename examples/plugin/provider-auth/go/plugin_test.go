package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

func TestPluginRegistrationHasRequiredHostMetadataAndConfiguredFormats(t *testing.T) {
	restore := installTestConfigAndCommand(t, func(context.Context, pluginConfig) (string, error) {
		return "unused", nil
	})
	defer restore()

	registration := pluginRegistration()
	if registration.SchemaVersion != pluginabi.SchemaVersion {
		t.Fatalf("schema version = %d, want %d", registration.SchemaVersion, pluginabi.SchemaVersion)
	}
	metadata := registration.Metadata
	if strings.TrimSpace(metadata.Name) == "" || strings.TrimSpace(metadata.Version) == "" ||
		strings.TrimSpace(metadata.Author) == "" || strings.TrimSpace(metadata.GitHubRepository) == "" {
		t.Fatalf("registration metadata does not satisfy host requirements: %#v", metadata)
	}
	capabilities := registration.Capabilities
	if !capabilities.ModelProvider || !capabilities.AuthProvider || !capabilities.Executor {
		t.Fatalf("registration capabilities = %#v", capabilities)
	}
	if capabilities.ExecutorModelScope != string(pluginapi.ExecutorModelScopeOAuth) {
		t.Fatalf("executor model scope = %q", capabilities.ExecutorModelScope)
	}
	if got := strings.Join(capabilities.ExecutorInputFormats, ","); got != "anthropic,chat-completions,responses" {
		t.Fatalf("executor formats = %q", got)
	}
}

func TestDecodeConfigNormalizesProtocolsAndModels(t *testing.T) {
	cfg := defaultPluginConfig()
	raw := []byte(`
command: /usr/local/bin/token-helper
args: [print, bearer]
timeout-ms: 7000
refresh-interval-seconds: 120
disable-cooling: true
protocols:
  openai-responses:
    base-url: https://provider.example/api/v1/
    headers:
      X-Provider-Version: "2026-01-01"
  claude:
    base-url: https://provider.example/anthropic/
    path: /v1/messages
  openai:
    base-url: https://provider.example/openai
models:
  - name: response-model
    protocol: openai-responses
  - name: message-model-v2
    alias: message-latest
    protocol: claude
  - name: chat-model
    protocol: openai
`)
	if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
		t.Fatalf("yaml decode: %v", errUnmarshal)
	}
	if errValidate := normalizeAndValidateConfig(&cfg); errValidate != nil {
		t.Fatalf("normalizeAndValidateConfig() error = %v", errValidate)
	}
	if cfg.Protocols["responses"].BaseURL != "https://provider.example/api/v1" || cfg.Protocols["responses"].Path != "/responses" {
		t.Fatalf("responses protocol = %#v", cfg.Protocols["responses"])
	}
	if cfg.Protocols["anthropic"].Path != "/v1/messages" || cfg.Protocols["chat-completions"].Path != "/chat/completions" {
		t.Fatalf("protocol paths = %#v", cfg.Protocols)
	}
	if cfg.Models[0].Protocol != "responses" || cfg.Models[1].Protocol != "anthropic" || cfg.Models[2].Protocol != "chat-completions" {
		t.Fatalf("models = %#v", cfg.Models)
	}
}

func TestConfigRequiresExplicitProtocolsAndModels(t *testing.T) {
	cfg := defaultPluginConfig()
	cfg.Command = "/usr/local/bin/token-helper"
	if errValidate := normalizeAndValidateConfig(&cfg); errValidate == nil || !strings.Contains(errValidate.Error(), "protocol") {
		t.Fatalf("validation error = %v, want missing protocol", errValidate)
	}
}

func TestProtocolForAllowedURLUsesConfiguredEndpointPath(t *testing.T) {
	restore := installTestConfigAndCommand(t, func(context.Context, pluginConfig) (string, error) {
		return "unused", nil
	})
	defer restore()

	protocol, ok := protocolForAllowedURL("https://provider.example/openai/chat/completions")
	if !ok || protocol != "chat-completions" {
		t.Fatalf("protocol=%q ok=%t", protocol, ok)
	}
	if _, ok := protocolForAllowedURL("https://provider.example/openai/responses"); ok {
		t.Fatal("unconfigured endpoint was allowed")
	}
}

func TestConfigRejectsCredentialHeaders(t *testing.T) {
	cfg := validTestConfig(t)
	protocol := cfg.Protocols["responses"]
	protocol.Headers["Authorization"] = "Bearer static-secret"
	cfg.Protocols["responses"] = protocol
	if errValidate := normalizeAndValidateConfig(&cfg); errValidate == nil || !strings.Contains(errValidate.Error(), "managed by the host") {
		t.Fatalf("validation error = %v", errValidate)
	}
}

func TestPrepareUpstreamPayloadRewritesArbitraryPrefixAndAlias(t *testing.T) {
	restore := installTestConfigAndCommand(t, func(context.Context, pluginConfig) (string, error) {
		return "unused", nil
	})
	defer restore()

	payload, errPrepare := prepareUpstreamPayload(
		[]byte(`{"model":"team/message-latest","messages":[]}`),
		"team/message-latest",
		"anthropic",
	)
	if errPrepare != nil {
		t.Fatalf("prepareUpstreamPayload() error = %v", errPrepare)
	}
	var decoded struct {
		Model string `json:"model"`
	}
	if errUnmarshal := json.Unmarshal(payload, &decoded); errUnmarshal != nil {
		t.Fatalf("decode rewritten payload: %v", errUnmarshal)
	}
	if decoded.Model != "message-model-v2" {
		t.Fatalf("rewritten model = %q", decoded.Model)
	}
}

func TestParseAuthCachesTokenWithoutReturningIt(t *testing.T) {
	restore := installTestConfigAndCommand(t, func(context.Context, pluginConfig) (string, error) {
		return "secret-bearer", nil
	})
	defer restore()

	req := pluginapi.AuthParseRequest{RawJSON: []byte(`{"type":"provider-auth","id":"auth-1","prefix":"team"}`)}
	rawReq, _ := json.Marshal(req)
	rawResp, errParse := parseAuth(rawReq)
	if errParse != nil {
		t.Fatalf("parseAuth() error = %v", errParse)
	}
	if strings.Contains(string(rawResp), "secret-bearer") {
		t.Fatal("parse response leaked command token")
	}
	if got := credentials.get("auth-1"); got != "secret-bearer" {
		t.Fatalf("cached token = %q", got)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(rawResp, &env); errUnmarshal != nil || !env.OK {
		t.Fatalf("parse envelope = %#v, err=%v", env, errUnmarshal)
	}
	var resp pluginapi.AuthParseResponse
	if errUnmarshal := json.Unmarshal(env.Result, &resp); errUnmarshal != nil {
		t.Fatalf("decode auth response: %v", errUnmarshal)
	}
	if !resp.Handled || resp.Auth.Provider != pluginIdentifier || resp.Auth.ID != "auth-1" {
		t.Fatalf("auth response = %#v", resp)
	}
	if resp.Auth.Metadata["disable_cooling"] != true {
		t.Fatalf("disable_cooling = %#v", resp.Auth.Metadata["disable_cooling"])
	}
}

func TestExecuteRefreshesOnceAfterUnauthorized(t *testing.T) {
	commandCalls := 0
	restore := installTestConfigAndCommand(t, func(context.Context, pluginConfig) (string, error) {
		commandCalls++
		if commandCalls == 1 {
			return "token-1", nil
		}
		return "token-2", nil
	})
	defer restore()

	if _, errCredential := credentials.credential(context.Background(), "auth-1", false); errCredential != nil {
		t.Fatalf("initial credential: %v", errCredential)
	}
	hostCalls := 0
	originalHostCall := hostCall
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostHTTPDo {
			t.Fatalf("host method = %q", method)
		}
		req, ok := payload.(hostHTTPRequest)
		if !ok {
			t.Fatalf("host payload type = %T", payload)
		}
		hostCalls++
		wantToken := "token-1"
		status := http.StatusUnauthorized
		body := []byte(`{"error":"expired"}`)
		if hostCalls == 2 {
			wantToken = "token-2"
			status = http.StatusOK
			body = []byte(`{"id":"resp-1"}`)
		}
		if got := req.Headers.Get("Authorization"); got != "Bearer "+wantToken {
			t.Fatalf("Authorization = %q, want token %q", got, wantToken)
		}
		if got := req.Headers.Get("X-Provider-Version"); got != "test" {
			t.Fatalf("X-Provider-Version = %q", got)
		}
		if req.URL != "https://provider.example/api/v1/responses" {
			t.Fatalf("URL = %q", req.URL)
		}
		var upstreamBody struct {
			Model string `json:"model"`
		}
		if errUnmarshal := json.Unmarshal(req.Body, &upstreamBody); errUnmarshal != nil || upstreamBody.Model != "response-model" {
			t.Fatalf("upstream body model = %q, err=%v", upstreamBody.Model, errUnmarshal)
		}
		raw, _ := json.Marshal(pluginapi.HTTPResponse{StatusCode: status, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: body})
		return raw, nil
	}
	defer func() { hostCall = originalHostCall }()

	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID:       "auth-1",
		AuthProvider: pluginIdentifier,
		Model:        "response-model",
		SourceFormat: "openai-response",
		Format:       "openai-response",
		Payload:      []byte(`{"model":"team/response-model","input":"hi"}`),
	}}
	rawReq, _ := json.Marshal(req)
	rawResp, errExecute := execute(rawReq)
	if errExecute != nil {
		t.Fatalf("execute() error = %v", errExecute)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(rawResp, &env); errUnmarshal != nil || !env.OK {
		t.Fatalf("execute envelope = %#v, err=%v, raw=%s", env, errUnmarshal, rawResp)
	}
	if hostCalls != 2 || commandCalls != 2 {
		t.Fatalf("host calls = %d, command calls = %d", hostCalls, commandCalls)
	}
	if strings.Contains(string(rawResp), "token-") {
		t.Fatal("execute response leaked a token")
	}
}

func TestExecuteHonorsDirectProxyFromAuthRecord(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v1/responses" {
			t.Errorf("upstream path = %q", req.URL.Path)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer direct-token" {
			t.Errorf("Authorization = %q", got)
		}
		var payload struct {
			Model string `json:"model"`
		}
		if errDecode := json.NewDecoder(req.Body).Decode(&payload); errDecode != nil {
			t.Errorf("decode upstream request: %v", errDecode)
		}
		if payload.Model != "response-model" {
			t.Errorf("upstream model = %q", payload.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"direct-response"}`))
	}))
	defer upstream.Close()

	restore := installTestConfigAndCommand(t, func(context.Context, pluginConfig) (string, error) {
		return "direct-token", nil
	})
	defer restore()
	cfg := loadedConfig()
	protocol := cfg.Protocols["responses"]
	protocol.BaseURL = upstream.URL + "/api/v1"
	cfg.Protocols["responses"] = protocol
	currentConfig.Store(cfg)

	originalHostCall := hostCall
	hostCall = func(method string, _ any) (json.RawMessage, error) {
		t.Fatalf("unexpected host callback %q for direct transport", method)
		return nil, nil
	}
	defer func() { hostCall = originalHostCall }()

	req := rpcExecutorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthID:       "auth-direct",
		AuthProvider: pluginIdentifier,
		Model:        "response-model",
		SourceFormat: "openai-response",
		Format:       "openai-response",
		Payload:      []byte(`{"model":"team/response-model","input":"hi"}`),
		StorageJSON:  []byte(`{"type":"provider-auth","id":"auth-direct","proxy_url":"direct"}`),
	}}
	rawReq, _ := json.Marshal(req)
	rawResp, errExecute := execute(rawReq)
	if errExecute != nil {
		t.Fatalf("execute() error = %v", errExecute)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(rawResp, &env); errUnmarshal != nil || !env.OK {
		t.Fatalf("execute envelope = %#v, err=%v, raw=%s", env, errUnmarshal, rawResp)
	}
	if strings.Contains(string(rawResp), "direct-token") {
		t.Fatal("execute response leaked the direct transport token")
	}
}

func TestExecuteStreamHonorsDirectProxyFromAuthRecord(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"))
	}))
	defer upstream.Close()

	restore := installTestConfigAndCommand(t, func(context.Context, pluginConfig) (string, error) {
		return "direct-token", nil
	})
	defer restore()
	cfg := loadedConfig()
	protocol := cfg.Protocols["responses"]
	protocol.BaseURL = upstream.URL + "/api/v1"
	cfg.Protocols["responses"] = protocol
	currentConfig.Store(cfg)

	var chunksMu sync.Mutex
	var chunks []byte
	closed := make(chan struct{}, 1)
	originalHostCall := hostCall
	hostCall = func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostStreamEmit:
			req := payload.(pluginStreamEmitRequest)
			chunksMu.Lock()
			chunks = append(chunks, req.Payload...)
			chunksMu.Unlock()
		case pluginabi.MethodHostStreamClose:
			closed <- struct{}{}
		default:
			t.Fatalf("unexpected host callback %q for direct stream", method)
		}
		return json.RawMessage(`{}`), nil
	}
	defer func() { hostCall = originalHostCall }()

	req := rpcExecutorRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			AuthID:       "auth-direct",
			AuthProvider: pluginIdentifier,
			Model:        "response-model",
			SourceFormat: "openai-response",
			Format:       "openai-response",
			Payload:      []byte(`{"model":"team/response-model","input":"hi","stream":true}`),
			StorageJSON:  []byte(`{"type":"provider-auth","id":"auth-direct","proxy_url":"direct"}`),
		},
		StreamID: "plugin-stream-1",
	}
	rawReq, _ := json.Marshal(req)
	rawResp, errExecute := executeStream(rawReq)
	if errExecute != nil {
		t.Fatalf("executeStream() error = %v", errExecute)
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(rawResp, &env); errUnmarshal != nil || !env.OK {
		t.Fatalf("stream envelope = %#v, err=%v, raw=%s", env, errUnmarshal, rawResp)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close")
	}
	chunksMu.Lock()
	defer chunksMu.Unlock()
	if !strings.Contains(string(chunks), "response.completed") {
		t.Fatalf("stream chunks = %q", chunks)
	}
}

func installTestConfigAndCommand(t *testing.T, runner func(context.Context, pluginConfig) (string, error)) func() {
	t.Helper()
	originalRunner := executeCredentialCommand
	executeCredentialCommand = runner
	cfg := validTestConfig(t)
	currentConfig.Store(cfg)
	credentials.reset()
	return func() {
		executeCredentialCommand = originalRunner
		credentials.reset()
		upstreamHTTPClients.reset()
	}
}

func validTestConfig(t *testing.T) pluginConfig {
	t.Helper()
	cfg := defaultPluginConfig()
	cfg.Command = "/usr/local/bin/token-helper"
	cfg.DisableCooling = true
	cfg.Protocols = map[string]configuredProtocol{
		"responses": {
			BaseURL: "https://provider.example/api/v1",
			Headers: map[string]string{"X-Provider-Version": "test"},
		},
		"anthropic": {
			BaseURL: "https://provider.example/anthropic",
		},
		"chat-completions": {
			BaseURL: "https://provider.example/openai",
		},
	}
	cfg.Models = []configuredModel{
		{Name: "response-model", Protocol: "responses"},
		{Name: "message-model-v2", Alias: "message-latest", Protocol: "anthropic"},
		{Name: "chat-model", Protocol: "chat-completions"},
	}
	if errValidate := normalizeAndValidateConfig(&cfg); errValidate != nil {
		t.Fatalf("test config: %v", errValidate)
	}
	return cfg
}
