package provider

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"cliproxyapi-github-copilot/internal/transport"
)

const quotaFixture = `{"copilot_plan":"individual","access_type_sku":"pro","quota_reset_date":"2026-11-01","quota_reset_date_utc":"2026-11-01T00:00:00.000Z","quota_snapshots":{"chat":{"unlimited":true,"has_quota":true,"percent_remaining":100},"completions":{"unlimited":true,"has_quota":true,"percent_remaining":100},"premium_interactions":{"unlimited":false,"has_quota":true,"percent_remaining":97.6,"remaining":1464,"entitlement":1500,"token_based_billing":true}}}`

func TestFetchQuotaSelectedCredentialAndTransport(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	calls := 0
	h := &mockHost{do: func(ctx context.Context, callback string, req transport.Request) (transport.Response, error) {
		calls++
		if callback != "quota-callback" || req.Method != http.MethodGet || req.URL != "https://api.github.com/copilot_internal/user" {
			t.Fatalf("incorrect quota transport: callback=%q request=%+v", callback, req)
		}
		if req.Headers.Get("Authorization") != "token selected-account" || req.Headers.Get("Accept") != "application/vnd.github+json" || req.Headers.Get("User-Agent") == "" {
			t.Fatal("quota request did not use the selected GitHub credential and API headers")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 15*time.Second {
			t.Fatal("quota request has no bounded timeout")
		}
		return transport.Response{StatusCode: 200, Body: []byte(quotaFixture), Headers: http.Header{"Date": {now.Add(2 * time.Second).Format(http.TimeFormat)}}}, nil
	}}
	s := newTestService(t, h)
	s.now = func() time.Time { return now }
	got, err := s.FetchQuota(context.Background(), "quota-callback", pluginapi.QuotaFetchRequest{
		Provider: "copilot", AuthID: "selected", StorageJSON: []byte(`{"type":"copilot","github_access_token":"selected-account"}`),
		Metadata: map[string]any{"github_access_token": "wrong-account"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(s.tokenEntries) != 0 || got.ServerTimeOffsetMs != 2000 {
		t.Fatalf("unexpected token exchange or clock offset: calls=%d offset=%d", calls, got.ServerTimeOffsetMs)
	}
	if got.Subscription == nil || got.Subscription.Plan != "individual" || got.Subscription.TierID != "pro" {
		t.Fatalf("missing subscription: %+v", got.Subscription)
	}
	if len(got.Groups) != 1 || len(got.Groups[0].Buckets) != 1 {
		t.Fatalf("unlimited allowances must not become finite quota bars: %+v", got.Groups)
	}
	bucket := got.Groups[0].Buckets[0]
	if bucket.Window != "premium_interactions" || bucket.Description != "Premium credits" || math.Abs(bucket.RemainingFraction-0.976) > 1e-12 || bucket.ResetTime != "2026-11-01T00:00:00Z" {
		t.Fatalf("incorrect premium allowance: %+v", bucket)
	}
}

func TestFetchQuotaCredentialValidationBeforeNetwork(t *testing.T) {
	h := &mockHost{do: func(context.Context, string, transport.Request) (transport.Response, error) {
		t.Fatal("invalid credential reached upstream")
		return transport.Response{}, nil
	}}
	s := newTestService(t, h)
	for _, req := range []pluginapi.QuotaFetchRequest{
		{Provider: "codex", StorageJSON: []byte(`{"type":"copilot","github_access_token":"secret"}`)},
		{Provider: "copilot"},
		{Provider: "copilot", StorageJSON: []byte(`{"type":"copilot"}`)},
		{Provider: "copilot", StorageJSON: []byte(`{"type":"codex","github_access_token":"secret"}`)},
		{Provider: "copilot", StorageJSON: []byte(`{"type":"copilot","github_access_token":"secret","github_base_url":"https://other.ghe.com"}`)},
	} {
		if _, err := s.FetchQuota(context.Background(), "", req); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid credential accepted or exposed: %v", err)
		}
	}
}

func TestFetchQuotaEnterpriseOrigin(t *testing.T) {
	h := &mockHost{do: func(_ context.Context, _ string, req transport.Request) (transport.Response, error) {
		if req.URL != "https://api.example.ghe.com/copilot_internal/user" {
			t.Fatalf("quota request left the configured tenant: %s", req.URL)
		}
		return reply(quotaFixture), nil
	}}
	s := newTestService(t, h)
	if err := s.Configure([]byte("github_base_url: https://example.ghe.com\ngithub_client_id: tenant-client\n")); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"", "https://other.ghe.com", "https://example.ghe.com"} {
		storage := []byte(`{"type":"copilot","github_access_token":"secret","github_base_url":"` + origin + `"}`)
		_, err := s.FetchQuota(context.Background(), "", pluginapi.QuotaFetchRequest{Provider: "copilot", StorageJSON: storage})
		if (err == nil) != (origin == "https://example.ghe.com") {
			t.Fatalf("unexpected origin acceptance for %q: %v", origin, err)
		}
	}
}

func TestFetchQuotaErrorsDoNotExposeUpstreamData(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp transport.Response
		err  error
	}{
		{"transport", transport.Response{}, errors.New("private-secret")},
		{"unauthorized", transport.Response{StatusCode: 401, Body: []byte("private-secret")}, nil},
		{"rate limit", transport.Response{StatusCode: 429, Body: []byte("private-secret")}, nil},
		{"malformed JSON", reply("private-secret"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestService(t, &mockHost{do: func(context.Context, string, transport.Request) (transport.Response, error) { return tc.resp, tc.err }})
			_, err := s.FetchQuota(context.Background(), "", pluginapi.QuotaFetchRequest{Provider: "copilot", StorageJSON: []byte(`{"type":"copilot","github_access_token":"private-secret"}`)})
			if err == nil || strings.Contains(err.Error(), "private-secret") {
				t.Fatalf("upstream data exposed or error absent: %v", err)
			}
		})
	}
}

func TestNormalizeQuotaFiniteAndUnlimitedAllowances(t *testing.T) {
	for _, tc := range []struct {
		name, snapshots string
		fractions       []float64
	}{
		{"free plan", `{"premium_interactions":{"has_quota":false},"chat":{"remaining":20,"entitlement":200},"completions":{"percent_remaining":0}}`, []float64{0.1, 0}},
		{"full allowance", `{"premium_interactions":{"percent_remaining":100}}`, []float64{1}},
		{"all unlimited", `{"chat":{"unlimited":true},"completions":{"unlimited":true}}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeQuota([]byte(`{"copilot_plan":"free","quota_reset_date":"2026-11-01","quota_snapshots":` + tc.snapshots + `}`))
			if err != nil {
				t.Fatal(err)
			}
			var buckets []pluginapi.QuotaBucket
			for _, group := range got.Groups {
				buckets = append(buckets, group.Buckets...)
			}
			if len(buckets) != len(tc.fractions) {
				t.Fatalf("buckets=%+v want %d", buckets, len(tc.fractions))
			}
			for i, want := range tc.fractions {
				if math.Abs(buckets[i].RemainingFraction-want) > 1e-12 || buckets[i].ResetTime != "2026-11-01T00:00:00Z" {
					t.Fatalf("incorrect bucket: %+v", buckets[i])
				}
			}
		})
	}
}

func TestNormalizeQuotaRejectsMissingAndInvalidReadings(t *testing.T) {
	for _, raw := range []string{
		`{}`, `{"quota_snapshots":{}}`, `{"quota_snapshots":{"unknown":{"percent_remaining":50}}}`,
		`{"quota_snapshots":{"premium_interactions":{}}}`, `{"quota_snapshots":{"premium_interactions":{"percent_remaining":null}}}`,
		`{"quota_snapshots":{"premium_interactions":{"percent_remaining":-1}}}`, `{"quota_snapshots":{"premium_interactions":{"percent_remaining":101}}}`,
		`{"quota_snapshots":{"premium_interactions":{"percent_remaining":"50"}}}`, `{"quota_snapshots":{"premium_interactions":{"remaining":10,"entitlement":0}}}`,
		`{"quota_reset_date":"invalid","quota_snapshots":{"premium_interactions":{"percent_remaining":50}}}`,
		`{"quota_reset_date_utc":"invalid","quota_snapshots":{"premium_interactions":{"percent_remaining":50}}}`,
		`{"quota_snapshots":{"premium_interactions":{"percent_remaining":50,"quota_reset_at":-1}}}`,
	} {
		if _, err := normalizeQuota([]byte(raw)); err == nil {
			t.Fatalf("invalid response accepted: %s", raw)
		}
	}
}

func TestQuotaResetTimePrecedence(t *testing.T) {
	const reset = "2026-11-01T00:00:00Z"
	date, _ := time.Parse(time.RFC3339, reset)
	for _, tc := range []struct {
		epoch     int64
		utc, date string
	}{
		{date.Unix(), "invalid", "invalid"},
		{0, "2026-11-01T02:00:00+02:00", "invalid"},
		{0, "", "2026-11-01"},
	} {
		got, err := quotaResetTime(tc.epoch, tc.utc, tc.date)
		if err != nil || got != reset {
			t.Fatalf("reset=%q err=%v", got, err)
		}
	}
}
