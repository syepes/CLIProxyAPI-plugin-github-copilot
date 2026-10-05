package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"cliproxyapi-github-copilot/internal/transport"
)

type mockHost struct {
	do func(context.Context, string, transport.Request) (transport.Response, error)
}

func (h *mockHost) Do(ctx context.Context, id string, r transport.Request) (transport.Response, error) {
	return h.do(ctx, id, r)
}
func (h *mockHost) OpenStream(context.Context, string, transport.Request) (transport.Stream, error) {
	return transport.Stream{}, errors.New("not implemented")
}
func (h *mockHost) ReadStream(context.Context, string) (transport.StreamChunk, error) {
	return transport.StreamChunk{}, errors.New("not implemented")
}
func (h *mockHost) CloseStream(context.Context, string) error   { return nil }
func (h *mockHost) Emit(context.Context, string, []byte) error  { return nil }
func (h *mockHost) CloseOutput(context.Context, string, string) {}
func reply(body string) transport.Response {
	return transport.Response{StatusCode: 200, Body: []byte(body)}
}
func catalog(ids ...string) string {
	models := []map[string]any{}
	for _, id := range ids {
		models = append(models, map[string]any{"id": id, "model_picker_enabled": true, "capabilities": map[string]string{"type": "chat"}, "supported_endpoints": []string{"/responses"}})
	}
	b, _ := json.Marshal(map[string]any{"data": models})
	return string(b)
}
func tokenReply(now time.Time, token string) transport.Response {
	return reply(fmt.Sprintf(`{"token":%q,"expires_at":%d}`, token, now.Add(time.Hour).Unix()))
}
func newTestService(t *testing.T, h transport.Host) *Service {
	t.Helper()
	s := New(h)
	t.Cleanup(s.Shutdown)
	return s
}

