package openai

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"acp-proxy/internal/acp"
)

func responsesServer(t *testing.T, mode string) *testHarness {
	t.Helper()
	return newTestHarness(t, mode)
}

func TestResponsesNonStreaming(t *testing.T) {
	h := responsesServer(t, "plain")
	defer h.Close()

	resp, body := h.post(t, "/mock/v1/responses", map[string]any{
		"model":        "gpt-x",
		"instructions": "be brief",
		"input": []map[string]any{
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "hi"},
			{"role": "user", "content": "again"},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var r responsesResponse
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	if r.Object != "response" || r.Status != "completed" {
		t.Errorf("envelope: %s", body)
	}
	if r.OutputText != "Hello ACP" {
		t.Errorf("output_text = %q", r.OutputText)
	}
	if len(r.Output) != 1 || r.Output[0].Type != "message" || r.Output[0].Role != "assistant" {
		t.Errorf("output items = %+v", r.Output)
	}
	if len(r.Output[0].Content) != 1 || r.Output[0].Content[0].Type != "output_text" {
		t.Errorf("content = %+v", r.Output[0].Content)
	}
	if r.ID == "" {
		t.Error("response id missing")
	}

	// The response must be retrievable and support follow-ups.
	resp2, body2 := h.get(t, "/mock/v1/responses/"+r.ID)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("retrieve status = %d, body = %s", resp2.StatusCode, body2)
	}
	var r2 responsesResponse
	if err := json.Unmarshal([]byte(body2), &r2); err != nil {
		t.Fatal(err)
	}
	if r2.ID != r.ID {
		t.Errorf("retrieved id = %q, want %q", r2.ID, r.ID)
	}

	// Follow-up via previous_response_id reuses the stored session.
	followUp := map[string]any{
		"model":                "gpt-x",
		"input":                "and then?",
		"previous_response_id": r.ID,
	}
	resp3, body3 := h.post(t, "/mock/v1/responses", followUp)
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("follow-up status = %d, body = %s", resp3.StatusCode, body3)
	}
	var r3 responsesResponse
	if err := json.Unmarshal([]byte(body3), &r3); err != nil {
		t.Fatal(err)
	}
	if r3.ID == r.ID {
		t.Error("follow-up must create a new response id")
	}
	if r3.OutputText != "Hello ACP" {
		t.Errorf("follow-up output_text = %q", r3.OutputText)
	}
}

