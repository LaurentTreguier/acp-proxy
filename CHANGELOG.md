# Changelog

## v0.1.0 — 2026-10-21

First release.

acp-proxy is a zero-config web server exposing OpenAI- and
Anthropic-compatible HTTP APIs backed by ACP (Agent Client Protocol)
agents, resolved on demand from the ACP registry through a URL prefix.

### Features

- OpenAI API: `POST /{agent}/v1/chat/completions` and the Responses API
  (`POST /{agent}/v1/responses`, streaming and non-streaming, with
  `previous_response_id` follow-ups persisted to disk so conversations
  survive restarts), plus `GET /{agent}/v1/models` and the plain-text
  `GET /agents` listing.
- Anthropic API: `POST /{agent}/v1/messages` (streaming and
  non-streaming, thinking blocks from agent thought chunks) and
  `POST /{agent}/v1/messages/count_tokens`.
- Dynamic agents: agent ids resolve at request time through the
  `/{agent}/v1/...` prefix; at most one process per agent is pooled and
  spawned lazily.
- ACP registry integration: `npx` distributions run through `npx -y`;
  binary distributions prefer an already-installed binary found on the
  PATH, otherwise download, sha256-verify and extract the archive into
  the cache directory.
- Zero-config: runs with no arguments on `127.0.0.1:11734`; every option
  available as a flag and an `ACP_PROXY_*` environment variable
  (positional port, `-host`, `-registry`, `-cache-dir`, `-agent`,
  `-dir`, `-auto`).
- Static agent overrides (`-agent "[id=]command [args...]"`), permission
  policy (auto-approve or cancel), agent stderr surfaced in the log.

### Known limitations

- No token accounting: `usage` is zeroed everywhere and count_tokens is
  a rough four-characters-per-token estimate.
- Tool calls are agent-side only; no tool-call or function-calling
  surface is exposed on any API.
- Thinking blocks carry an empty signature.
- Disk response records accumulate without eviction (memory is capped).
