# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]
### Fixed
### Added
### Changed
### Security


## [v0.1.1] - 2026-10-05

### Breaking

- ✨ feat!: `github_scope` other than `read:user` now requires `allow_custom_scopes: true`.
- ✨ feat!: GitHub, API and Copilot endpoints outside the default origins now require `allow_custom_endpoints: true`.
- ✨ feat!: changes to auth endpoints, client ID, scope or trust flags now require a plugin restart (`restart_required`), including across disable and re-enable.

### Added

- 💡 feat: `model_picker_required` option. When false (default), active chat models are exposed even if `model_picker_enabled` is false upstream.
- 💡 feat: `allow_raw_model_names` option to accept unprefixed model IDs such as `gpt-4o` alongside `copilot/gpt-4o`.
- 💡 feat: the dashboard status now reports `github_base_url`.
- 💡 feat: tenant-scoped GHE.com support (`https://TENANT.ghe.com`). It needs an explicit `github_client_id`, derives the API and Copilot origins from the tenant, and binds credentials to the tenant base URL.
- 🧪 tests: lifecycle, enterprise, stream lifecycle, transport and native integration tests, plus a native integration CI workflow.

### Fixed

- 🐛 fix: OpenAI streaming no longer double-wraps `data:` frames or emits a second `[DONE]`.
- 🐛 fix: multiline SSE error and terminal events are detected, and a stream that ends without a finish reason is rejected.
- 🐛 fix: stale token, retry and catalog caches are purged when a credential generation disappears.

### Security

- 🔒 security: Copilot inference requests, GitHub device login, token exchange/refresh, and GitHub user lookup no longer embed raw upstream error response text in client-facing error messages; each now returns a fixed, safe, status-coded message instead. Resolves the regression flagged as "under review" in the v0.1.1 upstream-error-body refactor.
- 🔒 security: validate the device-flow verification URL (scheme and host) before storing the login session, and expire stale pending sessions.
- 🔒 security: bound streams with a 2 minute idle timeout, a 30 minute total timeout and a 256 choice limit.
- 🔒 security: cancel and close upstream and output streams on cancellation, so blocked native read and emit calls are released.
- 🔒 security: reject credentials issued for a different GitHub origin.

### Changed

- 📦 package: CLIProxyAPI v8.0.12 to v8.0.15.
- 🪄 refactor: model catalog sync no longer writes `github_copilot_catalog_revision` back to host auth files. The registry can stay stale until the host calls `model.for_auth`.
- 🪄 refactor: Copilot user agent and plugin version 0.48.1 to 0.67.0.
- 🪄 refactor: upstream error bodies are now included in client-visible errors (redacted). Under review, because it reverses earlier hardening.
