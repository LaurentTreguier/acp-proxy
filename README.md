# acp-proxy

A small web server that exposes an OpenAI-compatible HTTP API and routes
inbound requests to any [ACP](https://agentclientprotocol.com) (Agent Client
Protocol) server via a built-in ACP client.

## How it works

- Clients speak the OpenAI API (`/v1/models`, `/v1/chat/completions`).
- Each configured "model" maps to an ACP agent command. The proxy spawns the
  agent as a subprocess, speaks JSON-RPC 2.0 over its stdin/stdout, and
  translates `session/update` notifications into OpenAI streaming chunks.
- Agent text output is surfaced as assistant message content (streaming via
  SSE or aggregated for non-streaming requests).

Status: early development on the `develop` branch.
