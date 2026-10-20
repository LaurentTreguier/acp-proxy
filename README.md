# acp-proxy

A small web server that exposes an OpenAI-compatible HTTP API and routes
inbound requests to any [ACP](https://agentclientprotocol.com) (Agent Client
Protocol) server via a built-in ACP client.

## How it works

- Clients speak the OpenAI Chat Completions API (`/v1/models`, `/v1/chat/completions`).
- Each configured "model" maps to an ACP agent command. The proxy spawns the
  agent as a subprocess and talks ACP via [coder/acp-go-sdk](https://github.com/coder/acp-go-sdk),
  translating `session/update` notifications into OpenAI streaming chunks.
- `agent_message_chunk` updates become assistant message content, streamed
  via SSE or aggregated for non-streaming requests.
- Every request opens a fresh ACP session: the OpenAI message history is
  rendered as a single prompt transcript for context.

## Run

Zero-config: the server starts with sane defaults and serves an empty model
list until an agent is configured.

```sh
go build -o acp-proxy .
./acp-proxy                 # 127.0.0.1:8080
./acp-proxy 8080            # positional port
./acp-proxy -agent "claude-code=npx -y @zed-industries/claude-code-acp"
```

## Options

Every option is a flag and an environment variable. Precedence, highest
first: positional port, flags, environment, defaults.

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| (positional) | — | — | listen port |
| `-port` | `ACP_PROXY_PORT` | `8080` | listen port |
| `-host` | `ACP_PROXY_HOST` | `127.0.0.1` | listen host |
| `-agent` (repeatable) | `ACP_PROXY_AGENTS` (`;`-separated) | none | agent spec `[name=]command [args...]` |
| `-dir` | `ACP_PROXY_DIR` | current directory | working directory for agent sessions |
| `-auto` | `ACP_PROXY_AUTO` | off | auto-approve permission requests |

An agent spec without a `name=` prefix is exposed as the model `default`.
Agents inherit the proxy's environment. Without `-auto`, agent permission
requests are answered "cancelled".

## API

- `GET /v1/models` — list configured agents as models.
- `POST /v1/chat/completions` — same body shape as OpenAI
  (`model`, `messages`, `stream`). System/user/assistant/tool messages are
  flattened into the prompt transcript; text content parts are supported.

Stop-reason mapping: `end_turn`/`cancelled` → `stop`, `max_tokens`/
`max_turn_requests` → `length`, `refusal` → `content_filter`.

## Limitations

- No fs/terminal client capabilities are advertised; agents asking for them
  get a JSON-RPC "method not supported" error.
- No token accounting (`usage` is zero), no tool-call surface in the
  OpenAI response, no session reuse across requests.
