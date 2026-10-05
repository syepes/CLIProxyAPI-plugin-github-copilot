package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"cliproxyapi-github-copilot/internal/sse"
	"cliproxyapi-github-copilot/internal/translate"
	"cliproxyapi-github-copilot/internal/transport"
)

const (
	copilotUserAgent     = "GitHubCopilotChat/0.67.0"
	copilotEditorVersion = "vscode/1.139.1"
	copilotPluginVersion = "copilot-chat/0.67.0"
	copilotIntegrationID = "vscode-chat"
	copilotAPIVersion    = "2026-08-01"
	streamIdleTimeout    = 2 * time.Minute
	streamTotalTimeout   = 30 * time.Minute
	streamMaxChoices     = 256
)

type streamTimeouts struct{ idle, total time.Duration }

type ExecuteRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type HTTPRequest struct {
	pluginapi.ExecutorHTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (s *Service) Execute(ctx context.Context, req ExecuteRequest) (pluginapi.ExecutorResponse, error) {
	sourceFormat := normalizeRequestFormat(firstNonEmpty(req.SourceFormat, req.Format))
	if sourceFormat == "" {
		return pluginapi.ExecutorResponse{}, statusError("unsupported_format", fmt.Sprintf("unsupported request format %q", firstNonEmpty(req.SourceFormat, req.Format)), http.StatusUnprocessableEntity)
	}
	nativeModel, errModel := s.Config().upstreamModelID(req.Model)
	if errModel != nil {
		return pluginapi.ExecutorResponse{}, errModel
	}
	storage, errParse := parseStorage(req.StorageJSON)
	if errParse != nil {
		return pluginapi.ExecutorResponse{}, errParse
	}
	endpoint, token, errEndpoint := s.endpointForFormat(ctx, req.HostCallbackID, req.AuthID, storage, nativeModel, sourceFormat)
	if errEndpoint != nil {
		return pluginapi.ExecutorResponse{}, errEndpoint
	}
	requestBody, errTranslate := translate.RequestForEndpointFrom(sourceFormat, nativeModel, req.Payload, false, endpoint)
	if errTranslate != nil {
		return pluginapi.ExecutorResponse{}, statusError("translation_error", errTranslate.Error(), http.StatusUnprocessableEntity)
	}
	resp, token, errDo := s.doModelRequest(ctx, req.HostCallbackID, req.AuthID, storage, token, endpoint, requestBody, false)
	if errDo != nil {
		return pluginapi.ExecutorResponse{}, errDo
	}
	body, errResponse := translate.ResponseFromEndpoint(ctx, endpoint, sourceFormat, req.Model, req.OriginalRequest, requestBody, resp.Body)
	if errResponse != nil {
		return pluginapi.ExecutorResponse{}, statusError("translation_error", errResponse.Error(), http.StatusBadGateway)
	}
	body, errResponse = rewriteResponseModel(body, req.Model)
	if errResponse != nil {
		return pluginapi.ExecutorResponse{}, statusError("translation_error", "cannot encode namespaced response", 502)
	}
	return pluginapi.ExecutorResponse{
		Payload: body,
		Headers: filterResponseHeaders(resp.Headers),
		Metadata: map[string]any{
			"copilot_endpoint": endpoint,
			"token_expires_at": token.ExpiresAt.UTC().Format(http.TimeFormat),
		},
	}, nil
}

func (s *Service) ExecuteStream(ctx context.Context, req ExecuteRequest) (http.Header, error) {
	return s.executeStreamWithTimeouts(ctx, req, streamTimeouts{idle: streamIdleTimeout, total: streamTotalTimeout})
}

// The opening request and pump share one lifetime. Canceling a temporary opening
// context after headers would also cancel the host's underlying HTTP request.
func (s *Service) executeStreamWithTimeouts(ctx context.Context, req ExecuteRequest, limits streamTimeouts) (http.Header, error) {
	if strings.TrimSpace(req.StreamID) == "" {
		return nil, statusError("invalid_request", "stream_id is required", http.StatusBadRequest)
	}
	sourceFormat := normalizeRequestFormat(firstNonEmpty(req.SourceFormat, req.Format))
	if sourceFormat == "" {
		return nil, statusError("unsupported_format", fmt.Sprintf("unsupported request format %q", firstNonEmpty(req.SourceFormat, req.Format)), http.StatusUnprocessableEntity)
	}
	nativeModel, errModel := s.Config().upstreamModelID(req.Model)
	if errModel != nil {
		return nil, errModel
	}
	storage, errParse := parseStorage(req.StorageJSON)
	if errParse != nil {
		return nil, errParse
	}
	ctx, cancel := context.WithTimeout(ctx, limits.total)
	shutdownDone := make(chan struct{})
	stopShutdown := context.AfterFunc(s.ctx, func() { defer close(shutdownDone); cancel() })
	finish := func() {
		cancel()
		if !stopShutdown() {
			<-shutdownDone
		}
	}
	transferred := false
	defer func() {
		if !transferred {
			finish()
		}
	}()
	endpoint, token, errEndpoint := s.endpointForFormat(ctx, req.HostCallbackID, req.AuthID, storage, nativeModel, sourceFormat)
	if errEndpoint != nil {
		return nil, errEndpoint
	}
	requestBody, errTranslate := translate.RequestForEndpointFrom(sourceFormat, nativeModel, req.Payload, true, endpoint)
	if errTranslate != nil {
		return nil, statusError("translation_error", errTranslate.Error(), http.StatusUnprocessableEntity)
	}
	// Bound waiting for the first headers as well as later reads. Join a fired
	// timer before proceeding so it cannot cancel a successfully transferred pump.
	upstream, token, errOpen := func() (transport.Stream, copilotTokenEntry, error) {
		timeoutDone := make(chan struct{})
		timer := time.AfterFunc(limits.idle, func() { defer close(timeoutDone); cancel() })
		defer func() {
			if !timer.Stop() {
				<-timeoutDone
			}
		}()
		return s.openModelStream(ctx, req.HostCallbackID, req.AuthID, storage, token, endpoint, requestBody)
	}()
	if errOpen != nil {
		return nil, errOpen
	}
	if err := ctx.Err(); err != nil {
		_ = s.host.CloseStream(context.Background(), upstream.ID)
		return nil, err
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		if _, errCollect := s.collectStreamError(ctx, upstream, limits.idle); errCollect != nil {
			return nil, errCollect
		}
		return nil, upstreamStatusError(upstream.StatusCode, "Copilot upstream request failed")
	}
	if !s.spawn(func() {
		defer finish()
		s.pumpStream(ctx, req.StreamID, endpoint, sourceFormat, req.Model, req.OriginalRequest, requestBody, upstream, limits.idle)
	}) {
		_ = s.host.CloseStream(context.Background(), upstream.ID)
		return nil, errors.New("plugin shutting down")
	}
	transferred = true
	headers := filterResponseHeaders(upstream.Headers)
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	return headers, nil
}

func (s *Service) doModelRequest(ctx context.Context, callbackID, authID string, storage authStorage, token copilotTokenEntry, endpoint string, body []byte, stream bool) (transport.Response, copilotTokenEntry, error) {
	request := transport.Request{
		Method:  http.MethodPost,
		URL:     token.APIBaseURL + endpoint,
		Headers: inferenceHeaders(token.Token, stream, body),
		Body:    body,
	}
	resp, errDo := s.host.Do(ctx, callbackID, request)
	if errDo != nil {
		return transport.Response{}, token, fmt.Errorf("call Copilot model endpoint: %w", errDo)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		s.invalidateToken(authID, storage, token.Token)
		refreshed, errToken := s.copilotToken(ctx, callbackID, authID, storage)
		if errToken != nil {
			return transport.Response{}, token, errToken
		}
		token = refreshed
		request.URL = token.APIBaseURL + endpoint
		request.Headers = inferenceHeaders(token.Token, stream, body)
		resp, errDo = s.host.Do(ctx, callbackID, request)
		if errDo != nil {
			return transport.Response{}, token, fmt.Errorf("call Copilot model endpoint after token refresh: %w", errDo)
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, token, upstreamStatusError(resp.StatusCode, "Copilot upstream request failed")
	}
	return resp, token, nil
}

func (s *Service) openModelStream(ctx context.Context, callbackID, authID string, storage authStorage, token copilotTokenEntry, endpoint string, body []byte) (transport.Stream, copilotTokenEntry, error) {
	request := transport.Request{
		Method:  http.MethodPost,
		URL:     token.APIBaseURL + endpoint,
		Headers: inferenceHeaders(token.Token, true, body),
		Body:    body,
	}
	stream, errOpen := s.host.OpenStream(ctx, callbackID, request)
	if errOpen != nil {
		return transport.Stream{}, token, fmt.Errorf("open Copilot model stream: %w", errOpen)
	}
	if stream.StatusCode == http.StatusUnauthorized {
		_ = s.host.CloseStream(ctx, stream.ID)
		s.invalidateToken(authID, storage, token.Token)
		refreshed, errToken := s.copilotToken(ctx, callbackID, authID, storage)
		if errToken != nil {
			return transport.Stream{}, token, errToken
		}
		token = refreshed
		request.URL = token.APIBaseURL + endpoint
		request.Headers = inferenceHeaders(token.Token, true, body)
		stream, errOpen = s.host.OpenStream(ctx, callbackID, request)
		if errOpen != nil {
			return transport.Stream{}, token, fmt.Errorf("open Copilot model stream after token refresh: %w", errOpen)
		}
	}
	return stream, token, nil
}

func (s *Service) collectStreamError(ctx context.Context, stream transport.Stream, idleTimeout time.Duration) ([]byte, error) {
	defer func() { _ = s.host.CloseStream(context.Background(), stream.ID) }()
	var body []byte
	lastActivity := time.Now()
	for {
		readCtx, cancel := context.WithDeadline(ctx, lastActivity.Add(idleTimeout))
		chunk, errRead := s.host.ReadStream(readCtx, stream.ID)
		cancel()
		if errRead != nil {
			return nil, fmt.Errorf("read Copilot error stream: %w", errRead)
		}
		if len(chunk.Payload) > 0 {
			lastActivity = time.Now()
		}
		if len(body)+len(chunk.Payload) > 1<<20 {
			return nil, errors.New("upstream error body too large")
		}
		body = append(body, chunk.Payload...)
		if chunk.Error != "" {
			return nil, errors.New("upstream error stream transport failed")
		}
		if chunk.Done {
			return body, nil
		}
	}
}

func (s *Service) pumpStream(ctx context.Context, outputID, endpoint, destination, model string, original, translated []byte, upstream transport.Stream, idleTimeout time.Duration) {
	var terminalErr error
	closeUpstream := sync.OnceFunc(func() { _ = s.host.CloseStream(context.Background(), upstream.ID) })
	defer func() {
		if recover() != nil {
			terminalErr = errors.New("upstream stream processing failed")
		}
		closeUpstream()
		message := ""
		if terminalErr != nil {
			message = "copilot stream failed or was canceled" // Upstream translation errors may contain private response text.
		}
		s.host.CloseOutput(ctx, outputID, message)
	}()

	decoder := &sse.Decoder{}
	var state any
	terminal := false
	finishedChoices := make(map[int]bool)
	emit := func(frame []byte) error {
		eventName, data := streamEventData(frame)
		var event struct {
			Type    string          `json:"type"`
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Index        int     `json:"index"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		isDone := endpoint == translate.EndpointChatCompletions && bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]"))
		if len(bytes.TrimSpace(data)) > 0 && !isDone && json.Unmarshal(data, &event) != nil {
			return errors.New("invalid upstream stream event")
		}
		if eventName == "error" || eventName == "response.failed" || event.Type == "error" || event.Type == "response.failed" || len(event.Error) > 0 && !bytes.Equal(event.Error, []byte("null")) {
			return errors.New("upstream stream failed")
		}
		if endpoint == translate.EndpointChatCompletions {
			for _, choice := range event.Choices {
				if choice.Index < 0 {
					return errors.New("invalid upstream stream choice index")
				}
				if _, seen := finishedChoices[choice.Index]; !seen && len(finishedChoices) >= streamMaxChoices {
					return errors.New("upstream stream has too many choices")
				}
				finishedChoices[choice.Index] = finishedChoices[choice.Index] || choice.FinishReason != nil && strings.TrimSpace(*choice.FinishReason) != ""
			}
		}
		if isDone {
			if len(finishedChoices) == 0 {
				return errors.New("upstream stream ended without a finish reason")
			}
			for _, finished := range finishedChoices {
				if !finished {
					return errors.New("upstream stream ended without a finish reason")
				}
			}
			terminal = true
		}
		switch event.Type {
		case "response.completed", "response.incomplete":
			terminal = endpoint == translate.EndpointResponses
		case "message_stop":
			terminal = endpoint == translate.EndpointMessages
		}
		if terminal {
			closeUpstream()
		}
		frames, errTranslate := translate.StreamFromEndpoint(ctx, endpoint, destination, model, original, translated, frame, &state)
		if errTranslate != nil {
			return errTranslate
		}
		for _, output := range frames {
			if len(output) == 0 {
				continue
			}
			output, errTranslate = rewriteStreamModel(output, model)
			if errTranslate != nil {
				return errTranslate
			}
			chunks := [][]byte{output}
			if destination == "openai" {
				chunks, errTranslate = formatOpenAIStreamChunksForHost(output)
				if errTranslate != nil {
					return errTranslate
				}
				// Some translators return several SSE events in one output. Rewrite
				// each extracted JSON object as well so none retains a native ID.
				for i, chunk := range chunks {
					chunks[i], errTranslate = rewriteResponseModel(chunk, model)
					if errTranslate != nil {
						return errTranslate
					}
				}
			}
			for _, chunk := range chunks {
				if errEmit := s.host.Emit(ctx, outputID, chunk); errEmit != nil {
					return errEmit
				}
			}
		}
		return nil
	}

	lastActivity := time.Now()
	for {
		readCtx, cancel := context.WithDeadline(ctx, lastActivity.Add(idleTimeout))
		chunk, errRead := s.host.ReadStream(readCtx, upstream.ID)
		cancel()
		if errRead != nil {
			terminalErr = fmt.Errorf("read Copilot stream: %w", errRead)
			return
		}
		if chunk.Error != "" {
			terminalErr = errors.New("upstream stream transport failed")
			return
		}
		if len(chunk.Payload) > 0 {
			lastActivity = time.Now()
		}
		if decoder.Buffered()+len(chunk.Payload) > 8<<20 {
			terminalErr = errors.New("upstream stream event too large")
			return
		}
		for _, frame := range decoder.Feed(chunk.Payload) {
			if errEmit := emit(frame); errEmit != nil {
				terminalErr = fmt.Errorf("translate Copilot stream: %w", errEmit)
				return
			}
			if terminal {
				return
			}
		}
		if chunk.Done {
			if len(bytes.TrimSpace(decoder.Flush())) > 0 || !terminal {
				terminalErr = errors.New("upstream stream ended without a terminal event")
			}
			return
		}
	}
}

// streamEventData joins all data fields in one SSE event before parsing JSON.
// Inspecting each line separately misses multiline error and terminal events.
func streamEventData(frame []byte) (string, []byte) {
	var event string
	var data [][]byte
	for _, line := range bytes.Split(bytes.ReplaceAll(frame, []byte("\r\n"), []byte("\n")), []byte("\n")) {
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "event":
			event = string(value)
		case "data":
			data = append(data, value)
		}
	}
	return event, bytes.Join(data, []byte("\n"))
}

// CLIProxyAPI wraps each Chat Completions chunk in its own data: frame
// and appends [DONE]. Emit one compact JSON object per event, without either
// wrapper. Other client protocols still require complete SSE frames.
func formatOpenAIStreamChunksForHost(frame []byte) ([][]byte, error) {
	var chunks [][]byte
	for _, event := range bytes.Split(bytes.ReplaceAll(frame, []byte("\r\n"), []byte("\n")), []byte("\n\n")) {
		_, data := streamEventData(event)
		data = bytes.TrimSpace(data)
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		var compact bytes.Buffer
		if data[0] != '{' || json.Compact(&compact, data) != nil {
			return nil, errors.New("invalid Chat Completions stream event")
		}
		chunks = append(chunks, compact.Bytes())
	}
	return chunks, nil
}

func normalizeRequestFormat(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "responses", "openai-response", "openai-responses":
		return "openai-response"
	case "openai", "chat", "chat-completions":
		return "openai"
	case "claude", "anthropic":
		return "claude"
	default:
		return ""
	}
}

func (s *Service) HTTP(context.Context, HTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	return pluginapi.ExecutorHTTPResponse{}, statusError("unsupported_operation", "arbitrary HTTP forwarding is disabled", 501)
}

func copilotHeaders(token string, stream bool) http.Header {
	requestID, _ := randomIdentifier(16)
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	headers := http.Header{}
	headers.Set("Accept", accept)
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("Content-Type", "application/json")
	headers.Set("Copilot-Integration-Id", copilotIntegrationID)
	headers.Set("Editor-Plugin-Version", copilotPluginVersion)
	headers.Set("Editor-Version", copilotEditorVersion)
	headers.Set("OpenAI-Intent", "conversation-agent")
	headers.Set("User-Agent", copilotUserAgent)
	headers.Set("X-Agent-Task-Id", requestID)
	headers.Set("X-GitHub-Api-Version", copilotAPIVersion)
	headers.Set("X-Initiator", "user")
	headers.Set("X-Interaction-Type", "conversation-agent")
	headers.Set("X-Request-Id", requestID)
	return headers
}

func filterResponseHeaders(headers http.Header) http.Header {
	out := http.Header{}
	for key, values := range headers {
		switch strings.ToLower(key) {
		case "content-type", "cache-control", "retry-after", "x-github-request-id", "x-request-id":
			out[key] = append([]string(nil), values...)
		}
	}
	return out
}

// Model discovery has a different intent from billed inference.
func modelHeaders(token string) http.Header {
	h := copilotHeaders(token, false)
	h.Set("OpenAI-Intent", "model-access")
	h.Set("X-Interaction-Type", "model-access")
	h.Del("Content-Type")
	h.Del("X-Initiator")
	return h
}

// Only structurally identified tool continuations are marked as agent requests.
// Caller-supplied initiator headers are never forwarded or trusted.
func inferenceHeaders(token string, stream bool, body []byte) http.Header {
	h := copilotHeaders(token, stream)
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &req) != nil {
		return h
	}
	if len(req.Messages) > 0 {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "tool" {
			h.Set("X-Initiator", "agent")
		} else if last.Role == "user" {
			var content []struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(last.Content, &content) == nil && len(content) > 0 {
				onlyTools := true
				for _, block := range content {
					if block.Type != "tool_result" {
						onlyTools = false
					}
				}
				if onlyTools {
					h.Set("X-Initiator", "agent")
				}
			}
		}
	}
	var input []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(req.Input, &input) == nil && len(input) > 0 && input[len(input)-1].Type == "function_call_output" {
		h.Set("X-Initiator", "agent")
	}
	if bytes.Contains(body, []byte(`"input_image"`)) || bytes.Contains(body, []byte(`"image_url"`)) || bytes.Contains(body, []byte(`"type":"image"`)) {
		h.Set("Copilot-Vision-Request", "true")
	}
	return h
}
