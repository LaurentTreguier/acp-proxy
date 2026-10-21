package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"acp-proxy/internal/acp"
)

// responsesRequest is the subset of the Responses API request the proxy
// consumes.
type responsesRequest struct {
	Model            string          `json:"model"`
	Input            json.RawMessage `json:"input"`
	Instructions     string          `json:"instructions"`
	Stream           bool            `json:"stream"`
	Store            *bool           `json:"store,omitempty"`
	PreviousResponse string          `json:"previous_response_id,omitempty"`
}

// responses types the proxy produces.

type outputText struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type outputItem struct {
	Type    string       `json:"type"`
	ID      string       `json:"id"`
	Role    string       `json:"role"`
	Status  string       `json:"status"`
	Content []outputText `json:"content,omitempty"`
}

type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type incompleteDetails struct {
	Reason string `json:"reason"`
}

type responsesResponse struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	CreatedAt         int64              `json:"created_at"`
	Status            string             `json:"status"`
	Error            any                `json:"error"`
	IncompleteDetails *incompleteDetails `json:"incomplete_details"`
	Instructions     any                `json:"instructions"`
	MaxOutputTokens  any                `json:"max_output_tokens"`
	Model            string             `json:"model"`
	Output           []outputItem       `json:"output"`
	OutputText       string             `json:"output_text"`
	Usage            responsesUsage     `json:"usage"`
}

// responseStore keeps the ACP session and final response for each
// created response, enabling previous_response_id follow-ups. Entries
// are capped; the oldest are evicted and their sessions closed.
type responseStore struct {
	mu    sync.Mutex
	order []string
	items map[string]storedResponse
	limit int
}

type storedResponse struct {
	agentID  string
	session *acp.Session
	response *responsesResponse
}

func newResponseStore(limit int) *responseStore {
	return &responseStore{items: make(map[string]storedResponse), limit: limit}
}

func (s *responseStore) get(id string) (storedResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[id]
	return item, ok
}

func (s *responseStore) put(id string, item storedResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[id] = item
	s.order = append(s.order, id)
	for len(s.order) > s.limit {
		oldest := s.order[0]
		s.order = s.order[1:]
		if evicted, ok := s.items[oldest]; ok {
			evicted.session.Close()
			delete(s.items, oldest)
		}
	}
}

// responses handles POST /{agent}/v1/responses. Statefulness:
// previous_response_id resumes the stored ACP session for the
// referenced response; responses are stored in memory unless the request
// sets store=false.
func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	var req responsesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	agentID := r.PathValue("agent")
	if len(req.Input) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "input: Field required")
		return
	}

	agent, err := s.resolver.Resolve(r.Context(), agentID)
	if err != nil {
		s.writeResolveError(w, err)
		return
	}

	store := req.Store == nil || *req.Store
	session, followUp, err := s.responsesSession(w, r, agentID, agent, req.PreviousResponse)
	if err != nil {
		return // response already written
	}

	prompt, hasContent := renderResponsesInput(req.Instructions, req.Input)
	if !hasContent {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"input contains no renderable text content")
		return
	}

	if req.Stream {
		s.streamResponse(w, r, session, req, agentID, prompt, store, followUp)
		return
	}
	s.blockingResponse(w, r, session, req, agentID, prompt, store, followUp)
}

// responsesSession returns the session to run on: the stored session of
// a previous response (follow-up), or a fresh pooled session.
func (s *Server) responsesSession(w http.ResponseWriter, r *http.Request, agentID string, agent acp.Agent, previousID string) (*acp.Session, bool, error) {
	if previousID != "" {
		stored, ok := s.respStore.get(previousID)
		if !ok {
			writeError(w, http.StatusNotFound, "invalid_request_error",
				fmt.Sprintf("No response found with ID '%s'.", previousID))
			return nil, false, errors.New("not found")
		}
		return stored.session, true, nil
	}
	session, err := s.pool.Session(r.Context(), agentID, agent)
	if err != nil {
		writeError(w, http.StatusBadGateway, "server_error", err.Error())
		return nil, false, err
	}
	return session, false, nil
}

