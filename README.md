# CLIProxyAPI GitHub Copilot plugin

## Description

A native Go plugin that connects CLIProxyAPI to GitHub Copilot.
It runs inside the host, not as a separate HTTP proxy.
Supported platforms are Linux and macOS on AMD64/ARM64, FreeBSD on AMD64, and Windows on AMD64.

This is an independent integration, not an official GitHub product.
Copilot subscriptions, account policies, and usage limits still apply.
GitHub's internal Copilot endpoints are not a stable public inference API.

## Features

- GitHub device-code login and reuse of existing `copilot` credentials.
- Account-specific model discovery with a cache lifetime of 30-60 seconds.
- Configurable model prefix, allowlist, aliases, and exclusions.
- OpenAI Chat Completions, Responses, and Claude Messages, including streaming and tools.
- An embedded dashboard for connecting accounts, checking status, and refreshing models.
- In-memory Copilot tokens with one renewal retry after an upstream `401`, before output starts.
- Native library validation, isolated host integration tests, and plugin-store ZIP packaging.

## Build

Use Go 1.27.1 or newer, a working C compiler, and GNU Make (`gmake` on FreeBSD).
The plugin requires a plugin-enabled CLIProxyAPI host; v8.0.15 is the integration-test baseline.

```sh
make check
make build
```

A local build produces `dist/<goos>/<goarch>/github-copilot.so`, `github-copilot.dylib` on macOS, or `github-copilot.dll` on Windows.
The build checks the library's architecture, native format, OS ABI, and exported entrypoint.
The host and library must match in OS, architecture, and C runtime.
Linux release builds use Ubuntu 26.04; build on your deployment distribution if you need a different libc baseline, including Alpine/musl.

Local builds use `UNCONFIGURED` repository metadata.
Supply your real repository URL for a distributable build:

```sh
make build VERSION=0.1.1 REPOSITORY=https://github.com/YOUR_ACCOUNT/YOUR_REPOSITORY
```

### FreeBSD cross-build

On Linux with Clang, LLD, and the script's download/extraction tools installed:

```sh
make freebsd
```

This uses a checksum-pinned FreeBSD 14.4 sysroot and produces `dist/freebsd/amd64/github-copilot.so`.
It cross-compiles libraries and tests without running a FreeBSD VM or executing the FreeBSD tests.

### Integration tests

Supply an existing plugin-enabled CLIProxyAPI binary matching your platform:

```sh
make integration CPA_BINARY=/absolute/path/to/cli-proxy-api
```

CI and release packaging both require native host integration checks against the same source commit.
CI also runs build and unit checks. The native job uses the unmodified v8.0.15 host pinned to commit `a4acc9f752bd46571f737a10c04bf413656ab06b`.
The integration target fails if `CPA_BINARY` or the built plugin is missing.
Tests use an isolated host, mock GitHub/Copilot endpoints, and temporary credentials, not your existing deployment or account.
Additional dependency checks are available through `make audit`.
Review licensing before publishing; no distribution license has been selected for the newly written code.

## Install

1. Stop CLIProxyAPI and back up its configuration and auth directory.
2. Disable the old `cliproxyapi-copilot` plugin, if installed.
3. Copy the built library into `<plugins-dir>/<goos>/<goarch>/`, keeping its original filename.
4. Merge the configuration below into the existing host configuration.
5. Restart CLIProxyAPI and open **GitHub Copilot** from its plugin management menu.

Keep existing API keys, management authentication, providers, and credentials.
Do not run the old and new Copilot plugins together: both own the `copilot` credential provider.
Do not add an `openai-compatibility` provider or a synthetic Copilot API key.

## Configure settings

### Basic configuration

```yaml
plugins:
  enabled: true
  dir: ./plugins
  configs:
    github-copilot:
      enabled: true
      model_prefix: copilot
      models: []
      models_excluded: []
      model_cache_ttl_seconds: 60
```

See [examples/config.yaml](examples/config.yaml) for the complete configuration.

### Account connection

Open `/v0/resource/plugins/github-copilot/dashboard`, enter the host management key, and select **Connect GitHub account**.
Approve the device code in GitHub, then use **Check accounts** or **Refresh models** to verify the connection.
The dashboard does not persist the management key in browser storage, URLs, or cookies.

