package plugin

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type noHost struct{}

func (noHost) Call(string, any, any) error { return errors.New("offline") }
func dispatch(t *testing.T, p *Plugin, method string, request any) map[string]json.RawMessage {
	t.Helper()
	b, _ := json.Marshal(request)
	var out map[string]json.RawMessage
	if err := json.Unmarshal(p.Dispatch(method, b), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestLifecycle(t *testing.T) {
	p := New(noHost{})
	defer p.Close()
	out := dispatch(t, p, "plugin.register", lifecycleRequest{SchemaVersion: 5})
	if string(out["ok"]) != "false" {
		t.Fatal("old schema accepted")
	}
	req := lifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("enabled: true")}
	out = dispatch(t, p, "plugin.register", req)
	if string(out["ok"]) != "true" {
		t.Fatalf("register failed: %s", out["error"])
	}
	old := p.service
	dispatch(t, p, "plugin.reconfigure", req)
	if p.service != old {
		t.Fatal("unchanged reload discarded runtime")
	}
	req.ConfigYAML = []byte("model_cache_ttl_seconds: 9999")
	out = dispatch(t, p, "plugin.reconfigure", req)
	if string(out["ok"]) != "false" || p.service != old || old.Context().Err() != nil {
		t.Fatal("invalid reconfigure did not preserve the working service and report failure")
	}
	req.ConfigYAML = []byte("enabled: false")
	out = dispatch(t, p, "plugin.reconfigure", req)
	if string(out["ok"]) != "true" || p.service != nil || old.Context().Err() == nil {
		t.Fatal("valid disabled config did not stop the service")
	}
	out = dispatch(t, p, "model.static", nil)
	if !strings.Contains(string(out["result"]), `"Models":[]`) {
		t.Fatal("static fallback models exposed")
	}
}
func TestDashboardCSPAndPublicData(t *testing.T) {
	p := New(noHost{})
	defer p.Close()
	req := pluginapi.ManagementRequest{Method: "GET", Path: "/v0/resource/plugins/github-copilot/dashboard"}
	raw, _ := json.Marshal(req)
	resp, err := p.manage(raw)
	if err != nil || resp.StatusCode != 200 {
		t.Fatal("dashboard unavailable")
	}
	csp := resp.Headers.Get("Content-Security-Policy")
	if !strings.Contains(csp, "sha256-") || strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("unsafe CSP %s", csp)
	}
	if strings.Contains(string(resp.Body), "localStorage") || strings.Contains(string(resp.Body), "sessionStorage") {
		t.Fatal("browser secret persistence")
	}
	req.Path = "/v0/resource/plugins/github-copilot/status"
	raw, _ = json.Marshal(req)
	resp, _ = p.manage(raw)
	if resp.StatusCode == 200 {
		t.Fatal("status exposed as a public resource")
	}
}
func TestMetadataContract(t *testing.T) {
	r := pluginRegistration()
	fields := map[string]pluginapi.ConfigField{}
	for _, field := range r.Metadata.ConfigFields {
		fields[field.Name] = field
	}
	if fields["model_prefix"].Type != pluginapi.ConfigFieldTypeString || !strings.Contains(fields["model_prefix"].Description, "default copilot") {
		t.Fatal("missing namespace configuration or documented default")
	}
	if fields["models"].Type != pluginapi.ConfigFieldTypeArray {
		t.Fatal("missing model allowlist configuration")
	}

	if fields["models_excluded"].Type != pluginapi.ConfigFieldTypeArray {
		t.Fatal("missing model exclusion configuration")
	}
	if _, exists := fields["excluded_model_prefixes"]; exists {
		t.Fatal("obsolete model exclusion configuration exposed")
	}
	for _, name := range []string{"allow_insecure_base_urls", "allow_custom_endpoints", "allow_custom_scopes"} {
		field := fields[name]
		if field.Type != pluginapi.ConfigFieldTypeBoolean || !strings.Contains(field.Description, "Opt in") || !strings.Contains(field.Description, "disabled by default") || !strings.Contains(field.Description, "restart") {
			t.Errorf("%s is missing an explicit boolean opt-in and restart warning", name)
		}
	}

	if r.SchemaVersion != 6 || r.Metadata.Author == "" || r.Metadata.GitHubRepository == "" {
		t.Fatal("host rejects metadata")
	}
	if !r.Capabilities.ManagementAPI || !r.Capabilities.QuotaProvider || r.Capabilities.ExecutorModelScope != pluginapi.ExecutorModelScopeOAuth || len(r.Capabilities.ExecutorInputFormats) != 3 {
		t.Fatal("incorrect capabilities")
	}
}

func TestQuotaCapabilityDispatch(t *testing.T) {
	p := New(noHost{})
	defer p.Close()
	dispatch(t, p, "plugin.register", lifecycleRequest{SchemaVersion: 6, ConfigYAML: []byte("enabled: true")})
	for _, method := range []string{"quota.identifier", "quota.describe"} {
		out := dispatch(t, p, method, struct{}{})
		if string(out["ok"]) != "true" || !strings.Contains(string(out["result"]), "copilot") {
			t.Fatalf("quota capability failed: %s %s", method, out)
		}
		if method == "quota.describe" {
			var description pluginapi.QuotaDescribeResponse
			if json.Unmarshal(out["result"], &description) != nil || description.SupportsReset || description.DisplayName != "GitHub Copilot" {
				t.Fatal("incorrect quota description")
			}
		}
	}
	for _, method := range []string{"quota.fetch", "quota.reset"} {
		out := dispatch(t, p, method, struct{}{})
		if string(out["ok"]) != "false" || strings.Contains(string(out["error"]), "unknown_method") {
			t.Fatalf("quota method missing or invalid operation accepted: %s %s", method, out)
		}
	}
	var out map[string]json.RawMessage
	if json.Unmarshal(p.Dispatch("quota.fetch", []byte("invalid")), &out) != nil || string(out["ok"]) != "false" {
		t.Fatal("malformed quota request accepted")
	}
}
