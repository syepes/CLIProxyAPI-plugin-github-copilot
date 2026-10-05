// Package integration loads the actual shared library in an isolated CLIProxyAPI process.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativePlugin(t *testing.T) {
	binary, library := os.Getenv("CPA_BINARY"), os.Getenv("CPA_PLUGIN_PATH")
	if binary == "" || library == "" {
		if os.Getenv("CPA_REQUIRE_NATIVE") == "1" {
			t.Fatal("native integration is required: set CPA_BINARY and CPA_PLUGIN_PATH")
		}
		t.Skip("set CPA_BINARY and CPA_PLUGIN_PATH")
	}
	id := os.Getenv("CPA_PLUGIN_ID")
	if id == "" {
		id = "github-copilot"
	}
	var disabled, outage atomic.Bool
	var calls atomic.Int64
	var grants atomic.Int64
	var rejectOnce atomic.Bool
	slowStarted := make(chan struct{}, 1)
	slowCanceled := make(chan struct{}, 1)
	terminalCanceled := make(chan struct{}, 1)
	var toolResult atomic.Bool
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/device/code":
			fmt.Fprintf(w, `{"device_code":"mock-device","user_code":"ABCD-EFGH","verification_uri":%q,"expires_in":900,"interval":5}`, upstream.URL+"/login/device")
		case "/login/oauth/access_token":
			fmt.Fprint(w, `{"access_token":"mock-github-token-new","token_type":"bearer","scope":"read:user"}`)
		case "/user":
			fmt.Fprint(w, `{"login":"new-account","id":123}`)
		case "/copilot_internal/v2/token":
			grants.Add(1)
			fmt.Fprintf(w, `{"token":"mock-copilot-token","expires_at":%d,"endpoints":{"api":%q}}`, time.Now().Add(time.Hour).Unix(), upstream.URL)
		case "/models":
			if outage.Load() {
				w.WriteHeader(503)
				fmt.Fprint(w, `{"error":"secret-must-not-leak"}`)
				return
			}
			state := "enabled"
			if disabled.Load() {
				state = "disabled"
			}
			fmt.Fprintf(w, `{"data":[{"id":"responses-only","model_picker_enabled":true,"supported_endpoints":["/responses"],"capabilities":{"type":"chat","supports":{"streaming":true,"tool_calls":true}}},{"id":"account-model","model_picker_enabled":true,"policy":{"state":%q},"supported_endpoints":["/responses","/chat/completions","/v1/messages"],"capabilities":{"type":"chat","supports":{"streaming":true,"tool_calls":true}}},{"id":"disabled-model","model_picker_enabled":true,"policy":{"state":"disabled"},"supported_endpoints":["/responses"],"capabilities":{"type":"chat"}},{"id":"hidden-model","model_picker_enabled":false,"supported_endpoints":["/responses"],"capabilities":{"type":"chat"}}]}`, state)
		case "/responses", "/chat/completions", "/v1/messages":
			calls.Add(1)
			if rejectOnce.CompareAndSwap(true, false) {
				w.WriteHeader(401)
				return
			}
			raw, _ := io.ReadAll(r.Body)
			if bytes.Contains(raw, []byte("wait-for-cancel")) {
				select {
				case slowStarted <- struct{}{}:
				default:
				}
				<-r.Context().Done()
				select {
				case slowCanceled <- struct{}{}:
				default:
				}
				return
			}
			if bytes.Contains(raw, []byte("function_call_output")) {
				toolResult.Store(true)
			}
			if bytes.Contains(raw, []byte("force-429")) {
				w.WriteHeader(429)
				fmt.Fprint(w, `{"error":"secret-must-not-leak"}`)
				return
			}
			var req map[string]any
			_ = json.Unmarshal(raw, &req)
			if req["model"] != "account-model" && req["model"] != "responses-only" {
				t.Errorf("client namespace leaked to GitHub: %v", req["model"])
				w.WriteHeader(400)
				return
			}
			if r.URL.Path == "/v1/messages" && req["context_management"] != nil {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"context management not supported"}`)
				return
			}
			if req["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				if bytes.Contains(raw, []byte("stream-error-body")) {
					fmt.Fprint(w, "event: error\ndata: {\"message\":\"secret-must-not-leak\"}\n\n")
					return
				}
				if bytes.Contains(raw, []byte("truncate-stream")) {
					fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
					return
				}
				var body string
				switch r.URL.Path {
				case "/chat/completions":
					body = "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"model\":\"account-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"model\":\"account-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
				case "/v1/messages":
					body = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"account-model\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				default:
					body = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"OK\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
				}
				for i := 0; i < len(body); i += 7 {
					_, _ = io.WriteString(w, body[i:min(i+7, len(body))])
					w.(http.Flusher).Flush()
				}
				if bytes.Contains(raw, []byte("hold-after-terminal")) {
					select {
					case <-r.Context().Done():
						terminalCanceled <- struct{}{}
					case <-time.After(5 * time.Second):
						t.Error("upstream remained open after terminal event")
					}
				}
				return
			}
			switch r.URL.Path {
			case "/chat/completions":
				fmt.Fprint(w, `{"id":"chat_1","object":"chat.completion","model":"account-model","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
			case "/v1/messages":
				fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","model":"account-model","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			default:
				if req["tools"] != nil {
					fmt.Fprint(w, `{"id":"resp_tool","object":"response","status":"completed","model":"account-model","output":[{"type":"function_call","call_id":"call_1","name":"echo","arguments":"{\"text\":\"OK\"}"}]}`)
					return
				}
				fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","model":"account-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}]}`)
			}
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(upstream.Close)
	temp := t.TempDir()
	authdir := filepath.Join(temp, "auth")
	plugins := filepath.Join(temp, "plugins", runtime.GOOS, runtime.GOARCH)
	for _, dir := range []string{authdir, plugins} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	authPath := filepath.Join(authdir, "copilot-test.json")
	write := func(path string, b []byte) {
		t.Helper()
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	originalAuth := []byte(`{"type":"copilot","github_access_token":"mock-github-token","github_login":"test","note":"preserve-me","custom":{"nested":true}}`)
	write(authPath, originalAuth)
	ext := ".so"
	if runtime.GOOS == "darwin" {
		ext = ".dylib"
	} else if runtime.GOOS == "windows" {
		ext = ".dll"
	}
	initialLibrary := library
	if old := os.Getenv("CPA_UPGRADE_FROM"); old != "" {
		initialLibrary = old
	}
	b, err := os.ReadFile(initialLibrary)
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(plugins, id+ext), b)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	cfg := map[string]any{"host": "127.0.0.1", "port": port, "auth-dir": authdir, "api-keys": []string{"client-test-key"}, "remote-management": map[string]any{"secret-key": "management-test-key", "disable-control-panel": true, "disable-auto-update-panel": true}, "request-retry": 0, "max-retry-interval": 0, "commercial-mode": true, "disable-cooling": true, "plugins": map[string]any{"enabled": true, "dir": filepath.Join(temp, "plugins"), "configs": map[string]any{id: map[string]any{"enabled": true, "github_base_url": upstream.URL, "github_api_url": upstream.URL, "copilot_api_url": upstream.URL, "allow_insecure_base_urls": true, "model_cache_ttl_seconds": 30, "model_picker_required": true}}}}
	raw, _ := json.Marshal(cfg)
	cfgPath := filepath.Join(temp, "config.json")
	write(cfgPath, raw)
	logPath := filepath.Join(temp, "host.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--config", cfgPath, "--local-model")
	cmd.Dir = temp
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + temp, "TMPDIR=" + os.TempDir()}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
		_ = log.Close()
		if t.Failed() {
			b, _ := os.ReadFile(logPath)
			lines := []string{}
			for _, line := range strings.Split(string(b), "\n") {
				if !strings.Contains(line, `GET     "/v1/models"`) {
					lines = append(lines, line)
				}
			}
			if len(lines) > 120 {
				lines = append(lines[:50], lines[len(lines)-70:]...)
			}
			t.Logf("host log: %s", strings.Join(lines, "\n"))
		}
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 15 * time.Second}
	request := func(method, path, key string, body any) (int, []byte) {
		t.Helper()
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		r, _ := http.NewRequest(method, base+path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err = io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, b
	}
	await := func(t *testing.T, f func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !f() {
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for host state")
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	await(t, func() bool {
		r, _ := http.NewRequest("GET", base+"/v1/models", nil)
		r.Header.Set("Authorization", "Bearer client-test-key")
		resp, err := client.Do(r)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return bytes.Contains(b, []byte("account-model"))
	})
	if os.Getenv("CPA_UPGRADE_FROM") != "" {
		t.Run("upgrade replaces bare model registration", func(t *testing.T) {
			_, before := request("GET", "/v1/models", "client-test-key", nil)
			if !bytes.Contains(before, []byte(`"id":"account-model"`)) {
				t.Fatalf("upgrade fixture is not the old plugin: %s", before)
			}
			replacement, err := os.ReadFile(library)
			if err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(plugins, id+"-v0.1.1"+ext), replacement)
			cfg["plugins"].(map[string]any)["configs"].(map[string]any)[id].(map[string]any)["store"] = map[string]any{"version": "0.1.1"}
			next, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			write(cfgPath, next)
			await(t, func() bool {
				_, body := request("GET", "/v1/models", "client-test-key", nil)
				return bytes.Contains(body, []byte(`"id":"copilot/account-model"`)) && !bytes.Contains(body, []byte(`"id":"account-model"`))
			})
		})
	}

	t.Run("edit config exposes model options", func(t *testing.T) {
		code, body := request("GET", "/v0/management/plugins", "management-test-key", nil)
		var listing struct {
			Plugins []struct {
				ID     string `json:"id"`
				Fields []struct {
					Name        string `json:"name"`
					Type        string `json:"type"`
					Description string `json:"description"`
				} `json:"config_fields"`
			} `json:"plugins"`
		}
		if err := json.Unmarshal(body, &listing); err != nil || code != 200 {
			t.Fatalf("plugin metadata: %d %s", code, body)
		}
		for _, plugin := range listing.Plugins {
			if plugin.ID != id {
				continue
			}
			prefix, models, excluded := false, false, false
			for _, field := range plugin.Fields {
				if field.Name == "model_prefix" && field.Type == "string" && strings.Contains(field.Description, "copilot") {
					prefix = true
				}
				if field.Name == "models" && field.Type == "array" {
					models = true
				}
				if field.Name == "models_excluded" && field.Type == "array" {
					excluded = true
				}
				if field.Name == "excluded_model_prefixes" {
					t.Fatal("obsolete model exclusion configuration exposed")
				}
			}
			if prefix && models && excluded {
				return
			}
		}
		t.Fatal("Edit config metadata lacks model_prefix with documented copilot default or models/models_excluded arrays")
	})

	t.Run("eligible models only", func(t *testing.T) {
		_, b := request("GET", "/v1/models", "client-test-key", nil)
		if bytes.Contains(b, []byte("disabled-model")) || bytes.Contains(b, []byte("hidden-model")) {
			t.Fatalf("ineligible models exposed: %s", b)
		}
	})
	t.Run("copilot model namespace", func(t *testing.T) {
		code, b := request("GET", "/v1/models", "client-test-key", nil)
		var listing struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if code != 200 || json.Unmarshal(b, &listing) != nil {
			t.Fatalf("model listing: %d %s", code, b)
		}
		found := false
		for _, model := range listing.Data {
			if !strings.HasPrefix(model.ID, "copilot/") {
				t.Errorf("model exposed without copilot namespace: %s", model.ID)
			}
			if model.ID == "copilot/account-model" {
				found = true
			}
		}
		if !found {
			t.Errorf("namespaced account model missing: %s", b)
		}
		code, b = request("GET", "/v1beta/models", "client-test-key", nil)
		var alternate struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if code != 200 || json.Unmarshal(b, &alternate) != nil || len(alternate.Models) == 0 {
			t.Fatalf("alternate registry listing: %d %s", code, b)
		}
		for _, model := range alternate.Models {
			if !strings.HasPrefix(strings.TrimPrefix(model.Name, "models/"), "copilot/") {
				t.Errorf("unprefixed registry name: %s", model.Name)
			}
		}
	})

	if os.Getenv("CPA_REGRESSION_ONLY") != "" {
		return
	}
	t.Run("native protocols", func(t *testing.T) {
		for _, route := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			for _, stream := range []bool{false, true} {
				body := map[string]any{"model": "copilot/account-model", "stream": stream, "max_tokens": 32, "messages": []any{map[string]string{"role": "user", "content": "OK"}}}
				if route == "/v1/responses" {
					delete(body, "messages")
					body["input"] = "OK"
					body["previous_response_id"] = "resp_prev"
				}
				code, b := request("POST", route, "client-test-key", body)
				if bytes.Contains(b, []byte(`"model":"account-model"`)) || bytes.Contains(b, []byte(`"model":"responses-only"`)) {
					t.Fatalf("unprefixed model returned to client: %s", b)
				}
				if code != 200 || !bytes.Contains(b, []byte("OK")) {
					t.Fatalf("%s stream=%v: %d %s", route, stream, code, b)
				}
				if stream {
					assertStreamFraming(t, route, b)
				}
			}
		}
	})
	t.Run("Claude Code keep-all context management", func(t *testing.T) {
		for _, stream := range []bool{false, true} {
			body := map[string]any{
				"model":              "copilot/account-model",
				"stream":             stream,
				"max_tokens":         64_000,
				"messages":           []any{map[string]string{"role": "user", "content": "OK"}},
				"thinking":           map[string]string{"type": "adaptive", "display": "omitted"},
				"output_config":      map[string]string{"effort": "high"},
				"context_management": map[string]any{"edits": []any{map[string]string{"type": "clear_thinking_20251015", "keep": "all"}}},
			}
			code, b := request("POST", "/v1/messages", "client-test-key", body)
			if code != 200 || !bytes.Contains(b, []byte("OK")) {
				t.Fatalf("keep-all stream=%v: %d %s", stream, code, b)
			}
			body["context_management"] = map[string]any{"edits": []any{map[string]any{"type": "clear_thinking_20251015", "keep": 1}}}
			code, b = request("POST", "/v1/messages", "client-test-key", body)
			if code != 400 {
				t.Fatalf("meaningful edit stream=%v was silently removed: %d %s", stream, code, b)
			}
		}
	})
	t.Run("cross protocol responses-only model", func(t *testing.T) {
		for _, route := range []string{"/v1/chat/completions", "/v1/messages"} {
			for _, stream := range []bool{false, true} {
				code, b := request("POST", route, "client-test-key", map[string]any{"model": "copilot/responses-only", "stream": stream, "max_tokens": 32, "messages": []any{map[string]string{"role": "user", "content": "OK"}}})
				if bytes.Contains(b, []byte(`"model":"account-model"`)) || bytes.Contains(b, []byte(`"model":"responses-only"`)) {
					t.Fatalf("unprefixed model returned to client: %s", b)
				}
				if code != 200 || !bytes.Contains(b, []byte("OK")) {
					t.Fatalf("cross protocol %s stream=%v: %d %s", route, stream, code, b)
				}
				if stream {
					assertStreamFraming(t, route, b)
				}
			}
		}
	})

	t.Run("bare model names are not Copilot routes", func(t *testing.T) {
		before := calls.Load()
		for _, route := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			code, _ := request("POST", route, "client-test-key", map[string]any{"model": "account-model", "input": "OK", "max_tokens": 32, "messages": []any{map[string]string{"role": "user", "content": "OK"}}})
			if code < 400 || calls.Load() != before {
				t.Fatalf("bare model accepted on %s: %d", route, code)
			}
		}
	})
	t.Run("namespaced token count", func(t *testing.T) {
		code, b := request("POST", "/v1/messages/count_tokens", "client-test-key", map[string]any{"model": "copilot/account-model", "messages": []any{map[string]string{"role": "user", "content": "hello world"}}})
		var count struct {
			InputTokens int `json:"input_tokens"`
		}
		_ = json.Unmarshal(b, &count)
		if code != 200 || count.InputTokens <= 0 {
			t.Fatalf("token count: %d %s", code, b)
		}
	})

	t.Run("terminal events close a connected upstream", func(t *testing.T) {
		for _, route := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			body := map[string]any{"model": "copilot/account-model", "input": "hold-after-terminal", "stream": true, "max_tokens": 32, "messages": []any{map[string]string{"role": "user", "content": "hold-after-terminal"}}}
			started := time.Now()
			code, raw := request("POST", route, "client-test-key", body)
			if code != 200 || time.Since(started) > 3*time.Second {
				t.Fatalf("terminal did not complete promptly on %s: %d %s", route, code, raw)
			}
			assertStreamFraming(t, route, raw)
			select {
			case <-terminalCanceled:
			case <-time.After(time.Second):
				t.Fatalf("upstream not canceled after terminal on %s", route)
			}
		}
	})
	t.Run("safe errors", func(t *testing.T) {
		code, b := request("POST", "/v1/responses", "client-test-key", map[string]any{"model": "copilot/account-model", "input": "force-429"})
		if code != 429 || bytes.Contains(b, []byte("secret-must-not-leak")) {
			t.Fatalf("unsafe status: %d %s", code, b)
		}
		_, b = request("POST", "/v1/responses", "client-test-key", map[string]any{"model": "copilot/account-model", "input": "truncate-stream", "stream": true})
		if !bytes.Contains(b, []byte("error")) {
			t.Fatalf("truncation accepted: %s", b)
		}
		for _, route := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			body := map[string]any{"model": "copilot/account-model", "input": "stream-error-body", "stream": true, "max_tokens": 32, "messages": []any{map[string]string{"role": "user", "content": "stream-error-body"}}}
			_, b = request("POST", route, "client-test-key", body)
			if !bytes.Contains(b, []byte("error")) || bytes.Contains(b, []byte("secret-must-not-leak")) {
				t.Fatalf("unsafe stream error %s: %s", route, b)
			}
		}
	})
	t.Run("management protection", func(t *testing.T) {
		code, _ := request("GET", "/v0/management/plugins/github-copilot/status", "", nil)
		if code != 401 && code != 403 {
			t.Fatalf("unprotected status: %d", code)
		}
		code, b := request("GET", "/v0/management/plugins/github-copilot/status", "management-test-key", nil)
		if code != 200 || bytes.Contains(b, []byte("mock-github-token")) || bytes.Contains(b, []byte("mock-copilot-token")) {
			t.Fatalf("unsafe status %d %s", code, b)
		}
	})
	refresh := func(t *testing.T) {
		t.Helper()
		code, b := request("POST", "/v0/management/plugins/github-copilot/refresh", "management-test-key", map[string]any{})
		if code != 200 {
			t.Fatalf("refresh: %d %s", code, b)
		}
		var status struct {
			Accounts []struct {
				Models []string `json:"models"`
			} `json:"accounts"`
		}
		if err := json.Unmarshal(b, &status); err != nil {
			t.Fatal(err)
		}
		for _, account := range status.Accounts {
			for _, model := range account.Models {
				if !strings.HasPrefix(model, "copilot/") {
					t.Fatalf("dashboard exposed bare model: %s", model)
				}
			}
		}
	}
	t.Run("revoked token retries once", func(t *testing.T) {
		before := grants.Load()
		rejectOnce.Store(true)
		code, b := request("POST", "/v1/responses", "client-test-key", map[string]any{"model": "copilot/account-model", "input": "OK"})
		if code != 200 || !bytes.Contains(b, []byte("OK")) || grants.Load() != before+1 {
			t.Fatalf("401 recovery failed: %d %s, grants=%d", code, b, grants.Load()-before)
		}
	})
	t.Run("function call round trip", func(t *testing.T) {
		code, b := request("POST", "/v1/responses", "client-test-key", map[string]any{"model": "copilot/account-model", "input": "use echo", "tools": []any{map[string]any{"type": "function", "name": "echo", "parameters": map[string]string{"type": "object"}}}})
		if code != 200 || !bytes.Contains(b, []byte("call_1")) {
			t.Fatalf("tool call failed %d %s", code, b)
		}
		code, b = request("POST", "/v1/responses", "client-test-key", map[string]any{"model": "copilot/account-model", "previous_response_id": "resp_tool", "input": []any{map[string]string{"type": "function_call_output", "call_id": "call_1", "output": "OK"}}})
		if code != 200 || !toolResult.Load() {
			t.Fatalf("tool result lost %d %s", code, b)
		}
	})
	t.Run("cancel before upstream headers", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/responses", strings.NewReader(`{"model":"copilot/account-model","input":"wait-for-cancel","stream":true}`))
		r.Header.Set("Authorization", "Bearer client-test-key")
		r.Header.Set("Content-Type", "application/json")
		done := make(chan struct{})
		go func() {
			defer close(done)
			resp, _ := client.Do(r)
			if resp != nil {
				resp.Body.Close()
			}
		}()
		select {
		case <-slowStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream never received request")
		}
		cancel()
		select {
		case <-slowCanceled:
		case <-time.After(5 * time.Second):
			t.Fatal("upstream not canceled")
		}
		<-done
	})

	localCatalogHas := func(t *testing.T, model string) bool {
		t.Helper()
		code, body := request("GET", "/v0/management/plugins/"+id+"/status", "management-test-key", nil)
		var status struct {
			Accounts []struct {
				Models    []string `json:"models"`
				SyncError string   `json:"sync_error"`
			} `json:"accounts"`
		}
		if code != 200 || json.Unmarshal(body, &status) != nil || len(status.Accounts) == 0 {
			t.Fatalf("account status unavailable: %d %s", code, body)
		}
		found := false
		for _, account := range status.Accounts {
			if account.SyncError == "" {
				t.Fatal("host registry limitation must be visible")
			}
			for _, current := range account.Models {
				if current == model {
					found = true
				}
			}
		}
		return found
	}
	t.Run("disable and reenable", func(t *testing.T) {
		// The host may normalize auth files during ordinary startup/inference.
		// Compare the file around the catalog-only operation under test.
		beforeAuth, err := os.ReadFile(authPath)
		if err != nil {
			t.Fatal(err)
		}
		disabled.Store(true)
		refresh(t)
		await(t, func() bool {
			return !localCatalogHas(t, "copilot/account-model")
		})
		before := calls.Load()
		code, _ := request("POST", "/v1/responses", "client-test-key", map[string]any{"model": "copilot/account-model", "input": "OK"})
		if code == 200 || calls.Load() != before {
			t.Fatal("disabled model reached upstream")
		}
		disabled.Store(false)
		refresh(t)
		await(t, func() bool {
			return localCatalogHas(t, "copilot/account-model")
		})
		raw, err := os.ReadFile(authPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw, beforeAuth) {
			t.Fatal("catalog refresh changed the host-owned credential")
		}
		if !bytes.Contains(raw, []byte("preserve-me")) || !bytes.Contains(raw, []byte(`"nested":true`)) {
			t.Fatal("credential metadata was lost")
		}
	})
	t.Run("discovery failure clears local eligibility", func(t *testing.T) {
		outage.Store(true)
		refresh(t)
		await(t, func() bool {
			return !localCatalogHas(t, "copilot/account-model")
		})
		before := calls.Load()
		code, _ := request("POST", "/v1/responses", "client-test-key", map[string]any{"model": "copilot/account-model", "input": "OK"})
		if code == 200 || calls.Load() != before {
			t.Fatal("unavailable catalog allowed inference")
		}
		outage.Store(false)
		refresh(t)
		await(t, func() bool {
			return localCatalogHas(t, "copilot/account-model")
		})
	})
	t.Run("GitHub device login", func(t *testing.T) {
		code, b := request("GET", "/v0/management/copilot-auth-url", "management-test-key", nil)
		var started struct {
			State string `json:"state"`
			URL   string `json:"url"`
		}
		_ = json.Unmarshal(b, &started)
		if code != 200 || started.State == "" || started.URL == "" {
			t.Fatalf("login start: %d %s", code, b)
		}
		await(t, func() bool {
			_, b := request("GET", "/v0/management/get-auth-status?state="+started.State, "management-test-key", nil)
			return bytes.Contains(b, []byte(`"status":"ok"`))
		})
		files, err := filepath.Glob(filepath.Join(authdir, "copilot-new-account*.json"))
		if err != nil || len(files) != 1 {
			t.Fatalf("login file missing: %v", err)
		}
		raw, err := os.ReadFile(files[0])
		var stored struct {
			Token  string `json:"github_access_token"`
			Issuer string `json:"github_base_url"`
		}
		if err != nil || json.Unmarshal(raw, &stored) != nil || stored.Token != "mock-github-token-new" || stored.Issuer != upstream.URL {
			t.Fatalf("issuer-scoped login not persisted: %v", err)
		}
	})

	t.Run("background refresh blocks disabled model locally", func(t *testing.T) {
		disabled.Store(true)
		deadline := time.Now().Add(45 * time.Second)
		for {
			if !localCatalogHas(t, "copilot/account-model") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("background catalog expiry did not remove local eligibility")
			}
			time.Sleep(200 * time.Millisecond)
		}
		before := calls.Load()
		code, _ := request("POST", "/v1/responses", "client-test-key", map[string]any{"model": "copilot/account-model", "input": "OK"})
		if code == 200 || calls.Load() != before {
			t.Fatal("background-disabled model reached upstream")
		}
		disabled.Store(false)
		refresh(t)
	})

	t.Run("unknown models rejected", func(t *testing.T) {
		before := calls.Load()
		code, _ := request("POST", "/v1/responses", "client-test-key", map[string]any{"model": "copilot/gpt-5.6-sol", "input": "OK"})
		if code == 200 || calls.Load() != before {
			t.Fatal("unknown model reached upstream")
		}
	})

	t.Run("edit config updates model routing", func(t *testing.T) {
		patch := func(config map[string]any, expected ...string) {
			t.Helper()
			code, body := request("PATCH", "/v0/management/plugins/"+id+"/config", "management-test-key", config)
			if code != 200 {
				t.Fatalf("config update: %d %s", code, body)
			}
			await(t, func() bool {
				code, body := request("GET", "/v1/models", "client-test-key", nil)
				var listing struct {
					Data []struct {
						ID string `json:"id"`
					} `json:"data"`
				}
				if code != 200 || json.Unmarshal(body, &listing) != nil || len(listing.Data) != len(expected) {
					return false
				}
				for _, want := range expected {
					found := false
					for _, model := range listing.Data {
						if model.ID == want {
							found = true
						}
					}
					if !found {
						return false
					}
				}
				return true
			})
		}
		patch(map[string]any{"model_prefix": "team"}, "team/account-model", "team/responses-only")
		patch(map[string]any{"models": []any{
			map[string]string{"name": "account-model"},
			map[string]string{"name": "account-model", "alias": "friendly"},
			map[string]string{"name": "disabled-model", "alias": "disabled"},
			map[string]string{"name": "unavailable", "alias": "missing"},
		}}, "team/account-model", "friendly")
		for _, public := range []string{"team/account-model", "friendly"} {
			for _, route := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
				for _, stream := range []bool{false, true} {
					body := map[string]any{"model": public, "stream": stream, "max_tokens": 32, "messages": []any{map[string]string{"role": "user", "content": "OK"}}}
					if route == "/v1/responses" {
						delete(body, "messages")
						body["input"] = "OK"
					}
					code, b := request("POST", route, "client-test-key", body)
					if code != 200 || !bytes.Contains(b, []byte("OK")) || bytes.Contains(b, []byte(`"model":"account-model"`)) {
						t.Fatalf("%s %s stream=%v: %d %s", public, route, stream, code, b)
					}
				}
			}
			code, b := request("POST", "/v1/messages/count_tokens", "client-test-key", map[string]any{"model": public, "messages": []any{map[string]string{"role": "user", "content": "OK"}}})
			if code != 200 || !bytes.Contains(b, []byte("input_tokens")) {
				t.Fatalf("alias token count: %d %s", code, b)
			}
		}
		// Dashboard snapshots are populated by the background reconciler after reload.
		await(t, func() bool {
			code, dashboard := request("GET", "/v0/management/plugins/"+id+"/status", "management-test-key", nil)
			return code == 200 && bytes.Contains(dashboard, []byte(`"friendly"`)) && !bytes.Contains(dashboard, []byte(`"team/responses-only"`))
		})
		before := calls.Load()
		for _, invalid := range []string{"copilot/account-model", "team/responses-only", "disabled", "missing"} {
			code, b := request("POST", "/v1/responses", "client-test-key", map[string]any{"model": invalid, "input": "OK"})
			if code == 200 || calls.Load() != before {
				t.Fatalf("rejected alias reached upstream: %s: %d %s", invalid, code, b)
			}
		}
		patch(map[string]any{"models_excluded": []string{"account-"}})
		patch(map[string]any{"model_prefix": "", "models": []any{}, "models_excluded": []string{}}, "copilot/account-model", "copilot/responses-only")
	})
}
