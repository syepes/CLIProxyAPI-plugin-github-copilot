package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cliproxyapi-github-copilot/internal/transport"
)

type copilotTokenResponse struct {
	Token      string            `json:"token"`
	ExpiresAt  int64             `json:"expires_at"`
	RefreshIn  int64             `json:"refresh_in"`
	Endpoints  map[string]string `json:"endpoints"`
	TokenError string            `json:"error"`
}

type copilotTokenEntry struct {
	Token       string
	APIBaseURL  string
	RefreshAt   time.Time
	ExpiresAt   time.Time
	Fingerprint string
}

type tokenFlight struct {
	done  chan struct{}
	entry copilotTokenEntry
	err   error
}

func (s *Service) copilotToken(ctx context.Context, callbackID, authID string, storage authStorage) (copilotTokenEntry, error) {
	cfg := s.Config()
	if err := cfg.validateStorageOrigin(storage); err != nil {
		return copilotTokenEntry{}, err
	}
	key := cacheKey(authID, storage)
	now := s.now()
	s.tokenMu.Lock()
	if retry := s.tokenRetries[key]; now.Before(retry) {
		cached, ok := s.tokenEntries[key]
		s.tokenMu.Unlock()
		if ok && now.Before(cached.ExpiresAt) {
			return cached, nil
		}
		return copilotTokenEntry{}, errors.New("copilot token exchange is backing off")
	}
	if cached, ok := s.tokenEntries[key]; ok && now.Before(cached.ExpiresAt) && now.Before(cached.RefreshAt) {
		s.tokenMu.Unlock()
		return cached, nil
	}
	flight := s.tokenInflight[key]
	if flight == nil {
		flight = &tokenFlight{done: make(chan struct{})}
		s.tokenInflight[key] = flight
		if !s.spawn(func() {
			grantCtx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
			defer cancel()
			entry, err := s.exchangeCopilotToken(grantCtx, "", tokenFingerprint(storage.GitHubAccessToken), storage.GitHubAccessToken)
			if err == nil {
				remaining := entry.ExpiresAt.Sub(s.now())
				buffer := min(cfg.tokenExpiryBuffer(), remaining/5)
				scheduled := entry.ExpiresAt.Add(-buffer)
				if entry.RefreshAt.IsZero() || scheduled.Before(entry.RefreshAt) {
					entry.RefreshAt = scheduled
				}
			}
			s.tokenMu.Lock()
			flight.entry = entry
			flight.err = err
			if err == nil {
				s.tokenEntries[key] = entry
				delete(s.tokenRetries, key)
			} else {
				s.tokenRetries[key] = s.now().Add(5 * time.Second)
				if cached, ok := s.tokenEntries[key]; ok && s.now().Before(cached.ExpiresAt) {
					flight.entry = cached
					flight.err = nil
				}
			}
			delete(s.tokenInflight, key)
			close(flight.done)
			s.tokenMu.Unlock()
		}) {
			delete(s.tokenInflight, key)
			s.tokenMu.Unlock()
			return copilotTokenEntry{}, errors.New("plugin unavailable")
		}
	}
	s.tokenMu.Unlock()
	select {
	case <-s.ctx.Done():
		return copilotTokenEntry{}, s.ctx.Err()
	case <-ctx.Done():
		return copilotTokenEntry{}, ctx.Err()
	case <-flight.done:
		if flight.err == nil && !s.now().Before(flight.entry.ExpiresAt) {
			return copilotTokenEntry{}, errors.New("copilot token expired during exchange")
		}
		return flight.entry, flight.err
	}
}

