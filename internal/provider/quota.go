package provider

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"cliproxyapi-github-copilot/internal/transport"
)

type quotaSnapshot struct {
	Unlimited        bool     `json:"unlimited"`
	HasQuota         *bool    `json:"has_quota"`
	PercentRemaining *float64 `json:"percent_remaining"`
	Remaining        *float64 `json:"remaining"`
	Entitlement      *float64 `json:"entitlement"`
	ResetAt          int64    `json:"quota_reset_at"`
	TokenBased       bool     `json:"token_based_billing"`
}

type quotaResponse struct {
	Plan         string                   `json:"copilot_plan"`
	SKU          string                   `json:"access_type_sku"`
	ResetDate    string                   `json:"quota_reset_date"`
	ResetDateUTC string                   `json:"quota_reset_date_utc"`
	Snapshots    map[string]quotaSnapshot `json:"quota_snapshots"`
}

// FetchQuota reads the selected credential's allowance with its GitHub token,
// rather than the short-lived token issued for Copilot inference.
func (s *Service) FetchQuota(ctx context.Context, callbackID string, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	if req.Provider != providerID {
		return pluginapi.QuotaFetchResponse{}, statusError("invalid_request", "quota provider must be copilot", http.StatusBadRequest)
	}
	storage, err := parseStorage(req.StorageJSON)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, statusError("invalid_auth", "Copilot quota credential is invalid", http.StatusBadRequest)
	}
	cfg := s.Config()
	if err := cfg.validateStorageOrigin(storage); err != nil {
		return pluginapi.QuotaFetchResponse{}, statusError("invalid_auth", err.Error(), http.StatusBadRequest)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := s.host.Do(ctx, callbackID, transport.Request{
		Method: http.MethodGet,
		URL:    cfg.GitHubAPIURL + "/copilot_internal/user",
		Headers: http.Header{
			"Accept":               {"application/vnd.github+json"},
			"Authorization":        {"token " + storage.GitHubAccessToken},
			"User-Agent":           {userAgent()},
			"X-GitHub-Api-Version": {"2025-04-01"},
		},
	})
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, statusError("quota_unavailable", "Copilot quota request failed", http.StatusBadGateway)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pluginapi.QuotaFetchResponse{}, upstreamStatusError(resp.StatusCode, "Copilot quota request failed")
	}
	out, err := normalizeQuota(resp.Body)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, statusError("invalid_quota", err.Error(), http.StatusBadGateway)
	}
	if serverTime, err := http.ParseTime(resp.Headers.Get("Date")); err == nil {
		out.ServerTimeOffsetMs = serverTime.Sub(s.now()).Milliseconds()
	}
	return out, nil
}

func normalizeQuota(raw []byte) (pluginapi.QuotaFetchResponse, error) {
	var data quotaResponse
	if json.Unmarshal(raw, &data) != nil || len(data.Snapshots) == 0 {
		return pluginapi.QuotaFetchResponse{}, errors.New("copilot quota response has no valid snapshots")
	}
	out := pluginapi.QuotaFetchResponse{}
	if plan, sku := strings.TrimSpace(data.Plan), strings.TrimSpace(data.SKU); plan != "" || sku != "" {
		out.Subscription = &pluginapi.QuotaSubscription{Plan: plan, TierID: sku}
	}
	var buckets []pluginapi.QuotaBucket
	recognized := false
	for _, window := range []struct{ name, description string }{
		{"premium_interactions", "Premium requests"},
		{"chat", "Chat"},
		{"completions", "Completions"},
	} {
		snapshot, exists := data.Snapshots[window.name]
		if !exists {
			continue
		}
		recognized = true
		if snapshot.Unlimited || (snapshot.HasQuota != nil && !*snapshot.HasQuota) {
			continue
		}
		fraction, err := quotaRemainingFraction(snapshot)
		if err != nil {
			return pluginapi.QuotaFetchResponse{}, err
		}
		reset, err := quotaResetTime(snapshot.ResetAt, data.ResetDateUTC, data.ResetDate)
		if err != nil {
			return pluginapi.QuotaFetchResponse{}, err
		}
		description := window.description
		if window.name == "premium_interactions" && snapshot.TokenBased {
			description = "Premium credits"
		}
		buckets = append(buckets, pluginapi.QuotaBucket{
			Window: window.name, Description: description, RemainingFraction: fraction, ResetTime: reset,
		})
	}
	if !recognized {
		return pluginapi.QuotaFetchResponse{}, errors.New("copilot quota response has no recognized snapshots")
	}
	if len(buckets) > 0 {
		out.Groups = []pluginapi.QuotaGroup{{DisplayName: "GitHub Copilot", Buckets: buckets}}
	}
	return out, nil
}

func quotaRemainingFraction(snapshot quotaSnapshot) (float64, error) {
	var fraction float64
	switch {
	case snapshot.PercentRemaining != nil:
		fraction = *snapshot.PercentRemaining / 100
	case snapshot.Remaining != nil && snapshot.Entitlement != nil && *snapshot.Entitlement > 0:
		fraction = *snapshot.Remaining / *snapshot.Entitlement
	default:
		return 0, errors.New("copilot quota snapshot has no remaining allowance")
	}
	if math.IsNaN(fraction) || math.IsInf(fraction, 0) || fraction < 0 || fraction > 1 {
		return 0, errors.New("copilot quota remaining allowance is invalid")
	}
	return fraction, nil
}

func quotaResetTime(epoch int64, utc, date string) (string, error) {
	if epoch < 0 || epoch > 253402300799 {
		return "", errors.New("copilot quota reset time is invalid")
	}
	if epoch > 0 {
		return time.Unix(epoch, 0).UTC().Format(time.RFC3339), nil
	}
	if utc != "" {
		parsed, err := time.Parse(time.RFC3339Nano, utc)
		if err != nil {
			return "", errors.New("copilot quota reset time is invalid")
		}
		return parsed.UTC().Format(time.RFC3339Nano), nil
	}
	if date != "" {
		parsed, err := time.Parse(time.DateOnly, date)
		if err != nil {
			return "", errors.New("copilot quota reset date is invalid")
		}
		return parsed.UTC().Format(time.RFC3339), nil
	}
	return "", nil
}
