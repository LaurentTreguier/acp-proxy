// Package anthropic implements the Anthropic Messages API on top of ACP
// agents: POST /{agent}/v1/messages (streaming and non-streaming) and
// POST /{agent}/v1/messages/count_tokens.
package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"acp-proxy/internal/acp"
)

// messageRequest is the subset of the Messages API request the proxy
// consumes.
type messageRequest struct {
	Model     string          `json:"model"`
	MaxTokens *int            `json:"max_tokens"`
	System    json.RawMessage `json:"system"`
	Messages  []messageParam  `json:"messages"`
	Stream    bool            `json:"stream"`
	Thinking  *thinkingConfig `json:"thinking,omitempty"`
}

type thinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type messageParam struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// contentBlock is one block of the assistant message content.
type contentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
}

type usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// messageResponse is the non-streaming Messages API response.
type messageResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Model        string         `json:"model"`
	Content      []contentBlock `json:"content"`
	StopReason   string         `json:"stop_reason"`
	StopSequence any            `json:"stop_sequence"`
	Usage        usage          `json:"usage"`
}

// Server serves the Anthropic Messages API backed by ACP agents.
type Server struct {
	resolver acp.Resolver
	pool     *acp.Pool
}

// New builds a Server on top of an agent resolver and an agent process
// pool.
func New(resolver acp.Resolver, pool *acp.Pool) *Server {
	return &Server{resolver: resolver, pool: pool}
}

// Register wires the API onto an http.ServeMux. Every request is scoped
// to one agent through the URL prefix.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/messages", s.unprefixed)
	mux.HandleFunc("POST /{agent}/v1/messages", s.messages)
	mux.HandleFunc("POST /{agent}/v1/messages/count_tokens", s.countTokens)
}

func (s *Server) unprefixed(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "invalid_request_error",
		"no agent selected: use the /{agent}/v1/messages path prefix")
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request) {
	var req messageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	agentID := r.PathValue("agent")
	if req.MaxTokens == nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "max_tokens: Field required")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages: at least one message is required")
		return
	}

	agent, err := s.resolver.Resolve(r.Context(), agentID)
	if err != nil {
		s.writeResolveError(w, err)
		return
	}

	session, err := s.pool.Session(r.Context(), agentID, agent)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	defer session.Close()

	prompt := buildPrompt(req)
	thinking := req.Thinking != nil && req.Thinking.Type == "enabled"

	if req.Stream {
		s.streamMessage(w, r, session, req, prompt, thinking)
		return
	}
	s.blockingMessage(w, r, session, req, prompt, thinking)
}

func (s *Server) writeResolveError(w http.ResponseWriter, err error) {
	var unknown *acp.UnknownAgentError
	if errors.As(err, &unknown) {
		writeError(w, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	writeError(w, http.StatusBadGateway, "api_error", err.Error())
}

func (s *Server) blockingMessage(w http.ResponseWriter, r *http.Request, session *acp.Session, req messageRequest, prompt string, thinking bool) {
	var text, thought strings.Builder
	stopReason, err := session.Prompt(r.Context(), prompt, func(u acp.Update) {
		switch u.Kind {
		case acp.KindAgentMessage:
			text.WriteString(u.Text)
		case acp.KindAgentThought:
			if thinking {
				thought.WriteString(u.Text)
			}
		}
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "agent failed: "+err.Error())
		return
	}

	content := make([]contentBlock, 0, 2)
	if thought.Len() > 0 {
		content = append(content, contentBlock{Type: "thinking", Thinking: thought.String()})
	}
	content = append(content, contentBlock{Type: "text", Text: text.String()})

	writeJSON(w, http.StatusOK, messageResponse{
		ID:         "msg_" + acp.RandomID(),
		Type:       "message",
		Role:       "assistant",
		Model:      req.Model,
		Content:    content,
		StopReason: stopReasonACPToAnthropic(stopReason),
		Usage:      usage{},
	})
}

func (s *Server) streamMessage(w http.ResponseWriter, r *http.Request, session *acp.Session, req messageRequest, prompt string, thinking bool) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	send := func(event string, payload any) {
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if flusher != nil {
			flusher.Flush()
		}
	}

	msg := messageResponse{
		ID: "msg_" + acp.RandomID(), Type: "message", Role: "assistant",
		Model: req.Model, StopReason: "", Content: []contentBlock{}, Usage: usage{},
	}
	send("message_start", map[string]any{"type": "message_start", "message": msg})

	// block tracks the currently open content block so thought and text
	// chunks land in separate blocks, like the real API.
	block := blockTracker{send: send, thinking: thinking}

	stopReason, err := session.Prompt(r.Context(), prompt, func(u acp.Update) {
		switch u.Kind {
		case acp.KindAgentMessage:
			block.text(u.Text)
		case acp.KindAgentThought:
			block.thought(u.Text)
		}
	})
	if err != nil {
		block.stop()
		send("error", map[string]any{"type": "error", "error": map[string]any{
			"type": "api_error", "message": "agent failed: " + err.Error()}})
		send("message_delta", map[string]any{"type": "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": 0}})
		send("message_stop", map[string]any{"type": "message_stop"})
		return
	}
	block.stop()

	send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stopReasonACPToAnthropic(stopReason), "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 0}})
	send("message_stop", map[string]any{"type": "message_stop"})
}

