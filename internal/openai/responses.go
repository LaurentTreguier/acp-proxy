package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
	Error             any                `json:"error"`
	IncompleteDetails *incompleteDetails `json:"incomplete_details"`
	Instructions      any                `json:"instructions"`
	MaxOutputTokens   any                `json:"max_output_tokens"`
	Model             string             `json:"model"`
	Output            []outputItem       `json:"output"`
	OutputText        string             `json:"output_text"`
	Usage             responsesUsage     `json:"usage"`
}

// turn is one conversation entry in a stored transcript.
type turn struct {
	Role string `json:"role"` // user, assistant, system, tool
	Text string `json:"text"`
}

// responseStore keeps the ACP session and final response for each
// created response, enabling previous_response_id follow-ups. Live
// entries are held in memory (capped, oldest evicted); every entry is
// also written to disk so follow-ups survive a proxy restart — replayed
// into a fresh ACP session instead of the original one.
type responseStore struct {
	mu    sync.Mutex
	order []string
	items map[string]storedResponse
	limit int
	// dir is the on-disk persistence directory; empty disables disk
	// persistence.
	dir string
}

type storedResponse struct {
	AgentID    string             `json:"agent_id"`
	Session    *acp.Session       `json:"-"` // nil after a restart
	Response   *responsesResponse `json:"response"`
	Transcript []turn             `json:"transcript"`
}

func newResponseStore(limit int, dir string) *responseStore {
	return &responseStore{items: make(map[string]storedResponse), limit: limit, dir: dir}
}

// get returns the stored response: from memory when the session is
// still live, otherwise from disk (with a nil session, marking a cold
// follow-up).
func (s *responseStore) get(id string) (storedResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item, ok := s.items[id]; ok {
		return item, true
	}
	return s.readFromDisk(id)
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
			if evicted.Session != nil {
				evicted.Session.Close()
			}
			delete(s.items, oldest)
		}
	}
	s.writeToDisk(id, item)
}

func (s *responseStore) readFromDisk(id string) (storedResponse, bool) {
	if s.dir == "" {
		return storedResponse{}, false
	}
	data, err := os.ReadFile(s.diskPath(id))
	if err != nil {
		return storedResponse{}, false
	}
	var rec storedResponse
	if err := json.Unmarshal(data, &rec); err != nil {
		return storedResponse{}, false
	}
	return rec, true
}

func (s *responseStore) writeToDisk(id string, item storedResponse) {
	if s.dir == "" {
		return
	}
	data, err := json.Marshal(item)
	if err != nil {
		return
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(s.diskPath(id), data, 0o644)
}

func (s *responseStore) diskPath(id string) string {
	// Response ids are proxy-generated hex; sanitize anyway.
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return -1
		}
	}, id)
	return filepath.Join(s.dir, safe+".json")
}

// responses handles POST /{agent}/v1/responses. Statefulness:
// previous_response_id resumes the stored ACP session for the
// referenced response when it is still live; after a proxy restart the
// stored transcript is replayed into a fresh session. Responses are held
// in memory (and on disk) unless the request sets store=false.
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
	session, prior, followUp, err := s.responsesSession(w, r, agentID, agent, req.PreviousResponse)
	if err != nil {
		return // response already written
	}

	prompt, hasContent := renderResponsesInput(req.Instructions, req.Input)
	if !hasContent {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"input contains no renderable text content")
		return
	}
	// Cold follow-up: the previous ACP session died with a restart; the
	// stored transcript carries the conversation into a fresh session.
	if prior.Session == nil && prior.Response != nil {
		prompt = renderTranscript(prior.Transcript) + "\n\n" + prompt
	}

	if req.Stream {
		s.streamResponse(w, r, session, req, agentID, prompt, store, followUp, prior)
		return
	}
	s.blockingResponse(w, r, session, req, agentID, prompt, store, followUp, prior)
}

// responsesSession returns the session to run on and the stored record
// of the previous response (zero for fresh requests): the live session
// for a warm follow-up, a fresh pooled session for a cold one (found
// only on disk), or a fresh session for a new conversation.
func (s *Server) responsesSession(w http.ResponseWriter, r *http.Request, agentID string, agent acp.Agent, previousID string) (*acp.Session, storedResponse, bool, error) {
	if previousID != "" {
		stored, ok := s.respStore.get(previousID)
		if !ok {
			writeError(w, http.StatusNotFound, "invalid_request_error",
				fmt.Sprintf("No response found with ID '%s'.", previousID))
			return nil, storedResponse{}, false, errors.New("not found")
		}
		if stored.Session != nil {
			return stored.Session, stored, true, nil
		}
		// Cold follow-up: replay on a fresh session.
		session, err := s.pool.Session(r.Context(), agentID, agent)
		if err != nil {
			writeError(w, http.StatusBadGateway, "server_error", err.Error())
			return nil, stored, false, err
		}
		return session, stored, true, nil
	}
	session, err := s.pool.Session(r.Context(), agentID, agent)
	if err != nil {
		writeError(w, http.StatusBadGateway, "server_error", err.Error())
		return nil, storedResponse{}, false, err
	}
	return session, storedResponse{}, false, nil
}