func (s *Server) blockingResponse(w http.ResponseWriter, r *http.Request, session *acp.Session, req responsesRequest, agentID, prompt string, store, followUp bool) {
	var text strings.Builder
	stopReason, err := session.Prompt(r.Context(), prompt, func(u acp.Update) {
		if u.Kind == acp.KindAgentMessage {
			text.WriteString(u.Text)
		}
	})
	if err != nil {
		if !followUp {
			session.Close()
		}
		writeError(w, http.StatusBadGateway, "server_error", "agent failed: "+err.Error())
		return
	}

	resp := newResponsesResponse(req)
	resp.OutputText = text.String()
	msg := newOutputMessage()
	msg.Content = []outputText{{Type: "output_text", Text: text.String(), Annotations: []any{}}}
	resp.Output = []outputItem{msg}
	switch stopReason {
	case "max_tokens", "max_turn_requests":
		resp.Status = "incomplete"
		resp.IncompleteDetails = &incompleteDetails{Reason: "max_output_tokens"}
	}

	s.finishResponses(resp, session, agentID, store, followUp)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) streamResponse(w http.ResponseWriter, r *http.Request, session *acp.Session, req responsesRequest, agentID, prompt string, store, followUp bool) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	seq := 0
	send := func(event string, payload map[string]any) {
		payload["type"] = event
		payload["sequence_number"] = seq
		seq++
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if flusher != nil {
			flusher.Flush()
		}
	}

	resp := newResponsesResponse(req)
	resp.Status = "in_progress"
	send("response.created", map[string]any{"response": resp})

	item := newOutputMessage()
	send("response.output_item.added", map[string]any{
		"output_index": 0, "item": inProgressMessage(item)})
	send("response.content_part.added", map[string]any{
		"item_id": item.ID, "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})

	var text strings.Builder
	stopReason, err := session.Prompt(r.Context(), prompt, func(u acp.Update) {
		if u.Kind != acp.KindAgentMessage || u.Text == "" {
			return
		}
		text.WriteString(u.Text)
		send("response.output_text.delta", map[string]any{
			"item_id": item.ID, "output_index": 0, "content_index": 0, "delta": u.Text})
	})

	send("response.output_text.done", map[string]any{
		"item_id": item.ID, "output_index": 0, "content_index": 0, "text": text.String()})
	send("response.content_part.done", map[string]any{
		"item_id": item.ID, "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text.String(), "annotations": []any{}}})

	item.Content = []outputText{{Type: "output_text", Text: text.String(), Annotations: []any{}}}
	item.Status = "completed"
	send("response.output_item.done", map[string]any{"output_index": 0, "item": item})

	resp.Status = "completed"
	switch stopReason {
	case "max_tokens", "max_turn_requests":
		resp.Status = "incomplete"
		resp.IncompleteDetails = &incompleteDetails{Reason: "max_output_tokens"}
	}
	resp.OutputText = text.String()
	resp.Output = []outputItem{item}

	if err != nil {
		s.finishResponses(resp, session, agentID, false, followUp)
		send("response.failed", map[string]any{
			"response": failedResponse(resp, "agent failed: "+err.Error())})
		return
	}
	s.finishResponses(resp, session, agentID, store, followUp)
	if resp.Status == "incomplete" {
		send("response.incomplete", map[string]any{"response": resp})
	} else {
		send("response.completed", map[string]any{"response": resp})
	}
}

// finishResponses stores (or closes) the session backing a response.
func (s *Server) finishResponses(resp responsesResponse, session *acp.Session, agentID string, store, followUp bool) {
	if store {
		s.respStore.put(resp.ID, storedResponse{
			agentID: agentID, session: session, response: &resp,
		})
		return
	}
	if !followUp {
		session.Close()
	}
}

// getResponse handles GET /{agent}/v1/responses/{id}: returns a stored
// response.
func (s *Server) getResponse(w http.ResponseWriter, r *http.Request) {
	stored, ok := s.respStore.get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "invalid_request_error",
			fmt.Sprintf("No response found with ID '%s'.", r.PathValue("id")))
		return
	}
	writeJSON(w, http.StatusOK, stored.response)
}

func newResponsesResponse(req responsesRequest) responsesResponse {
	return responsesResponse{
		ID:           "resp_" + RandomID(),
		Object:       "response",
		CreatedAt:    time.Now().Unix(),
		Status:       "completed",
		Error:        nil,
		Instructions: nilIfEmpty(req.Instructions),
		Model:        req.Model,
	}
}

func newOutputMessage() outputItem {
	return outputItem{
		Type:   "message",
		ID:     "msg_" + RandomID(),
		Role:   "assistant",
		Status: "completed",
	}
}

func inProgressMessage(item outputItem) outputItem {
	item.Status = "in_progress"
	item.Content = nil
	return item
}

func failedResponse(resp responsesResponse, message string) responsesResponse {
	resp.Status = "failed"
	resp.Error = map[string]any{"code": "agent_error", "message": message}
	return resp
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// renderResponsesInput turns the Responses input field into a prompt
// transcript: instructions first, then the input items in order. The
// second return reports whether any text content was found; requests
// without renderable content are rejected by the caller instead of
// reaching the agent as an empty conversation.
func renderResponsesInput(instructions string, input json.RawMessage) (string, bool) {
	var sb strings.Builder
	if instructions != "" {
		fmt.Fprintf(&sb, "[System instructions]\n%s\n\n", instructions)
	}
	if len(input) == 0 {
		return strings.TrimSuffix(sb.String(), "\n"), instructions != ""
	}
	var s string
	if err := json.Unmarshal(input, &s); err == nil {
		fmt.Fprintf(&sb, "User: %s\n", s)
		return strings.TrimSuffix(sb.String(), "\n"), true
	}
	// Input items: messages with a role, or raw items like
	// function_call_output.
	var items []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		CallID  string          `json:"call_id"`
		Output  string          `json:"output"`
	}
	if err := json.Unmarshal(input, &items); err != nil {
		return strings.TrimSuffix(sb.String(), "\n"), instructions != ""
	}
	hasContent := instructions != ""
	for _, it := range items {
		switch {
		case it.Type == "function_call_output":
			fmt.Fprintf(&sb, "[tool result for %s] %s\n", it.CallID, it.Output)
			hasContent = true
		case it.Role != "":
			text := extractText(it.Content)
			fmt.Fprintf(&sb, "%s: %s\n", roleLabel(it.Role), text)
			if text != "" {
				hasContent = true
			}
		}
	}
	return strings.TrimSuffix(sb.String(), "\n"), hasContent
}

func roleLabel(role string) string {
	switch role {
	case "system", "developer":
		return "System"
	case "user", "assistant":
		return strings.ToUpper(role[:1]) + role[1:]
	default:
		return role
	}
}
