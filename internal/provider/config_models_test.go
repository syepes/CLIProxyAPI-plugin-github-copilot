package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"cliproxyapi-github-copilot/internal/transport"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestModelConfigDefaultsAndNormalization(t *testing.T) {
	for _, tc := range []struct{ raw, prefix string }{
		{"", "copilot"}, {"model_prefix: ''\nmodels: []", "copilot"},
		{"model_prefix: ' / '", "copilot"}, {"model_prefix: ' team/// '", "team"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			cfg, err := ParseConfig([]byte(tc.raw))
			if err != nil || cfg.ModelPrefix != tc.prefix {
				t.Fatalf("config: %+v, %v", cfg, err)
			}
			for _, native := range []string{"model", "vendor/model", "copilot/model"} {
				public := cfg.exposedModelID(native)
				got, err := cfg.upstreamModelID(public)
				if err != nil || got != native {
					t.Fatalf("round trip %q: %q %v", public, got, err)
				}
			}
			for _, invalid := range []string{"model", tc.prefix + "/", "other/model"} {
				if _, err := cfg.upstreamModelID(invalid); err == nil {
					t.Fatalf("accepted %q", invalid)
				}
			}
		})
	}
}

func TestModelAllowlistAliases(t *testing.T) {
	cfg, err := ParseConfig([]byte(`model_prefix: team
models:
 - name: ' available '
 - name: available
   alias: ' friendly '
 - name: unavailable
   alias: missing
`))
	if err != nil {
		t.Fatal(err)
	}
	infos := modelInfos([]upstreamModel{{ID: "available"}, {ID: "unlisted"}}, cfg)
	var ids []string
	for _, info := range infos {
		ids = append(ids, info.ID)
		if info.Name != info.ID {
			t.Fatalf("alternate name mismatch: %+v", info)
		}
		native, err := cfg.upstreamModelID(info.ID)
		if err != nil || native != "available" {
			t.Fatalf("alias mapping: %q %v", native, err)
		}
	}
	if !reflect.DeepEqual(ids, []string{"friendly", "team/available"}) {
		t.Fatalf("exposed IDs: %v", ids)
	}
	for _, invalid := range []string{"available", "team/unlisted", "copilot/available", "team/friendly"} {
		if _, err := cfg.upstreamModelID(invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestInvalidModelAllowlist(t *testing.T) {
	for _, raw := range []string{
		"models: [model]", "models: [{alias: test}]", "models: [{name: ' '}]",
		"models: [{name: a, alias: same}, {name: b, alias: same}]",
		"models: [{name: a}, {name: b, alias: copilot/a}]",
	} {
		if _, err := ParseConfig([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestModelConfigCopyAndRejectedRequests(t *testing.T) {
	s := newTestService(t, &mockHost{do: func(context.Context, string, transport.Request) (transport.Response, error) {
		t.Fatal("unlisted model reached upstream")
		return transport.Response{}, nil
	}})
	if err := s.Configure([]byte("models: [{name: allowed, alias: friendly}]")); err != nil {
		t.Fatal(err)
	}
	cfg := s.Config()
	cfg.Models[0].Alias = "mutated"
	if s.Config().Models[0].Alias != "friendly" {
		t.Fatal("Config exposes mutable model slice")
	}
	req := ExecuteRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "copilot/unlisted", SourceFormat: "openai-response"}, StreamID: "test"}
	if _, err := s.Execute(context.Background(), req); err == nil {
		t.Fatal("unlisted execute accepted")
	}
	if _, err := s.ExecuteStream(context.Background(), req); err == nil {
		t.Fatal("unlisted stream accepted")
	}
	if _, err := s.CountTokensChecked(context.Background(), req); err == nil {
		t.Fatal("unlisted token count accepted")
	}
}

func TestConfiguredAliasCannotBypassAccountCatalog(t *testing.T) {
	for _, tc := range []struct{ name, native, extra string }{
		{"missing", "missing", ""},
		{"excluded", "available", "models_excluded: [available]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestService(t, &mockHost{do: func(_ context.Context, _ string, r transport.Request) (transport.Response, error) {
				if strings.Contains(r.URL, "/copilot_internal/") {
					return tokenReply(time.Now(), "test-token"), nil
				}
				if strings.HasSuffix(r.URL, "/models") {
					return reply(catalog("available")), nil
				}
				t.Fatal("ineligible configured model reached upstream inference")
				return transport.Response{}, nil
			}})
			if err := s.Configure([]byte("models: [{name: " + tc.native + ", alias: friendly}]\n" + tc.extra)); err != nil {
				t.Fatal(err)
			}
			req := ExecuteRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "friendly", SourceFormat: "claude", AuthID: "test", StorageJSON: []byte(`{"type":"copilot","github_access_token":"test"}`), Payload: []byte(`{"model":"friendly","messages":[]}`)}, StreamID: "test"}
			if _, err := s.Execute(context.Background(), req); err == nil {
				t.Fatal("ineligible alias executed")
			}
			if _, err := s.ExecuteStream(context.Background(), req); err == nil {
				t.Fatal("ineligible alias streamed")
			}
			if _, err := s.CountTokensChecked(context.Background(), req); err == nil {
				t.Fatal("ineligible alias counted")
			}
		})
	}
}

func TestModelsExcludedConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte("models_excluded: [' Claude- ', claude-, '', GPT-]"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.ModelsExcluded, []string{"claude-", "gpt-"}) {
		t.Fatalf("normalized exclusions: %v", cfg.ModelsExcluded)
	}
	if _, err := ParseConfig([]byte("models_excluded: invalid")); err == nil {
		t.Fatal("accepted non-array exclusions")
	}
}

func TestAllowRawModelNames(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowRawModelNames = true
	aliases := cfg.modelAliases("gpt-4o")
	if !reflect.DeepEqual(aliases, []string{"copilot/gpt-4o", "gpt-4o"}) {
		t.Fatalf("expected raw and prefixed aliases, got %v", aliases)
	}
	native, err := cfg.upstreamModelID("gpt-4o")
	if err != nil || native != "gpt-4o" {
		t.Fatalf("expected raw model id resolution, got %q %v", native, err)
	}
}
