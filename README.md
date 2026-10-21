# acp-proxy

A small web server that exposes OpenAI- and Anthropic-compatible HTTP
APIs and routes inbound requests to any
[ACP](https://agentclientprotocol.com) (Agent Client Protocol) server
via a built-in ACP client, backed by
[coder/acp-go-sdk](https://github.com/coder/acp-go-sdk).

## How it works

- Zero-config: running the binary starts a server with no agent running.
- Agents are resolved **on demand** from the
  [ACP registry](https://agentclientprotocol.com/get-started/registry) by
  id, through a URL prefix: `/{agent}/v1/...`. The proxy spawns the agent
  as a subprocess and speaks ACP over its stdio.
- Three API surfaces are served, each scoped to one agent through the
  prefix: the OpenAI chat completions API, the OpenAI Responses API and
  the Anthropic Messages API.
- Registry agents are resolved to a runnable command on first use: `npx`
  distributions run through `npx -y` (which itself caches the package);
  binary distributions first probe the PATH — an agent the user already
  installed elsewhere is used as-is (logged, since the registry's pinned
  version and checksum are bypassed) — and otherwise the archive is
  downloaded, sha256-verified and extracted to a cache directory.
- Each request gets a fresh ACP session on the agent's pooled process,
  released afterwards with `session/close` when the agent supports it.
  The request's message history is rendered as a single prompt transcript
  for context.
- `agent_message_chunk` updates become assistant message content, streamed
  via SSE or aggregated for non-streaming requests.

## Run

```sh
go build -o acp-proxy .
./acp-proxy                 # 127.0.0.1:11734, agents on demand
./acp-proxy 8080            # or any port; the examples below use the default

curl http://127.0.0.1:11734/agents             # plain text list of agent ids
curl http://127.0.0.1:11734/opencode/v1/models # one agent, OpenAI-shaped
curl http://127.0.0.1:11734/opencode/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"opencode","messages":[{"role":"user","content":"hello"}]}'
```

Agents need their own credentials (e.g. `ANTHROPIC_API_KEY`) in the
proxy's environment.

## API

Every request is scoped to one agent through the URL prefix. Three
OpenAI/Anthropic-compatible surfaces are served:

- `GET /agents` — plain text list of every known agent id (one per
  line): the ACP registry plus any statically declared agents.
- `GET /{agent}/v1/models` — one agent, OpenAI-shaped.

OpenAI API:

- `POST /{agent}/v1/chat/completions` — chat completions (`messages`,
  `stream`).
- `POST /{agent}/v1/responses` — the Responses API (`input` as string or
  items, `instructions`, `stream`, `store`). Responses are held in
  memory (up to 1024, oldest evicted) **and persisted to disk** under
  `<cache-dir>/responses/`, so `previous_response_id` follow-ups work
  across proxy restarts: live sessions are resumed directly, while
  post-restart follow-ups replay the stored transcript into a fresh ACP
  session. `GET /{agent}/v1/responses/{id}` retrieves a stored response.

Anthropic API:

- `POST /{agent}/v1/messages` — the Messages API (`max_tokens` required,
  `system`, `messages`, `stream`, `thinking`). With
  `thinking: {type: "enabled"}`, agent thought chunks are surfaced as
  thinking content blocks (with an empty signature). Stop reasons map
  `end_turn`/`cancelled` → `end_turn`, `max_tokens`/`max_turn_requests`
  → `max_tokens`, `refusal` → `refusal`.
- `POST /{agent}/v1/messages/count_tokens` — naive estimate (four
  characters per token; the proxy has no tokenizer).

The unprefixed endpoints return 404 with a hint.

Stop-reason mapping: `end_turn`/`cancelled` → `stop`, `max_tokens`/
`max_turn_requests` → `length`, `refusal` → `content_filter`.

## Options

Every option is a flag and an environment variable. Precedence, highest
first: positional port, flags, environment, defaults.

| Flag | Environment | Default | Meaning |
|---|---|---|---|
| (positional) | — | — | listen port |
| `-port` | `ACP_PROXY_PORT` | `11734` | listen port |
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
  present and extracted with path-traversal protection. When the agent
  binary is found on the PATH first, the pinned registry version and
  checksum are bypassed in favor of the local install.
- No token accounting (`usage` is zero everywhere, count_tokens is a
  rough character estimate), no tool-call surface in responses (ACP tool
  calls are agent-side and not exposed), no function calling on any
  surface. Sessions are closed after each request when the agent
  supports `session/close`, except those retained for Responses API
  follow-ups.