func TestCatalogEligibility(t *testing.T) {
	cases := []struct {
		name, fragment string
		pickerRequired bool
		want           int
	}{
		{"enabled", `"model_picker_enabled":true,"policy":{"state":"enabled"}`, false, 1},
		{"legacy optional policy", `"model_picker_enabled":true`, false, 1},
		{"disabled", `"model_picker_enabled":true,"policy":{"state":"disabled"}`, false, 0},
		{"unknown policy", `"model_picker_enabled":true,"policy":{"state":"unknown"}`, false, 0},
		{"empty policy", `"model_picker_enabled":true,"policy":{}`, false, 0},
		{"hidden allowed by default", `"model_picker_enabled":false`, false, 1},
		{"hidden filtered when picker required", `"model_picker_enabled":false`, true, 0},
		{"missing picker allowed by default", `"preview":true`, false, 1},
		{"missing picker filtered when required", `"preview":true`, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var list modelListResponse
			if err := json.Unmarshal([]byte(`{"data":[{"id":"model",`+tc.fragment+`,"capabilities":{"type":"chat"},"supported_endpoints":["/responses"]}]}`), &list); err != nil {
				t.Fatal(err)
			}
			cfg := DefaultConfig()
			cfg.ModelPickerRequired = tc.pickerRequired
			if got := normalizeModels(list.Data, cfg); len(got) != tc.want {
				t.Fatalf("eligible=%d want %d", len(got), tc.want)
			}
		})
	}
}
func TestCatalogExpiryFailureAndAccountIsolation(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	var unavailable atomic.Bool
	var discovery atomic.Int64
	h := &mockHost{do: func(_ context.Context, _ string, r transport.Request) (transport.Response, error) {
		if strings.Contains(r.URL, "/copilot_internal/") {
			return tokenReply(time.Unix(clock.Load(), 0), r.Headers.Get("Authorization")), nil
		}
		discovery.Add(1)
		if unavailable.Load() {
			return transport.Response{StatusCode: 503}, nil
		}
		if strings.Contains(r.Headers.Get("Authorization"), "account-b") {
			return reply(catalog("b-model")), nil
		}
		return reply(catalog("a-model")), nil
	}}
	s := newTestService(t, h)
	s.now = func() time.Time { return time.Unix(clock.Load(), 0) }
	ctx := context.Background()
	a := authStorage{GitHubAccessToken: "account-a"}
	b := authStorage{GitHubAccessToken: "account-b"}
	models, _, err := s.models(ctx, "", "same-id", a, false)
	if err != nil || len(models) != 1 || models[0].ID != "a-model" {
		t.Fatalf("models: %v %v", models, err)
	}
	models, _, err = s.models(ctx, "", "same-id", b, false)
	if err != nil || models[0].ID != "b-model" {
		t.Fatalf("account cache leak: %v %v", models, err)
	}
	if _, _, err = s.endpointForModel(ctx, "", "same-id", b, "a-model"); err == nil {
		t.Fatal("cross-account model accepted")
	}
	unavailable.Store(true)
	clock.Add(61)
	if _, _, err = s.models(ctx, "", "same-id", a, false); err == nil {
		t.Fatal("stale catalog served")
	}
	before := discovery.Load()
	if _, _, err = s.models(ctx, "", "same-id", a, false); err == nil || discovery.Load() != before {
		t.Fatal("discovery failure backoff missing")
	}
	raw, _ := json.Marshal(map[string]any{"type": "copilot", "github_access_token": "account-a"})
	resp, err := s.ModelsForAuth(ctx, "", pluginapi.AuthModelRequest{AuthID: "same-id", StorageJSON: raw})
	if err != nil || len(resp.Models) != 0 {
		t.Fatal("failure must unregister host models with an empty success")
	}
	unavailable.Store(false)
	clock.Add(6)
	models, _, err = s.models(ctx, "", "same-id", a, false)
	if err != nil || len(models) != 1 {
		t.Fatal("catalog did not recover")
	}
	if _, _, err = s.endpointForModel(ctx, "", "same-id", a, "gpt-5.6-sol"); err == nil {
		t.Fatal("hard-coded catalog bypass")
	}
}
func TestTokenSingleFlightAndCanceledWaiter(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var grants atomic.Int64
	h := &mockHost{do: func(ctx context.Context, _ string, r transport.Request) (transport.Response, error) {
		if grants.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return tokenReply(time.Now(), "shared"), nil
		case <-ctx.Done():
			return transport.Response{}, ctx.Err()
		}
	}}
	s := newTestService(t, h)
	a := authStorage{GitHubAccessToken: "account"}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { _, err := s.copilotToken(ctx, "", "id", a); finished <- err }()
	<-started
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := s.copilotToken(context.Background(), "", "id", a)
			if err != nil || token.Token != "shared" {
				t.Errorf("shared grant failed: %v", err)
			}
		}()
	}
	close(release)
	wg.Wait()
	if grants.Load() != 1 {
		t.Fatalf("grants=%d", grants.Load())
	}
	s.invalidateToken("id", a, "old-token")
	if _, err := s.copilotToken(context.Background(), "", "id", a); err != nil || grants.Load() != 1 {
		t.Fatal("late rejection evicted newer token")
	}
}
func TestInvalidTokenExpiryAndEndpoint(t *testing.T) {
	for _, body := range []string{`{"token":"t"}`, `{"token":"t","expires_at":1}`, `{"token":"t","expires_at":9999999999,"endpoints":{"api":"https://evil.test"}}`, `{"token":"t","expires_at":9999999999,"endpoints":{"api":"https://user:password@api.githubcopilot.com"}}`} {
		t.Run(body, func(t *testing.T) {
			s := newTestService(t, &mockHost{do: func(context.Context, string, transport.Request) (transport.Response, error) { return reply(body), nil }})
			if _, err := s.copilotToken(context.Background(), "", "id", authStorage{GitHubAccessToken: "g"}); err == nil {
				t.Fatal("unsafe token accepted")
			}
		})
	}
}
func TestCredentialUnknownFieldsSurviveRefresh(t *testing.T) {
	raw := []byte(`{"type":"copilot","github_access_token":"old","github_login":"test","note":"keep","custom":{"nested":true},"github_copilot_catalog_revision":"rev"}`)
	storage, err := parseStorage(raw)
	if err != nil {
		t.Fatal(err)
	}
	storage.GitHubAccessToken = "new"
	data, err := authData(storage, "id", "file.json", "prefix", "", true, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	_ = json.Unmarshal(data.StorageJSON, &fields)
	if fields["note"] != "keep" || fields["custom"] == nil || fields["github_access_token"] != "new" || data.Metadata[CatalogRevisionKey] != "rev" || !data.Disabled {
		t.Fatalf("metadata not preserved: %v", fields)
	}
}
func TestConfigurationBounds(t *testing.T) {
	for _, raw := range []string{"model_cache_ttl_seconds: 120", "github_api_url: http://evil.test", "github_api_url: https://user:secret@api.github.com", "allow_insecure_base_urls: true\ngithub_api_url: http://evil.test"} {
		if _, err := ParseConfig([]byte(raw)); err == nil {
			t.Fatalf("unsafe config accepted: %s", raw)
		}
	}
	cfg, err := ParseConfig(nil)
	if err != nil || cfg.ModelCacheTTLSeconds != 60 {
		t.Fatal("incorrect defaults")
	}
}
func TestNoArbitraryHTTP(t *testing.T) {
	s := newTestService(t, &mockHost{do: func(context.Context, string, transport.Request) (transport.Response, error) {
		t.Fatal("unexpected upstream call")
		return transport.Response{}, nil
	}})
	_, err := s.HTTP(context.Background(), HTTPRequest{})
	var status *StatusError
	if !errors.As(err, &status) || status.HTTPStatus != http.StatusNotImplemented {
		t.Fatalf("HTTP forwarding enabled: %v", err)
	}
}