func TestResponsesPreviousUnknown(t *testing.T) {
	h := responsesServer(t, "plain")
	defer h.Close()

	resp, body := h.post(t, "/mock/v1/responses", map[string]any{
		"model": "gpt-x", "input": "hello", "previous_response_id": "resp_nope",
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, body = %s", resp.StatusCode, body)
	}
}

// TestResponsesSurvivesRestart simulates a proxy restart: a response is
// created, the pool (and every ACP session) dies, and a new server with
// the same persistence directory continues the conversation by replaying
// the stored transcript.
func TestResponsesSurvivesRestart(t *testing.T) {
	t.Setenv("ACP_MOCK_AGENT", "1")
	t.Setenv("ACP_MOCK_MODE", "plain")
	t.Setenv("MOCK_PERMISSIONS", "cancel")

	dir := t.TempDir()

	poolA := acp.NewPool()
	muxA := http.NewServeMux()
	New(&fakeResolver{}, poolA, dir).Register(muxA)
	srvA := httptest.NewServer(muxA)

	_, body := post(t, srvA.URL+"/mock/v1/responses", map[string]any{
		"model": "gpt-x", "instructions": "be brief", "input": "hello",
	})
	srvA.Close()
	poolA.Close() // kills the agent process and its sessions, like a restart

	var first responsesResponse
	if err := json.Unmarshal([]byte(body), &first); err != nil {
		t.Fatal(err)
	}

	// A brand-new server (fresh pool, same responses dir) continues the
	// conversation from the previous_response_id.
	poolB := acp.NewPool()
	defer poolB.Close()
	serverB := New(&fakeResolver{}, poolB, dir)
	muxB := http.NewServeMux()
	serverB.Register(muxB)
	srvB := httptest.NewServer(muxB)
	defer srvB.Close()

	resp, body := post(t, srvB.URL+"/mock/v1/responses", map[string]any{
		"model": "gpt-x", "input": "and then?",
		"previous_response_id": first.ID,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-restart follow-up status = %d, body = %s", resp.StatusCode, body)
	}
	var second responsesResponse
	if err := json.Unmarshal([]byte(body), &second); err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Error("follow-up must create a new response id")
	}
	if second.OutputText != "Hello ACP" {
		t.Errorf("post-restart output_text = %q", second.OutputText)
	}

	// The follow-up's own record must carry the accumulated transcript,
	// including the pre-restart turns.
	stored, ok := serverB.respStore.get(second.ID)
	if !ok {
		t.Fatal("second response must be stored")
	}
	var wantRoles []string
	for _, tr := range stored.Transcript {
		wantRoles = append(wantRoles, tr.Role)
	}
	if len(stored.Transcript) < 4 {
		t.Fatalf("transcript = %+v, want system + user + assistant + new user + new assistant", stored.Transcript)
	}
	if stored.Transcript[0].Role != "system" || stored.Transcript[0].Text != "be brief" {
		t.Errorf("first turn = %+v, want stored instructions", stored.Transcript[0])
	}
	if last := stored.Transcript[len(stored.Transcript)-1]; last.Role != "assistant" || last.Text != "Hello ACP" {
		t.Errorf("last turn = %+v, want assistant reply", last)
	}
}

func TestResponsesStoreFalse(t *testing.T) {
	h := responsesServer(t, "plain")
	defer h.Close()

	resp, body := h.post(t, "/mock/v1/responses", map[string]any{
		"model": "gpt-x", "input": "hello", "store": false,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var r responsesResponse
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	resp2, _ := h.get(t, "/mock/v1/responses/"+r.ID)
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("store=false must not persist; got %d", resp2.StatusCode)
	}
}

func TestResponsesStreaming(t *testing.T) {
	h := responsesServer(t, "plain")
	defer h.Close()

	resp, body := h.post(t, "/mock/v1/responses", map[string]any{
		"model": "gpt-x", "input": "hello", "stream": true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}

	var events []string
	var delta strings.Builder
	terminal := ""
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type   string         `json:"type"`
			Delta  string         `json:"delta"`
			Text   string         `json:"text"`
			Item   *outputItem    `json:"item"`
			Response *responsesResponse `json:"response"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("bad event: %v", err)
		}
		switch ev.Type {
		case "response.output_text.delta":
			delta.WriteString(ev.Delta)
		case "response.completed":
			terminal = "completed"
			if ev.Response == nil || ev.Response.OutputText != "Hello ACP" {
				t.Errorf("completed response = %+v", ev.Response)
			}
		case "response.incomplete", "response.failed":
			terminal = ev.Type
		}
	}
	wantPrefix := []string{"response.created", "response.output_item.added",
		"response.content_part.added", "response.output_text.delta"}
	if len(events) < len(wantPrefix) {
		t.Fatalf("events = %v", events)
	}
	for i, want := range wantPrefix {
		if events[i] != want {
			t.Errorf("event[%d] = %q, want %q", i, events[i], want)
		}
	}
	if delta.String() != "Hello ACP" {
		t.Errorf("delta text = %q", delta.String())
	}
	if terminal != "completed" {
		t.Errorf("terminal event = %q", terminal)
	}
}

func TestResponsesInputString(t *testing.T) {
	h := responsesServer(t, "plain")
	defer h.Close()

	_, body := h.post(t, "/mock/v1/responses", map[string]any{
		"model": "gpt-x", "input": "just a string",
	})
	var r responsesResponse
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatal(err)
	}
	if r.OutputText != "Hello ACP" {
		t.Errorf("output_text = %q", r.OutputText)
	}
}

func TestRenderResponsesInput(t *testing.T) {
	items, _ := json.Marshal([]map[string]any{
		{"role": "system", "content": "be brief"},
		{"role": "user", "content": []map[string]any{{"type": "input_text", "text": "first"}}},
		{"role": "assistant", "content": []map[string]any{{"type": "output_text", "text": "hi"}}},
		{"type": "function_call_output", "call_id": "call_1", "output": "42"},
		{"role": "user", "content": []map[string]any{{"type": "input_text", "text": "second"}}},
	})
	got, hasContent := renderResponsesInput("sys prompt", items)
	want := "[System instructions]\nsys prompt\n\nSystem: be brief\nUser: first\nAssistant: hi\n[tool result for call_1] 42\nUser: second"
	if got != want {
		t.Errorf("render = %q, want %q", got, want)
	}
	if !hasContent {
		t.Error("expected content to be detected")
	}

	s, _ := json.Marshal("just a string")
	got, hasContent = renderResponsesInput("", s)
	if got != "User: just a string" || !hasContent {
		t.Errorf("string input = %q, hasContent = %v", got, hasContent)
	}

	// Chat-completions-style text parts keep working.
	chat, _ := json.Marshal([]map[string]any{{"type": "text", "text": "legacy"}})
	if got := extractText(chat); got != "legacy" {
		t.Errorf("text part = %q", got)
	}

	// Non-text parts alone are rejected with hasContent=false.
	empty, _ := json.Marshal([]map[string]any{
		{"role": "user", "content": []map[string]any{{"type": "input_image", "image_url": "..."}}},
	})
	_, hasContent = renderResponsesInput("", empty)
	if hasContent {
		t.Error("image-only input must not count as content")
	}
}

func TestResponsesRejectsContentlessInput(t *testing.T) {
	h := responsesServer(t, "plain")
	defer h.Close()

	resp, body := h.post(t, "/mock/v1/responses", map[string]any{
		"model": "gpt-x",
		"input": []map[string]any{
			{"role": "user", "content": []map[string]any{{"type": "input_image", "image_url": "..."}}},
		},
	})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "no renderable text") {
		t.Errorf("status = %d, body = %s", resp.StatusCode, body)
	}
}