func (s *Service) exchangeCopilotToken(ctx context.Context, callbackID, fingerprint, githubToken string) (copilotTokenEntry, error) {
	cfg := s.Config()
	resp, errDo := s.host.Do(ctx, callbackID, transport.Request{
		Method: http.MethodGet,
		URL:    cfg.GitHubAPIURL + "/copilot_internal/v2/token",
		Headers: http.Header{
			"Accept":               []string{"application/vnd.github+json"},
			"Authorization":        []string{"token " + githubToken},
			"User-Agent":           []string{userAgent()},
			"X-GitHub-Api-Version": []string{"2025-04-01"},
		},
	})
	if errDo != nil {
		return copilotTokenEntry{}, fmt.Errorf("exchange GitHub OAuth token for Copilot token: %w", errDo)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return copilotTokenEntry{}, upstreamStatusError(resp.StatusCode, "Copilot token request failed")
	}
	var token copilotTokenResponse
	if errUnmarshal := json.Unmarshal(resp.Body, &token); errUnmarshal != nil {
		return copilotTokenEntry{}, fmt.Errorf("decode Copilot token response: %w", errUnmarshal)
	}
	token.Token = strings.TrimSpace(token.Token)
	if token.Token == "" {
		return copilotTokenEntry{}, fmt.Errorf("copilot token response has no token")
	}
	expiresAt := tokenExpiry(token, s.now())
	if !expiresAt.After(s.now()) {
		return copilotTokenEntry{}, errors.New("copilot token has missing or expired expiry")
	}
	apiBase, errBase := copilotAPIBase(token.Endpoints, cfg)
	if errBase != nil {
		return copilotTokenEntry{}, errBase
	}
	refreshAt := time.Time{}
	if token.RefreshIn > 0 {
		refreshAt = s.now().Add(time.Duration(token.RefreshIn) * time.Second)
	}
	return copilotTokenEntry{
		RefreshAt:   refreshAt,
		Token:       token.Token,
		APIBaseURL:  apiBase,
		ExpiresAt:   expiresAt,
		Fingerprint: fingerprint,
	}, nil
}

func tokenExpiry(token copilotTokenResponse, now time.Time) time.Time {
	if token.ExpiresAt > 0 {
		return time.Unix(token.ExpiresAt, 0)
	}
	for _, part := range strings.Split(token.Token, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || key != "exp" {
			continue
		}
		seconds, errParse := strconv.ParseInt(value, 10, 64)
		if errParse == nil && seconds > 0 {
			return time.Unix(seconds, 0)
		}
	}
	_ = now // refresh_in is a renewal hint, not proof of token validity.
	return time.Time{}
}

func copilotAPIBase(endpoints map[string]string, cfg Config) (string, error) {
	raw := strings.TrimRight(strings.TrimSpace(endpoints["api"]), "/")
	if raw == "" {
		raw = cfg.CopilotAPIURL
	}
	parsed, errParse := url.Parse(raw)
	if errParse != nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("copilot token returned an invalid API endpoint")
	}
	if parsed.Scheme != "https" && !(cfg.AllowInsecureBaseURLs && parsed.Scheme == "http") {
		return "", fmt.Errorf("copilot API endpoint must use HTTPS")
	}
	if parsed.User != nil || parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("copilot API endpoint contains query or fragment")
	}
	configured, _ := url.Parse(cfg.CopilotAPIURL)
	if cfg.enterpriseHost() != "" {
		if parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, configured.Host) || parsed.Path != configured.Path {
			return "", errors.New("copilot API endpoint does not match configured GHE.com origin")
		}
		return raw, nil
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Scheme != configured.Scheme || parsed.Host != configured.Host {
		if parsed.Scheme != "https" || parsed.Port() != "" || !(host == "api.githubcopilot.com" || strings.HasSuffix(host, ".githubcopilot.com")) {
			return "", errors.New("untrusted Copilot API endpoint")
		}
	}
	return raw, nil
}

func (s *Service) invalidateToken(authID string, storage authStorage, rejected string) {
	s.tokenMu.Lock()
	key := cacheKey(authID, storage)
	if entry, ok := s.tokenEntries[key]; ok && entry.Token == rejected {
		delete(s.tokenEntries, key)
	}
	s.tokenMu.Unlock()
}
func (s *Service) invalidateAuth(authID string) {
	prefix := authID + "|"
	s.tokenMu.Lock()
	for key := range s.tokenEntries {
		if strings.HasPrefix(key, prefix) {
			delete(s.tokenEntries, key)
		}
	}
	s.tokenMu.Unlock()
	s.modelMu.Lock()
	for key := range s.modelEntries {
		if strings.HasPrefix(key, prefix) {
			delete(s.modelEntries, key)
		}
	}
	s.modelMu.Unlock()
}
