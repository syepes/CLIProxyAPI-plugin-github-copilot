package provider

import (
	"testing"

	"cliproxyapi-github-copilot/internal/translate"
)

func TestSelectEndpoint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		model     upstreamModel
		want      string
		wantError bool
	}{
		{
			name:  "responses preferred",
			model: upstreamModel{ID: "model-a", SupportedEndpoints: []string{"/chat/completions", "/responses"}},
			want:  translate.EndpointResponses,
		},
		{
			name:  "messages fallback",
			model: upstreamModel{ID: "model-b", SupportedEndpoints: []string{"messages"}},
			want:  translate.EndpointMessages,
		},
		{
			name:  "sol obeys catalog endpoints",
			model: upstreamModel{ID: "gpt-5.6-sol", SupportedEndpoints: []string{"/chat/completions"}},
			want:  translate.EndpointChatCompletions,
		},
		{
			name:      "terra without endpoints is unsupported",
			model:     upstreamModel{ID: "GPT-5.6-TERRA"},
			wantError: true,
		},
		{
			name:      "unsupported",
			model:     upstreamModel{ID: "embedding-model", SupportedEndpoints: []string{"/embeddings"}},
			wantError: true,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := selectEndpoint(test.model)
			if test.wantError {
				if err == nil {
					t.Fatal("expected an endpoint selection error")
				}
				return
			}
			if err != nil {
				t.Fatalf("select endpoint: %v", err)
			}
			if got != test.want {
				t.Fatalf("endpoint = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNormalizeModelsPreservesCatalogEndpoints(t *testing.T) {
	t.Parallel()

	models := normalizeModels([]upstreamModel{
		{
			ID:                 "gpt-5.6-sol",
			ModelPickerEnabled: true,
			SupportedEndpoints: []string{"/chat/completions"},
			Capabilities: modelCapabilities{Type: "chat",
				Supports: modelSupports{Streaming: true, ToolCalls: true, Vision: true},
				Limits:   modelLimits{MaxPromptTokens: 100, MaxOutputTokens: 20},
			},
		},
	}, DefaultConfig())
	if len(models) != 1 || contains(models[0].SupportedEndpoints, translate.EndpointResponses) {
		t.Fatalf("an unadvertised endpoint was invented: %#v", models)
	}
	info := modelInfos(models, DefaultConfig())[0]
	if !contains(info.SupportedGenerationMethods, translate.EndpointChatCompletions) {
		t.Fatalf("model metadata omits responses endpoint: %#v", info.SupportedGenerationMethods)
	}
	if !contains(info.SupportedInputModalities, "IMAGE") {
		t.Fatalf("model metadata omits image support: %#v", info.SupportedInputModalities)
	}
}

func TestNormalizeModelsDefaultsEndpointsForChat(t *testing.T) {
	t.Parallel()

	models := normalizeModels([]upstreamModel{
		{
			ID:           "gpt-4o",
			Capabilities: modelCapabilities{Type: "chat"},
		},
	}, DefaultConfig())
	if len(models) != 1 || len(models[0].SupportedEndpoints) != 1 || models[0].SupportedEndpoints[0] != translate.EndpointChatCompletions {
		t.Fatalf("expected /chat/completions default endpoint, got %#v", models)
	}
}

func TestFilterModelsExcludesConfiguredPrefixes(t *testing.T) {
	t.Parallel()

	models := filterModels([]upstreamModel{
		{ID: "gpt-5.6-sol"},
		{ID: "claude-sonnet-5"},
		{ID: "Claude-Haiku-4.5"},
	}, []string{"claude-"})
	if len(models) != 1 || models[0].ID != "gpt-5.6-sol" {
		t.Fatalf("filtered models = %#v", models)
	}
}

func TestNormalizeModelPrefixes(t *testing.T) {
	t.Parallel()

	got := normalizeModelPrefixes([]string{" Claude- ", "claude-", "", "GPT-"})
	if len(got) != 2 || got[0] != "claude-" || got[1] != "gpt-" {
		t.Fatalf("normalized prefixes = %#v", got)
	}
}