func (s *Server) blockingResponse(w http.ResponseWriter, r *http.Request, session *acp.Session, req responsesRequest, agentID, prompt string, store, followUp bool, prior storedResponse) {
	var text strings.Builder
	stopReason, err := session.Prompt(r.Context(), prompt, func(u acp.Update) {
		if u.Kind == acp.KindAgentMessage {
			text.WriteString(u.Text)
		}
	})
	if err != nil {
		if !prior.live() {
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

	s.finishResponses(resp, session, agentID, store, prior, req, text.String())
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) streamResponse(w http.ResponseWriter, r *http.Request, session *acp.Session, req responsesRequest, agentID, prompt string, store, followUp bool, prior storedResponse) {
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
		s.finishResponses(resp, session, agentID, false, prior, req, text.String())
		send("response.failed", map[string]any{
			"response": failedResponse(resp, "agent failed: "+err.Error())})
		return
	}
	s.finishResponses(resp, session, agentID, store, prior, req, text.String())
	if resp.Status == "incomplete" {
		send("response.incomplete", map[string]any{"response": resp})
	} else {
		send("response.completed", map[string]any{"response": resp})
	}
}

// finishResponses stores (or closes) the session backing a response.
func (s *Server) finishResponses(resp responsesResponse, session *acp.Session, agentID string, store bool, prior storedResponse, req responsesRequest, reply string) {
	if store {
		// Accumulate the conversation transcript: the prior chain plus
		// this turn's input and the agent's reply.
		transcript := make([]turn, 0, len(prior.Transcript)+3)
		transcript = append(transcript, prior.Transcript...)
		if !followUpOf(prior) {
			if req.Instructions != "" {
				transcript = append(transcript, turn{Role: "system", Text: req.Instructions})
			}
		}
		transcript = append(transcript, turnsFromInput(req.Input)...)
		transcript = append(transcript, turn{Role: "assistant", Text: reply})

		s.respStore.put(resp.ID, storedResponse{
			AgentID:    agentID,
			Session:    session,
			Response:   &resp,
			Transcript: transcript,
		})
		return
	}
	if session != nil && !prior.live() {
		session.Close()
	}
}

// live reports whether the stored record carries a live ACP session.
func (s storedResponse) live() bool { return s.Session != nil }

func followUpOf(prior storedResponse) bool { return prior.Response != nil }

// getResponse handles GET /{agent}/v1/responses/{id}: returns a stored
// response.
func (s *Server) getResponse(w http.ResponseWriter, r *http.Request) {
	stored, ok := s.respStore.get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "invalid_request_error",
			fmt.Sprintf("No response found with ID '%s'.", r.PathValue("id")))
		return
	}
	writeJSON(w, http.StatusOK, stored.Response)
}

func newResponsesResponse(req responsesRequest) responsesResponse {
	return responsesResponse{
		ID:           "resp_" + acp.RandomID(),
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
		ID:     "msg_" + acp.RandomID(),
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

// turnsFromInput extracts conversation turns from a Responses input
// field, for transcript persistence.
func turnsFromInput(input json.RawMessage) []turn {
	var s string
	if err := json.Unmarshal(input, &s); err == nil {
		if s != "" {
			return []turn{{Role: "user", Text: s}}
		}
		return nil
	}
	var items []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		CallID  string          `json:"call_id"`
		Output  string          `json:"output"`
	}
	if err := json.Unmarshal(input, &items); err != nil {
		return nil
	}
	var turns []turn
	for _, it := range items {
		switch {
		case it.Type == "function_call_output":
			turns = append(turns, turn{Role: "tool", Text: it.Output})
		case it.Role != "":
			turns = append(turns, turn{Role: it.Role, Text: extractText(it.Content)})
		}
	}
	return turns
}

// renderTranscript renders stored turns back into a prompt, for replaying
// a conversation into a fresh ACP session after a restart.
func renderTranscript(turns []turn) string {
	var sb strings.Builder
	for _, t := range turns {
		switch t.Role {
		case "system":
			fmt.Fprintf(&sb, "[System instructions]\n%s\n\n", t.Text)
		case "tool":
			fmt.Fprintf(&sb, "[tool result] %s\n", t.Text)
		default:
			fmt.Fprintf(&sb, "%s: %s\n", roleLabel(t.Role), t.Text)
		}
	}
	return strings.TrimSuffix(sb.String(), "\n")
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