Existing public GitHub credentials with `type: copilot` and `github_access_token` work without a new login.
New credentials record `github_base_url` and cannot be reused under a different configured GitHub origin.
GHE.com credentials require this origin binding; unbound legacy credentials are rejected before network access.
The host stores GitHub credentials; short-lived Copilot tokens stay in memory.
No login is performed during build, installation, or startup.

### Model settings

| Setting | Default | Purpose |
| --- | --- | --- |
| `model_prefix` | `copilot` | Exposes IDs as `copilot/<upstream-id>`; blank uses the default |
| `models` | `[]` | Optional `{name, alias}` allowlist; empty discovers all eligible models |
| `models_excluded` | `[]` | Case-insensitive upstream model ID prefixes to exclude |
| `model_cache_ttl_seconds` | `60` | Catalog cache lifetime, from 30 to 60 seconds |
| `model_picker_required` | `false` | When true, requires `model_picker_enabled: true` from GitHub; when false, exposes all active chat models |
| `allow_raw_model_names` | `false` | When true, exposes raw model names (e.g. `gpt-4o`) alongside prefixed ones (`copilot/gpt-4o`) |

```yaml
model_prefix: copilot
models:
  - name: upstream-model-id
  - name: upstream-model-id
    alias: coding
```

Without an alias, the public ID is `<model_prefix>/<name>`.
An explicit alias replaces the entire public ID and must be unique.
Use the exact IDs returned by the host's `/v1/models`; do not add a duplicate credential prefix.
Settings changes update the plugin's catalog, but the host's `/v1/models` listing can remain stale until the host requests the account models again.
The dashboard reports this limitation. Restart the host to rebuild its registry after a catalog or model-setting change.
Every inference request still checks the current account catalog, so a stale listing cannot authorize an unavailable model.

Only models enabled for the selected account, with chat capabilities and a supported endpoint, are eligible.
An explicit disabled or unknown policy rejects a model; an omitted policy is accepted.
Allowlisting does not enable unavailable models or override exclusions.
Expired catalogs and failed explicit refreshes fail closed; there are no static fallback models.
A multi-account host may list a combined catalog, but each request is checked against its selected account.

### Authentication settings

| Setting | Default | Purpose |
| --- | --- | --- |
| `github_client_id` | `Iv1.b507a08c87ecfe98` | Public GitHub device-flow client ID |
| `github_scope` | `read:user` | GitHub authorization scope; a broader scope needs explicit opt-in |
| `allow_custom_scopes` | `false` | Permit an operator-selected scope beyond `read:user` |
| `allow_custom_endpoints` | `false` | Trust custom non-GHE.com HTTPS endpoints; grants them access to the configured credentials and prompts |
| `oauth_timeout_seconds` | `900` | Device-login timeout, from 60 to 1800 seconds |
| `token_expiry_buffer_seconds` | `300` | Token renewal buffer, from 30 to 900 seconds |

### GitHub Enterprise Cloud on GHE.com

Set the tenant's root URL and an explicit OAuth client ID approved for device flow on that tenant:

```yaml
github_base_url: https://example.ghe.com
github_client_id: YOUR_TENANT_DEVICE_FLOW_CLIENT_ID
```

Omitting `github_api_url` and `copilot_api_url` derives `https://api.example.ghe.com` and `https://copilot-api.example.ghe.com`.
If you started from the public example, remove or replace its explicit public API overrides.
The OAuth client ID is public configuration, not a client secret. This plugin does not register an OAuth app or establish that any particular client ID is accepted by your enterprise.
Tenant policy, application authorization, and device-flow availability must be checked with your administrator.

OAuth, user lookup, token exchange, model discovery, inference, and refresh use the configured tenant endpoints.
Explicit API overrides must stay inside that tenant and use HTTPS origins without paths or ports.
The token response may select only the exact configured Copilot origin; another tenant and public `*.githubcopilot.com` endpoints are rejected.
Missing token endpoint metadata falls back to the configured tenant Copilot origin, never public GitHub.
A hostname ending in a dot is rejected. Root `ghe.com`, nested tenant names, and partial tenant API configuration are rejected.

