package openai

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
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
		{"role": "user", "content": "first"},
		{"role": "assistant", "content": "hi"},
		{"type": "function_call_output", "call_id": "call_1", "output": "42"},
		{"role": "user", "content": "second"},
	})
	got := renderResponsesInput("sys prompt", items)
	want := "[System instructions]\nsys prompt\n\nSystem: be brief\nUser: first\nAssistant: hi\n[tool result for call_1] 42\nUser: second"
	if got != want {
		t.Errorf("render = %q, want %q", got, want)
	}

	s, _ := json.Marshal("just a string")
	if got, want := renderResponsesInput("", s), "User: just a string"; got != want {
		t.Errorf("string input = %q, want %q", got, want)
	}
}
