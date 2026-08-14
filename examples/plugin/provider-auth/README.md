# Provider auth plugin

`provider-auth` is a trusted in-process CLIProxyAPI plugin for providers whose
temporary bearer token is produced by a local command. The command, arguments, upstream
protocols, URLs, headers, models, aliases, and refresh interval are configuration. The
plugin contains no provider-specific endpoint, command, or model default.

The persisted auth file contains only a stable identity. Command output stays in plugin
memory and is never written to auth storage.

## Capabilities

- Auth provider: parse file-backed auth records and refresh command-generated bearer tokens.
- Model provider: register configured models and aliases for each auth record.
- Executor: forward Responses, Anthropic Messages, or OpenAI Chat Completions through the
  CPA host HTTP transport.
- Streaming: relay upstream HTTP/SSE chunks through the CPA plugin stream bridge.
- Unauthorized recovery: invalidate the cached token, rerun the command, and retry once.

The plugin intentionally does not open an upstream WebSocket. A downstream CPA WebSocket
can still be served through the plugin's HTTP/SSE executor path.

## Build

Build on the same operating system and architecture as CLIProxyAPI because Go
`c-shared` builds require CGO and a target C toolchain.

The module is pinned to the official CLIProxyAPI `v7.2.131` plugin SDK and does
not require a modified CLIProxyAPI source tree.

```bash
cd examples/plugin/provider-auth/go
CGO_ENABLED=1 go test ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o provider-auth.so .
```

Use `.dylib` on macOS and `.dll` on Windows.

## Plugin configuration

```yaml
plugins:
  enabled: true
  dir: "/absolute/path/to/plugins"
  configs:
    provider-auth:
      enabled: true
      priority: 100
      command: "/absolute/path/to/token-helper"
      args: ["print-token"]
      timeout-ms: 5000
      refresh-interval-seconds: 300
      disable-cooling: false
      protocols:
        responses:
          base-url: "https://provider.example/api/v1"
          # Defaults to /responses.
          path: "/responses"
          headers:
            X-Provider-Version: "2026-01-01"
        anthropic:
          base-url: "https://provider.example/anthropic"
          # Defaults to /messages.
          path: "/messages"
        chat-completions:
          base-url: "https://provider.example/openai/v1"
          # Defaults to /chat/completions.
          path: "/chat/completions"
      models:
        - name: "response-model-v2"
          alias: "response-latest"
          protocol: "responses"
          thinking-levels: ["medium", "high"]
        - name: "message-model-v3"
          alias: "message-latest"
          protocol: "anthropic"
        - name: "chat-model-v1"
          protocol: "chat-completions"
```

Only configured protocols are registered with the host. Every model must reference one
of them. `path` is optional and defaults according to the protocol.

## Auth record

Place one JSON file in `auth-dir`. `prefix` is optional but recommended when migrating
alongside an existing provider.

```json
{
  "type": "provider-auth",
  "id": "provider-auth-default",
  "label": "Command bearer token",
  "prefix": "command-provider",
  "proxy_url": "direct"
}
```

`proxy_url` is optional:

- Omit it to use the official CPA host HTTP bridge and host-level network policy.
- Use `direct` to connect without a proxy.
- Use an `http`, `https`, `socks5`, or `socks5h` URL for a provider-specific proxy.

## Command contract

- `command` must be an absolute executable path.
- The plugin invokes it directly with `args`; no shell is involved.
- Exit status must be zero.
- Standard output must contain exactly one non-empty, whitespace-free bearer token.
- Output is capped at 64 KiB.

## Security boundary

- Tokens are held in a process-local map keyed by auth ID.
- Tokens are cleared on plugin reconfigure and shutdown.
- Tokens are never returned in `AuthData`, plugin errors, or logs.
- Configured `Authorization`, credential, cookie, and hop-by-hop headers are rejected or
  replaced by the host-managed bearer token.
- Install only trusted plugin binaries; dynamic plugins run inside the CPA process.

By default, upstream requests use the official CPA host HTTP bridge. In
`v7.2.131`, that bridge follows the host-level network policy and does not
propagate an auth record's `proxy_url` across the plugin ABI. When
`proxy_url` is explicitly configured, this plugin owns the HTTP transport so
the setting remains provider-scoped; those requests do not pass through the
host request-log bridge.
