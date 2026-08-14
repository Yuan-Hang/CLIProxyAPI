package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginIdentifier = "provider-auth"
	pluginVersion    = "0.2.0"
)

var currentConfig atomic.Value

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type pluginConfig struct {
	Enabled                bool                          `yaml:"enabled"`
	Command                string                        `yaml:"command"`
	Args                   []string                      `yaml:"args"`
	TimeoutMS              int                           `yaml:"timeout-ms"`
	RefreshIntervalSeconds int                           `yaml:"refresh-interval-seconds"`
	DisableCooling         bool                          `yaml:"disable-cooling"`
	Protocols              map[string]configuredProtocol `yaml:"protocols"`
	Models                 []configuredModel             `yaml:"models"`
}

type configuredProtocol struct {
	BaseURL string            `yaml:"base-url" json:"base_url"`
	Path    string            `yaml:"path,omitempty" json:"path,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
}

type configuredModel struct {
	Name                string   `yaml:"name" json:"name"`
	Alias               string   `yaml:"alias,omitempty" json:"alias,omitempty"`
	DisplayName         string   `yaml:"display-name,omitempty" json:"display_name,omitempty"`
	Protocol            string   `yaml:"protocol" json:"protocol"`
	ContextLength       int64    `yaml:"context-length,omitempty" json:"context_length,omitempty"`
	MaxCompletionTokens int64    `yaml:"max-completion-tokens,omitempty" json:"max_completion_tokens,omitempty"`
	ThinkingLevels      []string `yaml:"thinking-levels,omitempty" json:"thinking_levels,omitempty"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelProvider         bool     `json:"model_provider"`
	AuthProvider          bool     `json:"auth_provider"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required", 0, false))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error(), 0, false))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	credentials.reset()
	upstreamHTTPClients.reset()
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": pluginIdentifier})
	case pluginabi.MethodAuthParse:
		return parseAuth(request)
	case pluginabi.MethodAuthRefresh:
		return refreshAuth(request)
	case pluginabi.MethodAuthLoginStart, pluginabi.MethodAuthLoginPoll:
		return errorEnvelope("unsupported_login", "provider-auth uses file-backed auth records", 400, false), nil
	case pluginabi.MethodModelStatic:
		return staticModels(request)
	case pluginabi.MethodModelForAuth:
		return modelsForAuth(request)
	case pluginabi.MethodExecutorExecute:
		return execute(request)
	case pluginabi.MethodExecutorExecuteStream:
		return executeStream(request)
	case pluginabi.MethodExecutorCountTokens:
		return countTokens(request)
	case pluginabi.MethodExecutorHTTPRequest:
		return executeHTTPRequest(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method, 0, false), nil
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return fmt.Errorf("decode lifecycle request: %w", errUnmarshal)
		}
	}
	cfg := defaultPluginConfig()
	if len(req.ConfigYAML) > 0 {
		if errUnmarshal := yaml.Unmarshal(req.ConfigYAML, &cfg); errUnmarshal != nil {
			return fmt.Errorf("decode plugin config: %w", errUnmarshal)
		}
	}
	if errValidate := normalizeAndValidateConfig(&cfg); errValidate != nil {
		return errValidate
	}
	currentConfig.Store(cfg)
	credentials.reset()
	return nil
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:                true,
		TimeoutMS:              5000,
		RefreshIntervalSeconds: 300,
		Protocols:              map[string]configuredProtocol{},
	}
}

func normalizeAndValidateConfig(cfg *pluginConfig) error {
	if cfg == nil {
		return fmt.Errorf("plugin config is nil")
	}
	cfg.Command = strings.TrimSpace(cfg.Command)
	if cfg.Command == "" {
		return fmt.Errorf("command is required")
	}
	if !filepath.IsAbs(cfg.Command) {
		return fmt.Errorf("command must be an absolute path")
	}
	if cfg.TimeoutMS <= 0 || cfg.TimeoutMS > 60000 {
		return fmt.Errorf("timeout-ms must be between 1 and 60000")
	}
	if cfg.RefreshIntervalSeconds <= 0 || cfg.RefreshIntervalSeconds > 86400 {
		return fmt.Errorf("refresh-interval-seconds must be between 1 and 86400")
	}
	if len(cfg.Protocols) == 0 {
		return fmt.Errorf("at least one protocol is required")
	}
	normalizedProtocols := make(map[string]configuredProtocol, len(cfg.Protocols))
	for rawName, protocolCfg := range cfg.Protocols {
		protocol := normalizeProtocol(rawName)
		if protocol == "" {
			return fmt.Errorf("unsupported protocol %q", rawName)
		}
		if _, exists := normalizedProtocols[protocol]; exists {
			return fmt.Errorf("duplicate protocol %q", protocol)
		}
		baseURL, errURL := normalizeBaseURL(protocolCfg.BaseURL)
		if errURL != nil {
			return fmt.Errorf("protocols.%s.base-url: %w", rawName, errURL)
		}
		path, errPath := normalizeEndpointPath(protocolCfg.Path, defaultProtocolPath(protocol))
		if errPath != nil {
			return fmt.Errorf("protocols.%s.path: %w", rawName, errPath)
		}
		headers, errHeaders := normalizeProtocolHeaders(protocolCfg.Headers)
		if errHeaders != nil {
			return fmt.Errorf("protocols.%s.headers: %w", rawName, errHeaders)
		}
		normalizedProtocols[protocol] = configuredProtocol{BaseURL: baseURL, Path: path, Headers: headers}
	}
	cfg.Protocols = normalizedProtocols
	if len(cfg.Models) == 0 {
		return fmt.Errorf("at least one model is required")
	}
	seenNames := make(map[string]struct{}, len(cfg.Models))
	seenAliases := make(map[string]struct{}, len(cfg.Models))
	for i := range cfg.Models {
		model := &cfg.Models[i]
		model.Name = strings.TrimSpace(model.Name)
		model.Alias = strings.TrimSpace(model.Alias)
		model.DisplayName = strings.TrimSpace(model.DisplayName)
		model.Protocol = normalizeProtocol(model.Protocol)
		if model.Name == "" {
			return fmt.Errorf("models[%d].name is required", i)
		}
		if model.Protocol == "" {
			return fmt.Errorf("models[%d].protocol must be responses, anthropic, or chat-completions", i)
		}
		if _, exists := cfg.Protocols[model.Protocol]; !exists {
			return fmt.Errorf("models[%d].protocol %q is not configured", i, model.Protocol)
		}
		nameKey := strings.ToLower(model.Name)
		if _, exists := seenNames[nameKey]; exists {
			return fmt.Errorf("duplicate model name %q", model.Name)
		}
		seenNames[nameKey] = struct{}{}
		if model.Alias != "" {
			aliasKey := strings.ToLower(model.Alias)
			if _, exists := seenAliases[aliasKey]; exists {
				return fmt.Errorf("duplicate model alias %q", model.Alias)
			}
			seenAliases[aliasKey] = struct{}{}
		}
		if model.ContextLength <= 0 {
			model.ContextLength = 1048576
		}
		if model.MaxCompletionTokens <= 0 {
			model.MaxCompletionTokens = 32000
		}
		model.ThinkingLevels = normalizeStrings(model.ThinkingLevels)
	}
	return nil
}

func normalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	parsed, errParse := url.Parse(raw)
	if errParse != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("must be an absolute HTTP URL")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("query and fragment are not allowed")
	}
	return raw, nil
}

func normalizeProtocol(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "responses", "openai-responses", "openai-response":
		return "responses"
	case "anthropic", "claude":
		return "anthropic"
	case "chat-completions", "chat_completions", "openai", "openai-chat-completions":
		return "chat-completions"
	default:
		return ""
	}
}

func defaultProtocolPath(protocol string) string {
	switch normalizeProtocol(protocol) {
	case "responses":
		return "/responses"
	case "anthropic":
		return "/messages"
	case "chat-completions":
		return "/chat/completions"
	default:
		return ""
	}
}

func normalizeEndpointPath(raw, fallback string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = fallback
	}
	if raw == "" || !strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("must be an absolute URL path")
	}
	parsed, errParse := url.Parse(raw)
	if errParse != nil || parsed.IsAbs() || parsed.Host != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("must be a path without query or fragment")
	}
	return "/" + strings.TrimLeft(parsed.Path, "/"), nil
}

func normalizeProtocolHeaders(raw map[string]string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(raw))
	for key, value := range raw {
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("header name is empty")
		}
		if shouldDropForwardedHeader(key) {
			return nil, fmt.Errorf("header %q is managed by the host", key)
		}
		out[key] = strings.TrimSpace(value)
	}
	return out, nil
}

func normalizeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}

func loadedConfig() pluginConfig {
	raw := currentConfig.Load()
	if cfg, ok := raw.(pluginConfig); ok {
		return cfg
	}
	return defaultPluginConfig()
}

func pluginRegistration() registration {
	formats := configuredExecutorFormats(loadedConfig())
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Provider auth",
			Version:          pluginVersion,
			Author:           "CLIProxyAPI contributors",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "command", Type: pluginapi.ConfigFieldTypeString, Description: "Absolute executable path used to obtain a temporary bearer token."},
				{Name: "args", Type: pluginapi.ConfigFieldTypeArray, Description: "Arguments passed directly to the credential command."},
				{Name: "timeout-ms", Type: pluginapi.ConfigFieldTypeInteger, Description: "Credential command timeout in milliseconds."},
				{Name: "refresh-interval-seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Credential refresh interval in seconds."},
				{Name: "disable-cooling", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Disable host cooldown for command-backed credentials."},
				{Name: "protocols", Type: pluginapi.ConfigFieldTypeObject, Description: "Upstream base URL, path, and headers keyed by protocol."},
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Models and aliases exposed by this provider."},
			},
		},
		Capabilities: registrationCapability{
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeOAuth),
			ExecutorInputFormats:  formats,
			ExecutorOutputFormats: formats,
		},
	}
}

func configuredExecutorFormats(cfg pluginConfig) []string {
	formats := make([]string, 0, len(cfg.Protocols))
	for protocol := range cfg.Protocols {
		formats = append(formats, protocol)
	}
	sort.Strings(formats)
	return formats
}

func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal host callback %s: %w", method, errMarshal)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
	}
	var env envelope
	if errUnmarshal := json.Unmarshal(rawResponse, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host envelope %s: %w", method, errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, int(callCode))
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

var hostCall = callHost

func okEnvelope(value any) ([]byte, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string, status int, retryable bool) []byte {
	raw, _ := json.Marshal(envelope{
		OK: false,
		Error: &envelopeError{
			Code:       code,
			Message:    message,
			Retryable:  retryable,
			HTTPStatus: status,
		},
	})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