// blockTracker emits content_block_start/delta/stop events, switching
// between thinking and text blocks.
type blockTracker struct {
	send     func(event string, payload any)
	thinking bool
	index    int
	open     string // "", "thinking" or "text"
}

func (b *blockTracker) start(kind string) {
	b.send("content_block_start", map[string]any{
		"type": "content_block_start", "index": b.index,
		"content_block": map[string]any{"type": kind},
	})
	b.open = kind
}

func (b *blockTracker) stop() {
	if b.open == "" {
		return
	}
	if b.open == "thinking" {
		// The real API signs thinking blocks; the proxy cannot produce a
		// valid signature but emits the event to keep clients whole.
		b.send("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": b.index,
			"delta": map[string]any{"type": "signature_delta", "signature": ""},
		})
	}
	b.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": b.index})
	b.index++
	b.open = ""
}

func (b *blockTracker) thought(text string) {
	if !b.thinking {
		return
	}
	if b.open != "thinking" {
		b.stop()
		b.start("thinking")
	}
	b.send("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": b.index,
		"delta": map[string]any{"type": "thinking_delta", "thinking": text},
	})
}

func (b *blockTracker) text(text string) {
	if b.open != "text" {
		b.stop()
		b.start("text")
	}
	b.send("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": b.index,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (s *Server) countTokens(w http.ResponseWriter, r *http.Request) {
	var req messageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	prompt := buildPrompt(req)
	// Rough estimate: the proxy has no tokenizer, so approximate at four
	// characters per token.
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": len(prompt) / 4})
}

// stopReasonACPToAnthropic maps an ACP stop reason to an Anthropic
// stop_reason.
func stopReasonACPToAnthropic(stopReason string) string {
	switch stopReason {
	case acp.StopEndTurn, acp.StopCancelled:
		return "end_turn"
	case "max_tokens", "max_turn_requests":
		return "max_tokens"
	case "refusal":
		return "refusal"
	default:
		return "end_turn"
	}
}

// buildPrompt renders the Messages API request as a single ACP prompt
// text block: the system prompt, then the conversation transcript.
func buildPrompt(req messageRequest) string {
	var sb strings.Builder
	if len(req.System) > 0 {
		if sys := extractText(req.System); sys != "" {
			fmt.Fprintf(&sb, "[System instructions]\n%s\n\n", sys)
		}
	}
	for _, m := range req.Messages {
		text := extractText(m.Content)
		switch m.Role {
		case "user", "assistant":
			fmt.Fprintf(&sb, "%s: %s\n", strings.ToUpper(m.Role[:1])+m.Role[1:], text)
		default:
			fmt.Fprintf(&sb, "%s: %s\n", m.Role, text)
		}
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// extractText decodes a content field: plain string, or an array of
// content blocks (text, thinking, tool_use, tool_result are rendered).
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
		Content  json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "thinking":
			if b.Thinking != "" {
				parts = append(parts, b.Thinking)
			}
		case "tool_use":
			parts = append(parts, fmt.Sprintf("[tool_use: %s(%s)]", b.Name, string(b.Input)))
		case "tool_result":
			parts = append(parts, "[tool result] "+extractText(b.Content))
		}
	}
	return strings.Join(parts, "\n")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, typ, message string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": message},
	})
}
