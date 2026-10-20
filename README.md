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

```sh
go build -o acp-proxy .
./acp-proxy -config config.json        # -addr overrides the listen address
```

## Configure

JSON config file (see `config.example.json`):

```json
{
  "addr": "127.0.0.1:8080",
  "agents": [
    {
      "model": "claude-code",
      "command": "npx",
      "args": ["-y", "@zed-industries/claude-code-acp"],
      "dir": ".",
      "permissions": "cancel"
    }
  ]
}
```

Per-agent fields:

| Field | Meaning |
|---|---|
| `model` | Name exposed through the OpenAI API |
| `command`, `args` | ACP agent subprocess to spawn |
| `dir` | Working directory for the agent and the ACP session `cwd` |
| `env` | Extra environment variables for the agent |
| `permissions` | `cancel` (default): permission requests are answered "cancelled"; `auto`: the first `allow_*` option is selected |

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
