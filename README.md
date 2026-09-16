# agy-proxy

Expose the Antigravity CLI's model access (your Google AI subscription) as an
Anthropic Messages API endpoint, so Claude Code can run on models such as
`gemini-3.8-flash`.

The proxy speaks Google's Code Assist `v1internal` protocol on
`daily-cloudcode-pa.googleapis.com` and authenticates with the OAuth
credentials that the `agy` CLI stored in the macOS keychain (service `gemini`,
account `antigravity`). It appears to Google as `antigravity/cli`.

> **Not a Google product.** agy-proxy is a custom, unofficial tool with no
> affiliation with or endorsement by Google; it exists only to let your own
> Google AI subscription be used from Claude Code. Driving a personal
> subscription through an unofficial client is outside Google's ToS for that
> API: if your account is rate-limited, suspended, or terminated as a
> consequence, the authors of this tool accept no responsibility whatsoever.
> Use at your own risk.

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

## Install from a release

Prebuilt binaries for macOS (Apple Silicon and Intel) and Linux (x86-64 and
ARM) are in [Releases](https://github.com/sodre90/agy-proxy/releases). Each
archive contains `agy-proxy` plus this README:

    tar xzf agy-proxy-v0.1.0-darwin-arm64.tar.gz
    ./agy-proxy-v0.1.0-darwin-arm64/agy-proxy -check

On Linux there is no macOS keychain to copy the refresh token from, so place
a `~/.config/agy-proxy/creds.json` yourself before the first start (copy it
from a Mac where `agy` is logged in, or write the `refresh_token` field by
hand).

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

### The `claude-agy` shell command

For `~/.bash_profile` (works in zsh too): starts the proxy on demand, waits
for it, and runs `claude` against it. Point `bin` at your binary.

```bash
claude-agy() {
  local base="http://127.0.0.1:8788"
  local bin="$HOME/bin/agy-proxy"

  if ! curl -fsS -m 2 -o /dev/null "$base/health" 2>/dev/null; then
    if [[ ! -x "$bin" ]]; then
      echo "claude-agy: proxy binary missing at $bin" >&2
      return 1
    fi
    mkdir -p ~/.config/agy-proxy
    nohup "$bin" >> ~/.config/agy-proxy/proxy.log 2>&1 &
    disown
    for _ in {1..40}; do
      curl -fsS -m 2 -o /dev/null "$base/health" 2>/dev/null && break
      sleep 0.25
    done
  fi
  if ! curl -fsS -m 2 -o /dev/null "$base/health" 2>/dev/null; then
    echo "claude-agy: agy-proxy did not come up on $base (see ~/.config/agy-proxy/proxy.log)" >&2
    return 1
  fi

  local model="${AGY_MODEL:-gemini-3.8-flash-high}"
  local -a claude_env=(
    -u ANTHROPIC_API_KEY
    ANTHROPIC_BASE_URL="$base"
    ANTHROPIC_AUTH_TOKEN="agy-proxy-local"
    ANTHROPIC_MODEL="$model"
    ANTHROPIC_DEFAULT_HAIKU_MODEL="gemini-3.8-flash-low"
    ANTHROPIC_DEFAULT_SONNET_MODEL="$model"
    ANTHROPIC_DEFAULT_OPUS_MODEL="gemini-3.8-flash-high"
    ANTHROPIC_DEFAULT_FABLE_MODEL="gemini-3.8-flash-high"
    CLAUDE_CODE_SUBAGENT_MODEL="${AGY_SUBAGENT_MODEL:-gemini-3.8-flash-medium}"
    API_TIMEOUT_MS="${AGY_API_TIMEOUT_MS:-600000}"
    CLAUDE_BYTE_STREAM_IDLE_TIMEOUT_MS="${AGY_STREAM_IDLE_TIMEOUT_MS:-300000}"
  )
  env "${claude_env[@]}" claude "$@"
}
```

`AGY_MODEL` overrides the main and Sonnet slots; the Haiku/Opus/Fable slots
are pinned to 3.8 here and `AGY_SUBAGENT_MODEL` covers the subagent slot.

## Translation notes

- The proxy emits an SSE `ping` every 5s until the first upstream chunk, so
  the stream never goes byte-silent (Claude Code drops silent streams --
  that is the "Streaming response ended before any complete data was
  received" error).
- `gemini-3-flash-agent` and `gemini-3.5-flash-*` appear in `GET /v1/models`
  but are decommissioned: they answer any prompt in 0.1s with "Gemini 3.5
  Flash is no longer available." Do not route to them.

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
  identifies them as Claude Code traffic: the "You are a Claude agent, built
  on Anthropic's Claude Agent SDK." identity sentence, and the
  `x-anthropic-billing-header: cc_version=...; cc_entrypoint=...;` line that
  Claude Code 2.1.273 prepends to the system prompt. The proxy rewrites the
  identity phrasing and strips `x-anthropic-*` header lines; user messages
  and tool definitions are not scanned.

  That block is reported as `429 Resource has been exhausted (e.g. check
  quota)`, which is not a quota error -- a real one names itself ("Individual
  quota reached ... Resets in 1h24m47s"). A Claude Code update can introduce
  a new tell; capture the envelope with `AGY_PROXY_DUMP` and bisect it.

## Security

- Credentials live in `~/.config/agy-proxy/creds.json` (0600); the process
  reads the keychain entry once via `/usr/bin/security` (macOS may prompt for
  keychain access).
- The proxy binds to loopback by default; set `-token` if you expose it
  elsewhere. Logs never contain tokens.
- Using a personal subscription through an unofficial client is outside
  Google's ToS for that API; the account risk is entirely yours (see the
  disclaimer at the top of this README — and the MIT `LICENSE`, under which
  the software is provided AS IS with no warranty of any kind).
