package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
	session, prior, err := s.responsesSession(w, r, agentID, agent, req.PreviousResponse)
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
	if !prior.live() && prior.Response != nil {
		prompt = renderTranscript(prior.Transcript) + "\n\n" + prompt
	}

	if req.Stream {
		s.streamResponse(w, r, session, req, agentID, prompt, store, prior)
		return
	}
	s.blockingResponse(w, r, session, req, agentID, prompt, store, prior)
}

// responsesSession returns the session to run on and the stored record
// of the previous response (zero value for fresh requests): the live
// session for a warm follow-up, a fresh pooled session for a cold one
// (found only on disk), or a fresh session for a new conversation.
func (s *Server) responsesSession(w http.ResponseWriter, r *http.Request, agentID string, agent acp.Agent, previousID string) (*acp.Session, storedResponse, error) {
	if previousID != "" {
		stored, ok := s.respStore.get(previousID)
		if !ok {
			writeError(w, http.StatusNotFound, "invalid_request_error",
				fmt.Sprintf("No response found with ID '%s'.", previousID))
			return nil, storedResponse{}, errors.New("not found")
		}
		if stored.Session != nil {
			return stored.Session, stored, nil
		}
		// Cold follow-up: replay on a fresh session.
		session, err := s.pool.Session(r.Context(), agentID, agent)
		if err != nil {
			writeError(w, http.StatusBadGateway, "server_error", err.Error())
			return nil, stored, err
		}
		return session, stored, nil
	}
	session, err := s.pool.Session(r.Context(), agentID, agent)
	if err != nil {
		writeError(w, http.StatusBadGateway, "server_error", err.Error())
		return nil, storedResponse{}, err
	}
	return session, storedResponse{}, nil
}

func (s *Server) blockingResponse(w http.ResponseWriter, r *http.Request, session *acp.Session, req responsesRequest, agentID, prompt string, store bool, prior storedResponse) {
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

func (s *Server) streamResponse(w http.ResponseWriter, r *http.Request, session *acp.Session, req responsesRequest, agentID, prompt string, store bool, prior storedResponse) {
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

// followUpOf reports whether the record belongs to an existing
// conversation (its instructions are already in the transcript).
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
