package provider

import (
	"cliproxyapi-github-copilot/internal/transport"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

func TestCopilotHeadersUseRecognizedIntegration(t *testing.T) {
	headers := copilotHeaders("test-token", false)

	expected := map[string]string{
		"Copilot-Integration-Id": "vscode-chat",
		"Editor-Plugin-Version":  "copilot-chat/0.67.0",
		"Editor-Version":         "vscode/1.139.1",
		"OpenAI-Intent":          "conversation-agent",
		"User-Agent":             "GitHubCopilotChat/0.67.0",
		"X-GitHub-Api-Version":   "2026-08-01",
	}

	for name, want := range expected {
		if got := headers.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestCountTokensReturnsClaudeInputTokens(t *testing.T) {
	t.Parallel()

	resp, err := (&Service{}).CountTokens(ExecuteRequest{
		ExecutorRequest: pluginapi.ExecutorRequest{
			SourceFormat:    "claude",
			OriginalRequest: []byte(`{"model":"gpt-5.6-sol","system":"Be concise.","messages":[{"role":"user","content":"hello world"}]}`),
		},
	})
	if err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "input_tokens").Int(); got <= 0 {
		t.Fatalf("input_tokens = %d; response=%s", got, resp.Payload)
	}
}

func TestInferenceAndDiscoveryIntents(t *testing.T) {
	if h := modelHeaders("t"); h.Get("OpenAI-Intent") != "model-access" || h.Get("X-Initiator") != "" {
		t.Fatal("incorrect discovery intent")
	}
	for _, tc := range []struct{ body, initiator string }{
		{`{"input":"hello"}`, "user"},
		{`{"input":[{"type":"function_call_output","output":"OK"}]}`, "agent"},
		{`{"messages":[{"role":"tool","content":"OK"}]}`, "agent"},
		{`{"messages":[{"role":"user","content":[{"type":"tool_result"}]}]}`, "agent"},
		{`{"messages":[{"role":"user","content":[{"type":"tool_result"},{"type":"text","text":"continue"}]}]}`, "user"},
	} {
		if h := inferenceHeaders("t", false, []byte(tc.body)); h.Get("X-Initiator") != tc.initiator {
			t.Fatalf("incorrect initiator for %s", tc.body)
		}
	}
}

type failingStreamHost struct {
	mockHost
	read    bool
	message string
}

func (h *failingStreamHost) ReadStream(context.Context, string) (transport.StreamChunk, error) {
	if h.read {
		return transport.StreamChunk{Done: true}, nil
	}
	h.read = true
	return transport.StreamChunk{Payload: []byte("event: error\ndata: {\"error\":{\"message\":\"private-backend-details\"}}\n\n")}, nil
}
func (h *failingStreamHost) CloseOutput(_ context.Context, _ string, message string) {
	h.message = message
}
func TestStreamErrorsDoNotExposeBackendDetails(t *testing.T) {
	h := &failingStreamHost{}
	s := newTestService(t, h)
	s.pumpStream(context.Background(), "out", "/responses", "claude", "model", nil, nil, transport.Stream{ID: "in"}, streamIdleTimeout)
	if h.message == "" || strings.Contains(h.message, "private-backend-details") {
		t.Fatalf("unsafe stream error %q", h.message)
	}
}

func TestOpenAIStreamChunksForHost(t *testing.T) {
	for _, tc := range []struct {
		name, frame string
		want        []string
		bad         bool
	}{
		{"chat event", "data: {\"choices\":[]}\n\n", []string{`{"choices":[]}`}, false},
		{"CRLF and done", "data: {\"choices\":[]}\r\n\r\ndata: [DONE]\r\n\r\n", []string{`{"choices":[]}`}, false},
		{"multiple events", "data: {\"id\":1}\n\ndata: {\"id\":2}\n\n", []string{`{"id":1}`, `{"id":2}`}, false},
		{"multiline JSON", "event: chunk\ndata: {\"choices\":\ndata: []}\n\n", []string{`{"choices":[]}`}, false},
		{"keepalive", ": keepalive\n\ndata: [DONE]\n\n", nil, false},
		{"malformed payload", "data: private-backend-details\n\n", nil, true},
		{"non-object payload", "data: [1,2]\n\n", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks, err := formatOpenAIStreamChunksForHost([]byte(tc.frame))
			if (err != nil) != tc.bad {
				t.Fatalf("error = %v, want bad=%v", err, tc.bad)
			}
			if err != nil && strings.Contains(err.Error(), "private-backend-details") {
				t.Fatal("payload exposed in error")
			}
			if len(chunks) != len(tc.want) {
				t.Fatalf("chunks = %q, want %q", chunks, tc.want)
			}
			for i, want := range tc.want {
				if string(chunks[i]) != want {
					t.Errorf("chunk %d = %q, want %q", i, chunks[i], want)
				}
			}
		})
	}
}

type recordingStreamHost struct {
	mockHost
	chunks  []transport.StreamChunk
	outputs [][]byte
	message string
	closes  int
}

func (h *recordingStreamHost) ReadStream(context.Context, string) (transport.StreamChunk, error) {
	if len(h.chunks) == 0 {
		return transport.StreamChunk{Done: true}, nil
	}
	chunk := h.chunks[0]
	h.chunks = h.chunks[1:]
	return chunk, nil
}
func (h *recordingStreamHost) Emit(_ context.Context, _ string, b []byte) error {
	h.outputs = append(h.outputs, append([]byte(nil), b...))
	return nil
}
func (h *recordingStreamHost) CloseOutput(_ context.Context, _ string, message string) {
	h.message = message
}
func (h *recordingStreamHost) CloseStream(context.Context, string) error { h.closes++; return nil }

func TestPumpStreamChatHostFraming(t *testing.T) {
	h := &recordingStreamHost{chunks: []transport.StreamChunk{
		{Payload: []byte("data: {\"model\":\"native\",\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\r\n\r\ndata: [DO")},
		{Payload: []byte("NE]\n\n"), Done: true},
	}}
	s := newTestService(t, h)
	s.pumpStream(context.Background(), "out", "/chat/completions", "openai", "copilot/native", nil, nil, transport.Stream{ID: "in"}, streamIdleTimeout)
	if h.message != "" || h.closes != 1 || len(h.outputs) != 1 {
		t.Fatalf("message=%q closes=%d outputs=%q", h.message, h.closes, h.outputs)
	}
	if !json.Valid(h.outputs[0]) || gjson.GetBytes(h.outputs[0], "model").String() != "copilot/native" {
		t.Fatalf("invalid host JSON chunk: %s", h.outputs[0])
	}
}

func TestPumpStreamRejectsErrorsBeforeEmit(t *testing.T) {
	for _, destination := range []string{"openai", "openai-response", "claude"} {
		for _, frame := range []string{
			"event: error\ndata: {\"message\":\"private-backend-details\"}\n\n",
			"data: {\"error\":{\"message\":\"private-backend-details\"}}\n\n",
			"data: {\"type\":\ndata: \"response.failed\",\"message\":\"private-backend-details\"}\n\n",
		} {
			h := &recordingStreamHost{chunks: []transport.StreamChunk{{Payload: []byte(frame), Done: true}}}
			s := newTestService(t, h)
			s.pumpStream(context.Background(), "out", "/responses", destination, "copilot/model", nil, nil, transport.Stream{ID: "in"}, streamIdleTimeout)
			if len(h.outputs) != 0 || h.message == "" || strings.Contains(h.message, "private-backend-details") || h.closes != 1 {
				t.Fatalf("unsafe failure destination=%s outputs=%q message=%q closes=%d", destination, h.outputs, h.message, h.closes)
			}
		}
	}
}

func TestPumpStreamKeepsOtherProtocolFraming(t *testing.T) {
	for _, tc := range []struct{ endpoint, destination, frame string }{
		{"/responses", "openai-response", "event: response.completed\ndata: {\"type\":\ndata: \"response.completed\"}\n\n"},
		{"/v1/messages", "claude", "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
	} {
		h := &recordingStreamHost{chunks: []transport.StreamChunk{{Payload: []byte(tc.frame), Done: true}}}
		s := newTestService(t, h)
		s.pumpStream(context.Background(), "out", tc.endpoint, tc.destination, "copilot/model", nil, nil, transport.Stream{ID: "in"}, streamIdleTimeout)
		if h.message != "" || len(h.outputs) != 1 || string(h.outputs[0]) != tc.frame {
			t.Fatalf("framing changed: message=%q outputs=%q", h.message, h.outputs)
		}
	}
}