The plugin stores origin-bound credentials under tenant-specific filenames, so equal logins on different tenants do not overwrite one another.
Use separate plugin/host instances and auth directories for simultaneous public GitHub and GHE.com accounts.
Authentication endpoint or client-ID changes require a plugin restart; model settings can still change in place.
Synthetic tests cover routing and rejection paths. Live enterprise login and region-specific service behavior have not been certified.
This configuration supports GitHub Enterprise Cloud with data residency on GHE.com; it does not claim GitHub Enterprise Server support.

Routing references: [GitHub API access on GHE.com](https://docs.github.com/en/enterprise-cloud@latest/admin/data-residency/about-github-enterprise-cloud-with-data-residency#api-access), [Copilot network requirements](https://docs.github.com/en/copilot/reference/copilot-allowlist-reference#copilot-on-ghecom), and [GitHub's Copilot endpoint derivation](https://github.com/github/gh-aw-mcpg/blob/main/docs/AWF_PIPELINE_ENVIRONMENT_VARIABLES.md#7-copilot-api-target-derivation).

### Credential updates and host compatibility

Background catalog synchronization is read-only. CLIProxyAPI v8.0.15 has no atomic metadata patch or compare-and-swap for auth records.
Writing a catalog revision through its full-document save callback could overwrite concurrent token rotation or administrator edits.
The plugin therefore refreshes local catalogs without rewriting credentials. Automatic host registry notification remains unavailable until the host offers a safe API.
Normal OAuth refresh still returns updated credentials through the host's dedicated refresh lifecycle.

### Security and API limits

By default, public GitHub mode accepts only `github.com` for OAuth, `api.github.com` for REST, and HTTPS `*.githubcopilot.com` origins for Copilot.
Other non-GHE.com HTTPS endpoints require `allow_custom_endpoints: true`; this explicitly trusts those destinations with legacy unbound credentials and request data.
This flag never relaxes GHE.com tenant isolation, HTTPS, userinfo, or query/fragment checks.
Endpoint overrides are trusted operator settings, not values to accept from user prompts.
HTTPS is required; `allow_insecure_base_urls` explicitly permits loopback test endpoints, including HTTP.
Authentication destinations, client ID, scope, and trust flags cannot change through reconfiguration, including a disable/re-enable cycle; restart the plugin to apply those changes.
Invalid configuration returns a native lifecycle error without replacing the existing service. An explicit `enabled: false` remains a successful disable when routing is unchanged.
The host may save a management config edit before applying it, so an HTTP success from that edit is not proof of successful plugin reconfiguration.
Check host plugin status/logs after edits; restore the last accepted configuration or restart with the intended routing if the host withdraws the failed registration.
HTTP requests use the host's supported HTTP callbacks and global proxy behavior. The plugin does not create a separate HTTP transport.
The v8.0.15 native HTTP callback bridge does not expose per-account/request proxy overrides to this plugin; do not rely on those overrides for isolation.
The v8.0.15 plugin API does not expose redirect control or the final response URL. Initial-origin checks cannot constrain later redirects followed by the host.
Use trusted endpoints and enforce destination/scheme restrictions in your outbound proxy or network policy; redirect safety remains a host limitation.
No host patch is required or applied. Configuring GHE.com does not by itself guarantee regional routing if an upstream redirects.
Protect the auth directory and management API, and disable request logging for credential exchanges.
Native plugins run with the host's privileges.

Supported client routes are `POST /v1/chat/completions`, `POST /v1/responses`, and `POST /v1/messages`.
Same-protocol requests preserve native fields; cross-protocol conversion cannot preserve every provider-specific feature.
Claude token counting is a local estimate, not an authoritative Anthropic count.
Embeddings, completion-only models, and generic HTTP forwarding are not supported.
Incomplete streams fail, and delivered streams are never replayed by the plugin.
Streams are limited to 2 minutes without upstream activity and 30 minutes total, including the wait for first headers.
A valid terminal event closes the upstream immediately; Chat Completions requires a finish reason before `[DONE]`.
Set host `request-retry: 0` if you also need to disable host-level retries.
