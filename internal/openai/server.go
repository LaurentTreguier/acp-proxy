package openai

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"acp-proxy/internal/acp"
	"acp-proxy/internal/config"
)

// Server serves the OpenAI-compatible API backed by ACP agents.
type Server struct {
	cfg *config.Options
}

// New builds a Server from the resolved options.
func New(cfg *config.Options) *Server {
	return &Server{cfg: cfg}
}

// Register wires the API onto an http.ServeMux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	data := make([]map[string]any, 0, len(s.cfg.Agents))
	for _, a := range s.cfg.Agents {
		data = append(data, map[string]any{
			"id":       a.Model,
			"object":   "model",
			"created":  0,
			"owned_by": "acp-proxy",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	agentCfg, ok := s.cfg.Agent(req.Model)
	if !ok {
		writeError(w, http.StatusNotFound, "invalid_request_error",
			fmt.Sprintf("model %q not found", req.Model))
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages is required")
		return
	}

	promptText := buildPrompt(req.Messages)

	agent := acp.Agent{
		Command:     agentCfg.Command,
		Args:        agentCfg.Args,
		Dir:         s.cfg.Dir,
		Permissions: s.cfg.PermissionPolicy(),
	}
	session, err := acp.Start(r.Context(), agent)
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
		ID:      "chatcmpl-" + randomID(),
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

	id := "chatcmpl-" + randomID()
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

func randomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "000000000000"
	}
	return hex.EncodeToString(b[:])
}
