# acp-proxy

A small web server that exposes an OpenAI-compatible HTTP API and routes
inbound requests to any [ACP](https://agentclientprotocol.com) (Agent Client
Protocol) server via a built-in ACP client, backed by
[coder/acp-go-sdk](https://github.com/coder/acp-go-sdk).

## How it works

- Zero-config: running the binary starts an OpenAI-compatible server with
  no agent running.
- Agents are resolved **on demand** from the
  [ACP registry](https://agentclientprotocol.com/get-started/registry) by
  id, through a URL prefix: `/{agent}/v1/chat/completions`. The proxy
  spawns the agent as a subprocess and speaks ACP over its stdio.
- Registry agents are downloaded automatically on first use: `npx`
  distributions run through `npx -y` (which fetches the package), binary
  distributions are downloaded, sha256-verified and extracted to a cache
  directory.
- Each request gets a fresh ACP session on the agent's pooled process,
  released afterwards with `session/close` when the agent supports it.
  The OpenAI message history is rendered as a single prompt transcript
  for context.
- `agent_message_chunk` updates become assistant message content, streamed
  via SSE or aggregated for non-streaming requests.

## Run

```sh
go build -o acp-proxy .
./acp-proxy                 # 127.0.0.1:8080, agents on demand
./acp-proxy 8080            # positional port

curl http://127.0.0.1:8080/v1/models            # every registry agent
curl http://127.0.0.1:8080/claude-acp/v1/models # one agent
curl http://127.0.0.1:8080/claude-acp/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"claude-acp","messages":[{"role":"user","content":"hello"}]}'
```

Agents need their own credentials (e.g. `ANTHROPIC_API_KEY`) in the
proxy's environment.

## API

Every completion request is scoped to one agent through the URL prefix.

- `GET /v1/models` — every known agent: the ACP registry plus any
  statically declared agents.
- `GET /{agent}/v1/models` — one agent.
- `POST /{agent}/v1/chat/completions` — completion routed to that agent,
  spawned on demand. Same body shape as OpenAI (`messages`, `stream`).

The unprefixed `POST /v1/chat/completions` is not supported and returns
404 with a hint.

Stop-reason mapping: `end_turn`/`cancelled` → `stop`, `max_tokens`/
`max_turn_requests` → `length`, `refusal` → `content_filter`.

## Options

Every option is a flag and an environment variable. Precedence, highest
first: positional port, flags, environment, defaults.

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| (positional) | — | — | listen port |
| `-port` | `ACP_PROXY_PORT` | `8080` | listen port |
| `-host` | `ACP_PROXY_HOST` | `127.0.0.1` | listen host |
| `-registry` | `ACP_PROXY_REGISTRY` | official ACP registry | registry index URL |
| `-cache-dir` | `ACP_PROXY_CACHE_DIR` | user cache dir | where downloaded agents live |
| `-agent` (repeatable) | `ACP_PROXY_AGENTS` (`;`-separated) | none | static agent `[id=]command [args...]`, overrides registry entries |
| `-dir` | `ACP_PROXY_DIR` | current directory | working directory for agent sessions |
| `-auto` | `ACP_PROXY_AUTO` | off | auto-approve permission requests |

## Security notes

- Agent permission requests are answered "cancelled" unless `-auto` is
  set. With `-auto`, the first `allow_*` option is selected — agents can
  then run tools unattended.
- The proxy advertises no fs/terminal client capabilities; agents asking
  for them get a JSON-RPC "method not supported" error.
- Binary archives are verified against the registry's sha256 when
  present and extracted with path-traversal protection.
- No token accounting (`usage` is zero), no tool-call surface in the
  OpenAI response, no session reuse across requests (sessions are
  closed after each request when the agent supports `session/close`).
