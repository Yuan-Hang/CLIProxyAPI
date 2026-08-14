package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const maxCredentialBytes = 64 * 1024

type authRecord struct {
	Type     string `json:"type"`
	ID       string `json:"id,omitempty"`
	Label    string `json:"label,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	ProxyURL string `json:"proxy_url,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

type tokenCache struct {
	mu        sync.RWMutex
	refreshMu sync.Mutex
	tokens    map[string]string
}

var credentials = &tokenCache{tokens: make(map[string]string)}

func (c *tokenCache) reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.tokens = make(map[string]string)
	c.mu.Unlock()
}

func (c *tokenCache) invalidate(authID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.tokens, strings.TrimSpace(authID))
	c.mu.Unlock()
}

func (c *tokenCache) get(authID string) string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	token := c.tokens[strings.TrimSpace(authID)]
	c.mu.RUnlock()
	return token
}

func (c *tokenCache) credential(ctx context.Context, authID string, force bool) (string, error) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return "", fmt.Errorf("auth ID is required")
	}
	if !force {
		if token := c.get(authID); token != "" {
			return token, nil
		}
	}
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if !force {
		if token := c.get(authID); token != "" {
			return token, nil
		}
	}
	token, errCommand := executeCredentialCommand(ctx, loadedConfig())
	if errCommand != nil {
		return "", errCommand
	}
	c.mu.Lock()
	c.tokens[authID] = token
	c.mu.Unlock()
	return token, nil
}

var executeCredentialCommand = runCredentialCommand

func runCredentialCommand(ctx context.Context, cfg pluginConfig) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := time.Duration(cfg.TimeoutMS) * time.Millisecond
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, cfg.Command, cfg.Args...)
	output, errOutput := cmd.Output()
	if errOutput != nil {
		if commandCtx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("credential command timed out after %s", timeout)
		}
		return "", fmt.Errorf("credential command failed: %w", errOutput)
	}
	if len(output) > maxCredentialBytes {
		return "", fmt.Errorf("credential command output exceeds %d bytes", maxCredentialBytes)
	}
	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", fmt.Errorf("credential command returned an empty token")
	}
	if strings.IndexFunc(token, unicode.IsSpace) >= 0 {
		return "", fmt.Errorf("credential command returned a token containing whitespace")
	}
	return token, nil
}

func parseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode auth parse request: %w", errUnmarshal)
	}
	record, handled, errRecord := decodeAuthRecord(req.RawJSON)
	if errRecord != nil {
		return nil, errRecord
	}
	if !handled {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	if _, errCredential := credentials.credential(context.Background(), record.ID, false); errCredential != nil {
		return errorEnvelope("credential_command_failed", errCredential.Error(), 503, true), nil
	}
	data, errData := authDataForRecord(record)
	if errData != nil {
		return nil, errData
	}
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: data})
}

func refreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode auth refresh request: %w", errUnmarshal)
	}
	record, handled, errRecord := decodeAuthRecord(req.StorageJSON)
	if errRecord != nil {
		return nil, errRecord
	}
	if !handled {
		return errorEnvelope("invalid_auth", "auth storage is not a provider-auth record", 400, false), nil
	}
	if record.ID == "" {
		record.ID = strings.TrimSpace(req.AuthID)
	}
	if _, errCredential := credentials.credential(context.Background(), record.ID, true); errCredential != nil {
		return errorEnvelope("credential_command_failed", errCredential.Error(), 503, true), nil
	}
	data, errData := authDataForRecord(record)
	if errData != nil {
		return nil, errData
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             data,
		NextRefreshAfter: time.Now().UTC().Add(time.Duration(loadedConfig().RefreshIntervalSeconds) * time.Second),
	})
}

func decodeAuthRecord(raw []byte) (authRecord, bool, error) {
	if len(raw) == 0 {
		return authRecord{}, false, nil
	}
	var record authRecord
	if errUnmarshal := json.Unmarshal(raw, &record); errUnmarshal != nil {
		return authRecord{}, false, fmt.Errorf("decode auth record: %w", errUnmarshal)
	}
	if !strings.EqualFold(strings.TrimSpace(record.Type), pluginIdentifier) {
		return authRecord{}, false, nil
	}
	record.Type = pluginIdentifier
	record.ID = strings.TrimSpace(record.ID)
	if record.ID == "" {
		record.ID = pluginIdentifier + "-default"
	}
	record.Label = strings.TrimSpace(record.Label)
	if record.Label == "" {
		record.Label = "Command bearer token"
	}
	record.Prefix = strings.Trim(strings.TrimSpace(record.Prefix), "/")
	record.ProxyURL = strings.TrimSpace(record.ProxyURL)
	return record, true, nil
}

func authDataForRecord(record authRecord) (pluginapi.AuthData, error) {
	storageJSON, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		return pluginapi.AuthData{}, fmt.Errorf("encode auth storage: %w", errMarshal)
	}
	attributes := map[string]string{"auth_kind": "oauth"}
	aliasesJSON, errAliases := configuredAliasesJSON(loadedConfig().Models)
	if errAliases != nil {
		return pluginapi.AuthData{}, errAliases
	}
	if aliasesJSON != "" {
		attributes["model_aliases"] = aliasesJSON
	}
	cfg := loadedConfig()
	now := time.Now().UTC()
	return pluginapi.AuthData{
		Provider:    pluginIdentifier,
		ID:          record.ID,
		Label:       record.Label,
		Prefix:      record.Prefix,
		ProxyURL:    record.ProxyURL,
		Disabled:    record.Disabled,
		StorageJSON: storageJSON,
		Metadata: map[string]any{
			"type":                     pluginIdentifier,
			"auth_kind":                "oauth",
			"refresh_interval_seconds": cfg.RefreshIntervalSeconds,
			"last_refresh":             now.Format(time.RFC3339),
			"disable_cooling":          cfg.DisableCooling,
		},
		Attributes:       attributes,
		NextRefreshAfter: now.Add(time.Duration(cfg.RefreshIntervalSeconds) * time.Second),
	}, nil
}
