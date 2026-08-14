package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type modelAlias struct {
	Name         string `json:"name"`
	Alias        string `json:"alias"`
	DisplayName  string `json:"display-name,omitempty"`
	ForceMapping bool   `json:"force-mapping,omitempty"`
}

func staticModels(_ []byte) ([]byte, error) {
	return okEnvelope(pluginapi.ModelResponse{Provider: pluginIdentifier, Models: []pluginapi.ModelInfo{}})
}

func modelsForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode auth model request: %w", errUnmarshal)
	}
	if !strings.EqualFold(strings.TrimSpace(req.AuthProvider), pluginIdentifier) {
		return okEnvelope(pluginapi.ModelResponse{})
	}
	return okEnvelope(pluginapi.ModelResponse{
		Provider: pluginIdentifier,
		Models:   configuredModelInfos(loadedConfig().Models),
	})
}

func configuredModelInfos(models []configuredModel) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		info := pluginapi.ModelInfo{
			ID:                         model.Name,
			Object:                     "model",
			OwnedBy:                    pluginIdentifier,
			Type:                       model.Protocol,
			DisplayName:                model.DisplayName,
			Name:                       model.Name,
			SupportedGenerationMethods: []string{"chat"},
			ContextLength:              model.ContextLength,
			MaxCompletionTokens:        model.MaxCompletionTokens,
			SupportedInputModalities:   []string{"text", "image"},
			SupportedOutputModalities:  []string{"text"},
			UserDefined:                true,
		}
		if len(model.ThinkingLevels) > 0 {
			info.Thinking = &pluginapi.ThinkingSupport{Levels: append([]string(nil), model.ThinkingLevels...)}
		}
		out = append(out, info)
	}
	return out
}

func configuredAliasesJSON(models []configuredModel) (string, error) {
	aliases := make([]modelAlias, 0)
	for _, model := range models {
		if strings.TrimSpace(model.Alias) == "" {
			continue
		}
		aliases = append(aliases, modelAlias{
			Name:        model.Name,
			Alias:       model.Alias,
			DisplayName: model.DisplayName,
		})
	}
	if len(aliases) == 0 {
		return "", nil
	}
	raw, errMarshal := json.Marshal(aliases)
	if errMarshal != nil {
		return "", fmt.Errorf("encode model aliases: %w", errMarshal)
	}
	return string(raw), nil
}

func modelSupportsProtocol(modelName, protocol string, models []configuredModel) bool {
	_, ok := configuredUpstreamModel(modelName, protocol, models)
	return ok
}

func configuredUpstreamModel(modelName, protocol string, models []configuredModel) (string, bool) {
	modelName = stripModelSuffix(modelName)
	protocol = normalizeProtocol(protocol)
	for _, model := range models {
		if model.Protocol != protocol {
			continue
		}
		if matchesConfiguredModel(modelName, model.Name) || (model.Alias != "" && matchesConfiguredModel(modelName, model.Alias)) {
			return model.Name, true
		}
	}
	return "", false
}

func matchesConfiguredModel(requested, configured string) bool {
	requested = strings.TrimSpace(requested)
	configured = strings.TrimSpace(configured)
	return strings.EqualFold(requested, configured) || strings.HasSuffix(strings.ToLower(requested), "/"+strings.ToLower(configured))
}

func stripModelSuffix(model string) string {
	model = strings.TrimSpace(model)
	if index := strings.LastIndex(model, "("); index > 0 && strings.HasSuffix(model, ")") {
		return strings.TrimSpace(model[:index])
	}
	return model
}
