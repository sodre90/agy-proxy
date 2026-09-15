# agy-proxy

Expose the Antigravity CLI's model access (your Google AI subscription) as an
Anthropic Messages API endpoint, so Claude Code can run on models such as
`gemini-3.8-flash`.

The proxy speaks Google's Code Assist `v1internal` protocol on
`daily-cloudcode-pa.googleapis.com` and authenticates with the OAuth
credentials that the `agy` CLI stored in the macOS keychain (service `gemini`,
account `antigravity`). It appears to Google as `antigravity/cli`.

## Build

    go build -o agy-proxy .

## Run

    ./agy-proxy -verbose

On first start it copies the keychain refresh token to
`~/.config/agy-proxy/creds.json` (mode 0600) and refreshes access tokens from
there. Make sure you are logged in (`agy` works) beforehand.

Flags (env fallbacks in parentheses):

| Flag | Default | Meaning |
| --- | --- | --- |
| `-addr` (`AGY_PROXY_ADDR`) | `127.0.0.1:8788` | listen address |
| `-model` (`AGY_PROXY_MODEL`) | `gemini-3.8-flash-high` | upstream model used for `sonnet`/`opus`/`haiku` aliases or empty model |
| `-base-url` (`AGY_PROXY_BASE_URL`) | `https://daily-cloudcode-pa.googleapis.com` | upstream base URL |
| `-token` (`AGY_PROXY_TOKEN`) | empty | optional token Claude Code must present; empty accepts anything |
| `-state-dir` (`AGY_PROXY_STATE`) | `~/.config/agy-proxy` | credential cache directory |
| `-check` | – | verify token refresh + model catalog, then exit |
| `-verbose` | – | debug logging (never logs tokens) |

`AGY_PROXY_DUMP=/path/to/file` additionally dumps the last translated upstream
request (conversation content included; for debugging only).

## Use from Claude Code

    ANTHROPIC_BASE_URL=http://127.0.0.1:8788 \
    ANTHROPIC_AUTH_TOKEN=anything \
    ANTHROPIC_MODEL=gemini-3.8-flash-high \
    claude

Model choices (from `agy`'s catalog, served at `GET /v1/models`):
`gemini-3.8-flash-high`, `gemini-3.8-flash-medium`, `gemini-3.8-flash-low`,
plus `gemini-3.1-pro-*` and other IDs. You can also point
`ANTHROPIC_DEFAULT_SONNET_MODEL` etc. at these IDs, or use plain
`sonnet`/`opus`/`haiku` and let the `-model` flag decide.

The `[1m]` context-window suffix is stripped before forwarding; thinking
level is derived from the model name suffix (`-high`/`-low`, else medium).

## Latency

`gemini-3.8-flash-*` takes 14-30s to first token on this account. That is
upstream, not the proxy: the real `agy --model gemini-3.8-flash-high -p "hi"`
takes 19s too. Every other catalog family answers in under 2s
(`gemini-3.7-flash-*`, `gemini-3.6-flash-*`, `gemini-3.1-pro-*`).
Export `AGY_MODEL=gemini-3.7-flash-high` for an interactive-feeling session
(that covers the main and Sonnet slots; the Haiku/Opus/subagent slots are
pinned to 3.8 in `claude-agy` and have their own `AGY_SUBAGENT_MODEL` knob).

3.8 also returns intermittent `503 No capacity available for model
gemini-3.8-flash-high` under load -- observed several times on 2026-09-15.

Two things keep Claude Code from giving up during that wait:

- The proxy emits an SSE `ping` every 5s until the first upstream chunk, so
  the stream never goes byte-silent (Claude Code drops silent streams --
  that is the "Streaming response ended before any complete data was
  received" error).
- `claude-agy` raises `API_TIMEOUT_MS` and
  `CLAUDE_BYTE_STREAM_IDLE_TIMEOUT_MS` for the non-streaming title/utility
  calls that cannot be kept alive. Both names come from the Claude Code
  binary; the ping fix is the one confirmed by testing.

`gemini-3-flash-agent` and `gemini-3.5-flash-*` appear in `GET /v1/models`
but are decommissioned: they answer any prompt in 0.1s with "Gemini 3.5 Flash
is no longer available." Do not route to them.

## Translation notes

- Anthropic `system` becomes the upstream `systemInstruction`; `assistant`
  becomes the `model` role, and mid-conversation `system` messages are folded
  into adjacent user turns (Gemini rejects a `system` role in `contents`).
- Tool definitions are pruned to the subset of JSON Schema the upstream proto
  accepts (`$schema`, `$defs`, `title`, `default`, etc. are dropped; type
  keywords are uppercased).
- Upstream `thoughtSignature` values are remembered per tool-call id and
  replayed with history; without them the model loses its reasoning chain on
  tool round-trips.
- The upstream server soft-blocks (429) requests whose `systemInstruction`
  identifies them as Claude Code traffic (it fingerprints the "You are a
  Claude agent, built on Anthropic's Claude Agent SDK." identity sentence).
  The proxy rewrites that identity phrasing to neutral Antigravity terms;
  user messages and tool definitions are not scanned.

## Security

- Credentials live in `~/.config/agy-proxy/creds.json` (0600); the process
  reads the keychain entry once via `/usr/bin/security` (macOS may prompt for
  keychain access).
- The proxy binds to loopback by default; set `-token` if you expose it
  elsewhere. Logs never contain tokens.
- Using a personal subscription through an unofficial client is outside
  Google's ToS for that API; the account risk is yours.
