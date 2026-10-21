package openai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"acp-proxy/internal/acp"
)

// AgentInfo describes one resolvable ACP agent.
type AgentInfo struct {
	ID          string
	Name        string
	Version     string
	Description string
}

// Resolver turns an agent id into a runnable ACP agent, on demand.
type Resolver interface {
	// List returns every known agent.
	List(ctx context.Context) ([]AgentInfo, error)
	// Resolve returns the agent to spawn for an id. Agents are spawned
	// per request, so Resolve must be safe for concurrent use.
	Resolve(ctx context.Context, id string) (acp.Agent, error)
}

// UnknownAgentError reports an agent id that the resolver does not know;
// the API maps it to HTTP 404.
type UnknownAgentError struct {
	ID string
}

func (e *UnknownAgentError) Error() string {
	return fmt.Sprintf("unknown agent %q, see GET /agents", e.ID)
}

// Server serves the OpenAI-compatible API backed by ACP agents.
type Server struct {
	resolver Resolver
	pool     *acp.Pool
	// respStore stores sessions per response id, enabling
	// previous_response_id follow-ups.
	respStore *responseStore
}

// New builds a Server on top of an agent resolver and an agent process
// pool.
func New(resolver Resolver, pool *acp.Pool) *Server {
	return &Server{resolver: resolver, pool: pool, respStore: newResponseStore(1024)}
}

// Register wires the API onto an http.ServeMux. Every request is scoped
// to one agent through the URL prefix: /{agent}/v1/...
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /agents", s.agentsList)
	mux.HandleFunc("GET /{agent}/v1/models", s.modelsOne)
	mux.HandleFunc("POST /v1/chat/completions", s.unprefixed)
	mux.HandleFunc("POST /{agent}/v1/chat/completions", s.chatCompletions)
	mux.HandleFunc("POST /v1/responses", s.unprefixed)
	mux.HandleFunc("POST /{agent}/v1/responses", s.responses)
	mux.HandleFunc("GET /{agent}/v1/responses/{id}", s.getResponse)
}

func (s *Server) unprefixed(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "invalid_request_error",
		"no agent selected: use the /{agent}/v1/... path prefix")
}

// writeResolveError maps resolution failures to HTTP errors: unknown
// agents are 404s, everything else is a 502.
func (s *Server) writeResolveError(w http.ResponseWriter, err error) {
	var unknown *UnknownAgentError
	if errors.As(err, &unknown) {
		writeError(w, http.StatusNotFound, "server_error", err.Error())
		return
	}
	writeError(w, http.StatusBadGateway, "server_error", err.Error())
}

// agentsList lists every known agent id, one per line, as plain text.
func (s *Server) agentsList(w http.ResponseWriter, r *http.Request) {
	agents, err := s.resolver.List(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "server_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, a := range agents {
		fmt.Fprintln(w, a.ID)
	}
}

func (s *Server) modelsOne(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("agent")
	agents, err := s.resolver.List(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "server_error", err.Error())
		return
	}
	for _, a := range agents {
		if a.ID == id {
			writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": modelList([]AgentInfo{a})})
			return
		}
	}
	writeError(w, http.StatusNotFound, "invalid_request_error",
		fmt.Sprintf("unknown agent %q, see GET /agents", id))
}

func modelList(agents []AgentInfo) []map[string]any {
	data := make([]map[string]any, 0, len(agents))
	for _, a := range agents {
		data = append(data, map[string]any{
			"id":       a.ID,
			"object":   "model",
			"created":  0,
			"owned_by": "acp-proxy",
		})
	}
	return data
}

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	agentID := r.PathValue("agent")
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return
	}

	agent, err := s.resolver.Resolve(r.Context(), agentID)
	if err != nil {
		s.writeResolveError(w, err)
		return
	}

	promptText := buildPrompt(req.Messages)

	// The agent process is pooled: at most one per agent id, spawned
	// lazily and shared across requests. Each request gets its own ACP
	// session on it, released when the request completes.
	session, err := s.pool.Session(r.Context(), agentID, agent)
	if err != nil {
		writeError(w, http.StatusBadGateway, "server_error", err.Error())
		return
	}
	defer session.Close()

	if req.Stream {
		s.streamCompletion(w, r, session, req, promptText)
		return
	}
	s.blockingCompletion(w, r, session, req, promptText)
}

func (s *Server) blockingCompletion(w http.ResponseWriter, r *http.Request, session *acp.Session, req ChatCompletionRequest, promptText string) {
	var sb strings.Builder
	stopReason, err := session.Prompt(r.Context(), promptText, func(u acp.Update) {
		if u.Kind == "agent_message_chunk" {
			sb.WriteString(u.Text)
		}
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "server_error", "agent failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ChatCompletionResponse{
		ID:      "chatcmpl-" + RandomID(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []choice{{
			Index:        0,
			Message:      choiceMessage{Role: "assistant", Content: sb.String()},
			FinishReason: finishReason(stopReason),
		}},
		Usage: usage{},
	})
}

func (s *Server) streamCompletion(w http.ResponseWriter, r *http.Request, session *acp.Session, req ChatCompletionRequest, promptText string) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	id := "chatcmpl-" + RandomID()
	created := time.Now().Unix()
	send := func(chunk ChatCompletionChunk) {
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
	}

	// Opening chunk carrying the assistant role.
	send(ChatCompletionChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
		Choices: []chunkChoice{{Index: 0, Delta: delta{Role: "assistant"}}},
	})

	stopReason, err := session.Prompt(r.Context(), promptText, func(u acp.Update) {
		if u.Kind != "agent_message_chunk" || u.Text == "" {
			return
		}
		send(ChatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
			Choices: []chunkChoice{{Index: 0, Delta: delta{Content: u.Text}}},
		})
	})
	if err != nil {
		// The 200 header was already sent: surface the failure as an SSE
		// error event, then close the stream.
		errPayload, _ := json.Marshal(Error{Error: errorBody{
			Message: "agent failed: " + err.Error(), Type: "server_error"}})
		fmt.Fprintf(w, "data: %s\n\n", errPayload)
		if flusher != nil {
			flusher.Flush()
		}
	} else {
		send(ChatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
			Choices: []chunkChoice{{Index: 0, Delta: delta{}, FinishReason: finishReason(stopReason)}},
		})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// finishReason maps an ACP stop reason to an OpenAI finish reason.
func finishReason(stopReason string) string {
	switch stopReason {
	case acp.StopEndTurn:
		return "stop"
	case "max_tokens", "max_turn_requests":
		return "length"
	case "refusal":
		return "content_filter"
	case acp.StopCancelled:
		return "stop"
	default:
		return "stop"
	}
}

// buildPrompt renders the OpenAI message list as a single ACP prompt text
// block. Every request starts a fresh ACP session, so prior conversation
// is included as a transcript for context.
func buildPrompt(messages []Message) string {
	var sb strings.Builder
	for _, m := range messages {
		text := extractText(m.Content)
		switch m.Role {
		case "system":
			fmt.Fprintf(&sb, "[System instructions]\n%s\n\n", text)
		case "user":
			fmt.Fprintf(&sb, "User: %s\n", text)
		case "assistant", "tool":
			fmt.Fprintf(&sb, "Assistant: %s\n", text)
		}
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, typ, message string) {
	writeJSON(w, status, Error{Error: errorBody{Message: message, Type: typ}})
}

func RandomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "000000000000"
	}
	return hex.EncodeToString(b[:])
}
