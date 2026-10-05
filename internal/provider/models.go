package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"cliproxyapi-github-copilot/internal/translate"
	"cliproxyapi-github-copilot/internal/transport"
)

type modelListResponse struct {
	Data []upstreamModel `json:"data"`
}

type upstreamModel struct {
	ID      string `json:"id"`
	Vendor  string `json:"vendor"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Object  string `json:"object"`
	Policy  *struct {
		State string `json:"state"`
	} `json:"policy,omitempty"`
	ModelPickerEnabled  bool              `json:"model_picker_enabled"`
	Preview             bool              `json:"preview"`
	SupportedEndpoints  []string          `json:"supported_endpoints"`
	WarningMessages     []modelMessage    `json:"warning_messages"`
	InformationMessages []modelMessage    `json:"info_messages"`
	Capabilities        modelCapabilities `json:"capabilities"`
}

type modelMessage struct {
	Message string `json:"message"`
}

type modelCapabilities struct {
	Type      string        `json:"type"`
	Tokenizer string        `json:"tokenizer"`
	Family    string        `json:"family"`
	Object    string        `json:"object"`
	Supports  modelSupports `json:"supports"`
	Limits    modelLimits   `json:"limits"`
}

type modelSupports struct {
	ToolCalls         bool     `json:"tool_calls"`
	ParallelToolCalls bool     `json:"parallel_tool_calls"`
	Streaming         bool     `json:"streaming"`
	Vision            bool     `json:"vision"`
	AdaptiveThinking  bool     `json:"adaptive_thinking"`
	ReasoningEffort   []string `json:"reasoning_effort"`
}

type modelLimits struct {
	MaxInputs                   int64 `json:"max_inputs"`
	MaxPromptTokens             int64 `json:"max_prompt_tokens"`
	MaxOutputTokens             int64 `json:"max_output_tokens"`
	MaxNonStreamingOutputTokens int64 `json:"max_non_streaming_output_tokens"`
	MaxContextWindowTokens      int64 `json:"max_context_window_tokens"`
}

type modelCacheEntry struct {
	Fingerprint string
	APIBaseURL  string
	FetchedAt   time.Time
	ExpiresAt   time.Time
	Models      []upstreamModel
	ByID        map[string]upstreamModel
	Error       string
	RetryAt     time.Time
}

func (s *Service) StaticModels() pluginapi.ModelResponse {
	return pluginapi.ModelResponse{Provider: providerID, Models: []pluginapi.ModelInfo{}}
}

// Model snapshots are isolated by credential generation, not just a reusable host ID.
func cacheKey(authID string, storage authStorage) string {
	return authID + "|" + tokenFingerprint(storage.GitHubAccessToken) + "|" + storage.GitHubBaseURL
}

type modelFlight struct {
	done   chan struct{}
	models []upstreamModel
	token  copilotTokenEntry
	err    error
}

func (s *Service) ModelsForAuth(ctx context.Context, callbackID string, req pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	empty := pluginapi.ModelResponse{Provider: providerID, Models: []pluginapi.ModelInfo{}}
	storage, err := parseStorage(req.StorageJSON)
	if err != nil {
		return empty, nil
	}
	models, _, err := s.models(ctx, callbackID, req.AuthID, storage, false)
	// A successful empty set tells v8 to unregister old models. Returning an
	// error leaves the old host registry unchanged. Status retains the failure.
	if err != nil {
		return empty, nil
	}
	return pluginapi.ModelResponse{Provider: providerID, Models: modelInfos(models, s.Config())}, nil
}
func (s *Service) models(ctx context.Context, callbackID, authID string, storage authStorage, force bool) ([]upstreamModel, copilotTokenEntry, error) {
	if !s.Config().Enabled || s.ctx.Err() != nil {
		return nil, copilotTokenEntry{}, errors.New("plugin unavailable")
	}
	if err := s.Config().validateStorageOrigin(storage); err != nil {
		return nil, copilotTokenEntry{}, err
	}
	key := cacheKey(authID, storage)
	now := s.now()
	s.modelMu.Lock()
	if cached, ok := s.modelEntries[key]; !force && ok {
		if cached.Error == "" && now.Before(cached.ExpiresAt) {
			models := cloneUpstreamModels(cached.Models)
			s.modelMu.Unlock()
			token, err := s.copilotToken(ctx, callbackID, authID, storage)
			return models, token, err
		}
		if now.Before(cached.RetryAt) {
			s.modelMu.Unlock()
			return nil, copilotTokenEntry{}, errors.New("account model catalog unavailable")
		}
	}
	flight := s.modelInflight[key]
	if flight == nil {
		flight = &modelFlight{done: make(chan struct{})}
		s.modelInflight[key] = flight
		if !s.spawn(func() {
			requestCtx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
			defer cancel()
			models, token, err := s.fetchModels(requestCtx, "", authID, storage)
			entry := modelCacheEntry{Fingerprint: tokenFingerprint(storage.GitHubAccessToken), APIBaseURL: token.APIBaseURL, FetchedAt: s.now(), ExpiresAt: s.now().Add(s.Config().modelCacheTTL()), Models: cloneUpstreamModels(models)}
			if err != nil {
				entry.Error = "account model discovery failed"
				entry.ExpiresAt = s.now()
				entry.RetryAt = s.now().Add(5 * time.Second)
			}
			s.modelMu.Lock()
			s.modelEntries[key] = entry
			flight.models = models
			flight.token = token
			flight.err = err
			delete(s.modelInflight, key)
			close(flight.done)
			s.modelMu.Unlock()
		}) {
			delete(s.modelInflight, key)
			s.modelMu.Unlock()
			return nil, copilotTokenEntry{}, errors.New("plugin unavailable")
		}
	}
	s.modelMu.Unlock()
	select {
	case <-s.ctx.Done():
		return nil, copilotTokenEntry{}, s.ctx.Err()
	case <-ctx.Done():
		return nil, copilotTokenEntry{}, ctx.Err()
	case <-flight.done:
		return cloneUpstreamModels(flight.models), flight.token, flight.err
	}
}
func (s *Service) fetchModels(ctx context.Context, callbackID, authID string, storage authStorage) ([]upstreamModel, copilotTokenEntry, error) {
	token, err := s.copilotToken(ctx, callbackID, authID, storage)
	if err != nil {
		return nil, token, err
	}
	fetch := func() (transport.Response, error) {
		return s.host.Do(ctx, callbackID, transport.Request{Method: http.MethodGet, URL: token.APIBaseURL + "/models", Headers: modelHeaders(token.Token)})
	}
	resp, err := fetch()
	if err != nil {
		return nil, token, errors.New("model discovery transport failed")
	}
	if resp.StatusCode == 401 {
		s.invalidateToken(authID, storage, token.Token)
		token, err = s.copilotToken(ctx, callbackID, authID, storage)
		if err != nil {
			return nil, token, err
		}
		resp, err = fetch()
		if err != nil {
			return nil, token, err
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, token, upstreamStatusError(resp.StatusCode, "model catalog request rejected")
	}
	var list modelListResponse
	if len(resp.Body) > 8<<20 || json.Unmarshal(resp.Body, &list) != nil || list.Data == nil {
		return nil, token, errors.New("invalid account model catalog")
	}
	return filterModels(normalizeModels(list.Data, s.Config()), s.Config().ModelsExcluded), token, nil
}
func (s *Service) endpointForModel(ctx context.Context, callbackID, authID string, storage authStorage, modelID string) (string, copilotTokenEntry, error) {
	return s.endpointForFormat(ctx, callbackID, authID, storage, modelID, "")
}
func (s *Service) endpointForFormat(ctx context.Context, callbackID, authID string, storage authStorage, modelID, format string) (string, copilotTokenEntry, error) {
	models, token, err := s.models(ctx, callbackID, authID, storage, false)
	if err != nil {
		return "", token, statusError("catalog_unavailable", "account model catalog is unavailable", 503)
	}
	for _, m := range models {
		if m.ID == modelID {
			preferred := map[string]string{"openai": translate.EndpointChatCompletions, "openai-response": translate.EndpointResponses, "claude": translate.EndpointMessages}[format]
			if contains(m.SupportedEndpoints, preferred) {
				return preferred, token, nil
			}
			endpoint, err := selectEndpoint(m)
			return endpoint, token, err
		}
	}
	return "", token, statusError("model_not_found", "model is not enabled in the authenticated account catalog", 404)
}

func selectEndpoint(model upstreamModel) (string, error) {

	endpoints := normalizeEndpoints(model.SupportedEndpoints)
	for _, preferred := range []string{translate.EndpointResponses, translate.EndpointChatCompletions, translate.EndpointMessages} {
		for _, endpoint := range endpoints {
			if endpoint == preferred {
				return preferred, nil
			}
		}
	}
	return "", statusError("unsupported_model_endpoint", "Copilot model exposes no supported chat endpoint", http.StatusUnprocessableEntity)
}

func normalizeModels(models []upstreamModel, cfg Config) []upstreamModel {
	counts := map[string]int{}
	for _, m := range models {
		counts[strings.ToLower(strings.TrimSpace(m.ID))]++
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]upstreamModel, 0, len(models))
	for _, model := range models {
		model.ID = strings.TrimSpace(model.ID)
		if counts[strings.ToLower(model.ID)] > 1 || model.ID == "" || (cfg.ModelPickerRequired && !model.ModelPickerEnabled) || model.Capabilities.Type != "chat" || (model.Policy != nil && model.Policy.State != "enabled") {
			continue
		}
		key := strings.ToLower(model.ID)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		model.Name = strings.TrimSpace(model.Name)
		model.Vendor = strings.TrimSpace(model.Vendor)
		model.Version = strings.TrimSpace(model.Version)
		model.Object = strings.TrimSpace(model.Object)
		model.SupportedEndpoints = normalizeEndpoints(model.SupportedEndpoints)
		if len(model.SupportedEndpoints) == 0 {
			if model.Capabilities.Type == "chat" {
				model.SupportedEndpoints = []string{translate.EndpointChatCompletions}
			} else {
				continue
			}
		}

		out = append(out, model)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].ID) < strings.ToLower(out[j].ID)
	})
	return out
}

func filterModels(models []upstreamModel, excludedPrefixes []string) []upstreamModel {
	if len(excludedPrefixes) == 0 {
		return models
	}
	out := make([]upstreamModel, 0, len(models))
	for _, model := range models {
		modelID := strings.ToLower(model.ID)
		excluded := false
		for _, prefix := range excludedPrefixes {
			if strings.HasPrefix(modelID, prefix) {
				excluded = true
				break
			}
		}
		if !excluded {
			out = append(out, model)
		}
	}
	return out
}

func normalizeEndpoints(endpoints []string) []string {
	seen := make(map[string]struct{}, len(endpoints))
	out := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		switch strings.ToLower(strings.TrimSpace(endpoint)) {
		case "/responses", "responses", "openai-responses":
			endpoint = translate.EndpointResponses
		case "/chat/completions", "chat", "chat-completions":
			endpoint = translate.EndpointChatCompletions
		case "/v1/messages", "messages", "anthropic":
			endpoint = translate.EndpointMessages
		default:
			continue
		}
		if _, exists := seen[endpoint]; exists {
			continue
		}
		seen[endpoint] = struct{}{}
		out = append(out, endpoint)
	}
	return out
}

// Both ID and Name are public registry fields (Name is used by the Gemini list view).
// Keep actual upstream identifiers only in the authenticated upstreamModel catalog.
func modelInfos(models []upstreamModel, cfg Config) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		owner := model.Vendor
		if owner == "" {
			owner = "github-copilot"
		}
		displayName := model.Name
		if displayName == "" {
			displayName = model.ID
		}
		contextLength := model.Capabilities.Limits.MaxContextWindowTokens
		if contextLength == 0 {
			contextLength = model.Capabilities.Limits.MaxPromptTokens + model.Capabilities.Limits.MaxOutputTokens
		}
		parameters := []string{}
		if model.Capabilities.Supports.Streaming {
			parameters = append(parameters, "stream")
		}
		if model.Capabilities.Supports.ToolCalls {
			parameters = append(parameters, "tools", "tool_choice")
		}
		if model.Capabilities.Supports.ParallelToolCalls {
			parameters = append(parameters, "parallel_tool_calls")
		}
		if len(model.Capabilities.Supports.ReasoningEffort) > 0 {
			parameters = append(parameters, "reasoning_effort")
		}
		inputModalities := []string{"TEXT"}
		if model.Capabilities.Supports.Vision {
			inputModalities = append(inputModalities, "IMAGE")
		}
		var thinking *pluginapi.ThinkingSupport
		if model.Capabilities.Supports.AdaptiveThinking || len(model.Capabilities.Supports.ReasoningEffort) > 0 {
			thinking = &pluginapi.ThinkingSupport{
				DynamicAllowed: model.Capabilities.Supports.AdaptiveThinking,
				Levels:         append([]string(nil), model.Capabilities.Supports.ReasoningEffort...),
			}
		}
		for _, alias := range cfg.modelAliases(model.ID) {
			out = append(out, pluginapi.ModelInfo{
				ID:                         alias,
				Object:                     firstNonEmpty(model.Object, "model"),
				OwnedBy:                    owner,
				Type:                       firstNonEmpty(model.Capabilities.Type, "chat"),
				DisplayName:                displayName,
				Name:                       alias,
				Version:                    model.Version,
				Description:                modelDescription(model),
				InputTokenLimit:            model.Capabilities.Limits.MaxPromptTokens,
				OutputTokenLimit:           model.Capabilities.Limits.MaxOutputTokens,
				SupportedGenerationMethods: append([]string(nil), model.SupportedEndpoints...),
				ContextLength:              contextLength,
				MaxCompletionTokens:        model.Capabilities.Limits.MaxOutputTokens,
				SupportedParameters:        parameters,
				SupportedInputModalities:   inputModalities,
				SupportedOutputModalities:  []string{"TEXT"},
				Thinking:                   thinking,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func modelDescription(model upstreamModel) string {
	for _, item := range append(model.WarningMessages, model.InformationMessages...) {
		if message := strings.TrimSpace(item.Message); message != "" {
			return message
		}
	}
	parts := []string{"GitHub Copilot subscription model"}
	if family := strings.TrimSpace(model.Capabilities.Family); family != "" {
		parts = append(parts, "family "+family)
	}
	if len(model.SupportedEndpoints) > 0 {
		parts = append(parts, "endpoints "+strings.Join(model.SupportedEndpoints, ", "))
	}
	return strings.Join(parts, "; ")
}

func cloneUpstreamModels(in []upstreamModel) []upstreamModel {
	out := make([]upstreamModel, len(in))
	copy(out, in)
	for i := range out {
		out[i].SupportedEndpoints = append([]string(nil), in[i].SupportedEndpoints...)
		out[i].Capabilities.Supports.ReasoningEffort = append([]string(nil), in[i].Capabilities.Supports.ReasoningEffort...)
		out[i].WarningMessages = append([]modelMessage(nil), in[i].WarningMessages...)
		out[i].InformationMessages = append([]modelMessage(nil), in[i].InformationMessages...)
	}
	return out
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
