package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"cliproxyapi-github-copilot/internal/provider"
	"cliproxyapi-github-copilot/internal/transport"
)

const ID = "github-copilot"

var (
	Version    = "0.1.1"
	Repository = "UNCONFIGURED"
)

type Plugin struct {
	mu          sync.RWMutex
	configureMu sync.Mutex
	service     *provider.Service
	host        transport.Caller
	config      []byte
	// Keep the accepted routing even while disabled so disable/re-enable cannot
	// bypass the restart requirement for credentials and pending OAuth grants.
	authRouting *provider.Config
	configError string
	closed      bool
}

func New(host transport.Caller) *Plugin { return &Plugin{host: host} }
func (p *Plugin) Close() {
	p.configureMu.Lock()
	defer p.configureMu.Unlock()
	p.mu.Lock()
	s := p.service
	p.service = nil
	p.closed = true
	p.mu.Unlock()
	if s != nil {
		s.Shutdown()
	}
}

func (p *Plugin) Dispatch(method string, raw []byte) (out []byte) {
	defer func() {
		if recover() != nil {
			out = errorEnvelope("internal_error", "plugin operation failed safely", 500, false)
		}
	}()
	out, _ = p.handleMethod(method, raw)
	return
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ManagementAPI         bool                         `json:"management_api"`
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type rpcAuthLoginStartRequest struct {
	pluginapi.AuthLoginStartRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcAuthLoginPollRequest struct {
	pluginapi.AuthLoginPollRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcAuthRefreshRequest struct {
	pluginapi.AuthRefreshRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcAuthModelRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (p *Plugin) handleMethod(method string, request []byte) ([]byte, bool) {
	result, errHandle := p.dispatch(method, request)
	if errHandle != nil {
		var statusErr *provider.StatusError
		if errors.As(errHandle, &statusErr) {
			return errorEnvelope(statusErr.Code, statusErr.Message, statusErr.HTTPStatus, statusErr.Retryable), true
		}
		return errorEnvelope("plugin_error", errHandle.Error(), http.StatusInternalServerError, false), true
	}
	raw, errEnvelope := okEnvelope(result)
	if errEnvelope != nil {
		return errorEnvelope("encoding_error", errEnvelope.Error(), http.StatusInternalServerError, false), true
	}
	return raw, false
}

func (p *Plugin) dispatch(method string, request []byte) (any, error) {
	ctx := context.Background()
	p.mu.RLock()
	pluginService := p.service
	p.mu.RUnlock()
	if pluginService != nil {
		ctx = pluginService.Context()
	}
	if pluginService == nil && method != "plugin.register" && method != "plugin.reconfigure" && method != "plugin.quiesce" && method != "plugin.shutdown" && method != "management.register" && method != "management.handle" && method != "model.static" {
		return nil, &provider.StatusError{Code: "unavailable", Message: "plugin is disabled or not configured", HTTPStatus: 503}
	}
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var req lifecycleRequest
		if len(request) > 0 {
			if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
				return nil, errUnmarshal
			}
		}
		if req.SchemaVersion < 6 {
			return nil, &provider.StatusError{Code: "unsupported_host", Message: "CLIProxyAPI schema 6 is required", HTTPStatus: 400}
		}
		p.configureMu.Lock()
		defer p.configureMu.Unlock()
		p.mu.RLock()
		same := p.authRouting != nil && bytes.Equal(p.config, req.ConfigYAML)
		closed := p.closed
		p.mu.RUnlock()
		if closed {
			return nil, &provider.StatusError{Code: "unavailable", Message: "plugin has been shut down", HTTPStatus: http.StatusServiceUnavailable}
		}
		if same {
			p.mu.Lock()
			p.configError = ""
			p.mu.Unlock()
			return pluginRegistration(), nil
		}
		next := provider.New(transport.New(p.host))
		if err := next.Configure(req.ConfigYAML); err != nil {
			next.Shutdown()
			p.mu.Lock()
			p.configError = err.Error()
			p.mu.Unlock()
			return nil, &provider.StatusError{Code: "invalid_config", Message: err.Error(), HTTPStatus: http.StatusBadRequest}
		}
		cfg := next.Config()
		p.mu.RLock()
		routing := p.authRouting
		if p.service != nil {
			active := p.service.Config()
			routing = &active
		}
		p.mu.RUnlock()
		if routing != nil && !routing.SameAuthRouting(cfg) {
			next.Shutdown()
			return nil, &provider.StatusError{Code: "restart_required", Message: "authentication endpoint, client, scope, or trust changes require a plugin restart", HTTPStatus: http.StatusBadRequest}
		}
		if !cfg.Enabled {
			next.Shutdown()
			next = nil
		}
		p.mu.Lock()
		old := p.service
		p.service = next
		p.config = append([]byte(nil), req.ConfigYAML...)
		p.authRouting = &cfg
		p.configError = ""
		p.mu.Unlock()
		if old != nil {
			old.Shutdown()
		}
		if next != nil {
			next.Start()
		}

		return pluginRegistration(), nil
	case "plugin.quiesce", "plugin.shutdown":
		p.Close()
		return struct{}{}, nil
	case "management.register":
		return managementRegistration(), nil
	case "management.handle":
		return p.manage(request)
	case pluginabi.MethodModelStatic:
		if pluginService == nil {
			return pluginapi.ModelResponse{Provider: "copilot", Models: []pluginapi.ModelInfo{}}, nil
		}
		return pluginService.StaticModels(), nil
	case pluginabi.MethodModelForAuth:
		var req rpcAuthModelRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		return pluginService.ModelsForAuth(ctx, req.HostCallbackID, req.AuthModelRequest)
	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier:
		return identifierResponse{Identifier: "copilot"}, nil
	case pluginabi.MethodAuthParse:
		var req pluginapi.AuthParseRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		return pluginService.ParseAuth(req)
	case pluginabi.MethodAuthLoginStart:
		var req rpcAuthLoginStartRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		return pluginService.StartLogin(ctx, req.HostCallbackID)
	case pluginabi.MethodAuthLoginPoll:
		var req rpcAuthLoginPollRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		return pluginService.PollLogin(ctx, req.HostCallbackID, req.State)
	case pluginabi.MethodAuthRefresh:
		var req rpcAuthRefreshRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		return pluginService.RefreshAuth(ctx, req.HostCallbackID, req.AuthRefreshRequest)
	case pluginabi.MethodExecutorExecute:
		var req provider.ExecuteRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		return pluginService.Execute(ctx, req)
	case pluginabi.MethodExecutorExecuteStream:
		var req provider.ExecuteRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		headers, errStream := pluginService.ExecuteStream(ctx, req)
		if errStream != nil {
			return nil, errStream
		}
		return map[string]any{"headers": headers}, nil
	case pluginabi.MethodExecutorCountTokens:
		var req provider.ExecuteRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		return pluginService.CountTokensChecked(ctx, req)
	case pluginabi.MethodExecutorHTTPRequest:
		var req provider.HTTPRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}
		return pluginService.HTTP(ctx, req)
	default:
		return nil, &provider.StatusError{
			Code:       "unknown_method",
			Message:    "unknown plugin method: " + method,
			HTTPStatus: http.StatusNotImplemented,
		}
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "GitHub Copilot Subscription Provider",
			Version:          Version,
			Author:           "syepes",
			GitHubRepository: Repository,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "model_prefix", Type: pluginapi.ConfigFieldTypeString, Description: "Prefix for discovered model aliases (default copilot/)."},
				{Name: "models", Type: pluginapi.ConfigFieldTypeArray, Description: "Optional model allowlist: objects with name and optional alias. Empty discovers all eligible account models."},
				{Name: "models_excluded", Type: pluginapi.ConfigFieldTypeArray, Description: "Case-insensitive model ID prefixes omitted from Copilot discovery to prevent collisions with native providers."},
				{Name: "model_cache_ttl_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "In-memory Copilot model catalog cache lifetime."},
				{Name: "github_client_id", Type: pluginapi.ConfigFieldTypeString, Description: "Public GitHub OAuth application client identifier used for device flow."},
				{Name: "github_scope", Type: pluginapi.ConfigFieldTypeString, Description: "Space-delimited GitHub OAuth scopes; defaults to the least-privilege read:user scope."},
				{Name: "github_base_url", Type: pluginapi.ConfigFieldTypeString, Description: "GitHub web OAuth base URL."},
				{Name: "github_api_url", Type: pluginapi.ConfigFieldTypeString, Description: "GitHub REST API base URL used for identity and Copilot token exchange."},
				{Name: "copilot_api_url", Type: pluginapi.ConfigFieldTypeString, Description: "Fallback Copilot API base URL when the token response has no API endpoint."},
				{Name: "allow_insecure_base_urls", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Opt in to HTTP loopback endpoints for local testing only; disabled by default. Changing this requires a plugin restart."},
				{Name: "allow_custom_endpoints", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Opt in to custom HTTPS endpoints outside the default GitHub and Copilot origins; disabled by default. Only enable for endpoints you trust with credentials. GHE.com tenant restrictions still apply. Changing this requires a plugin restart."},
				{Name: "allow_custom_scopes", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Opt in to OAuth scopes other than read:user; disabled by default. Broader scopes may grant additional account access. Changing this requires a plugin restart."},
				{Name: "oauth_timeout_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Maximum device-code lifetime accepted by the plugin."},
				{Name: "token_expiry_buffer_seconds", Type: pluginapi.ConfigFieldTypeInteger, Description: "Refresh Copilot API tokens this long before expiration."},
			},
		},
		Capabilities: registrationCapability{
			ManagementAPI:         true,
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeOAuth,
			ExecutorInputFormats:  []string{"openai", "openai-response", "claude"},
			ExecutorOutputFormats: []string{"openai", "openai-response", "claude"},
		},
	}
}

func okEnvelope(value any) ([]byte, error) {
	result, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: result})
}

func errorEnvelope(code, message string, status int, retryable bool) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{
		OK: false,
		Error: &pluginabi.Error{
			Code:       code,
			Message:    message,
			HTTPStatus: status,
			Retryable:  retryable,
		},
	})
	return raw
}
