package anthropic

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"acp-proxy/internal/acp"
	"acp-proxy/internal/mockagent"
)

// TestMockAgentProcess is re-entered via exec: when ACP_MOCK_AGENT=1 the
// test binary becomes the mock ACP agent instead of running tests.
func TestMockAgentProcess(t *testing.T) {
	if os.Getenv("ACP_MOCK_AGENT") != "1" {
		return
	}
	mockagent.Run()
	os.Exit(0)
}

type fakeResolver struct{}

func (f *fakeResolver) List(ctx context.Context) ([]acp.AgentInfo, error) {
	return []acp.AgentInfo{{ID: "mock", Name: "Mock"}}, nil
}

func (f *fakeResolver) Resolve(ctx context.Context, id string) (acp.Agent, error) {
	if id != "mock" {
		return acp.Agent{}, &acp.UnknownAgentError{ID: id}
	}
	return acp.Agent{
		Command:     os.Args[0],
		Args:        []string{"-test.run=TestMockAgentProcess"},
		Permissions: os.Getenv("MOCK_PERMISSIONS"),
	}, nil
}

func testServer(t *testing.T, mode string) *httptest.Server {
	t.Helper()
	t.Setenv("ACP_MOCK_AGENT", "1")
	t.Setenv("ACP_MOCK_MODE", mode)
	t.Setenv("MOCK_PERMISSIONS", "cancel")
	pool := acp.NewPool()
	t.Cleanup(pool.Close)
	mux := http.NewServeMux()
	New(&fakeResolver{}, pool).Register(mux)
	return httptest.NewServer(mux)
}

func post(t *testing.T, url string, body any) (*http.Response, string) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(raw)
}

func messageBody(thinking bool) map[string]any {
	body := map[string]any{
		"model":      "claude-x",
		"max_tokens": 1024,
		"system":     "be brief",
		"messages": []map[string]any{
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "hi"}}},
			{"role": "user", "content": []map[string]any{{"type": "text", "text": "again"}}},
		},
	}
	if thinking {
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 1024}
	}
	return body
}

func TestMessagesNonStreaming(t *testing.T) {
	srv := testServer(t, "plain")
	defer srv.Close()

	resp, body := post(t, srv.URL+"/mock/v1/messages", messageBody(false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var msg messageResponse
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "message" || msg.Role != "assistant" {
		t.Errorf("unexpected envelope: %s", body)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" || msg.Content[0].Text != "Hello ACP" {
		t.Errorf("content = %+v", msg.Content)
	}
	if msg.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", msg.StopReason)
	}
	if msg.Usage.InputTokens != 0 || msg.Usage.OutputTokens != 0 {
		t.Errorf("usage = %+v, want zeros (no tokenizer)", msg.Usage)
	}
}

func TestMessagesThinkingEnabled(t *testing.T) {
	srv := testServer(t, "plain") // mock agent emits a thought chunk first
	defer srv.Close()

	_, body := post(t, srv.URL+"/mock/v1/messages", messageBody(true))
	var msg messageResponse
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatal(err)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("content blocks = %d, want thinking + text, body = %s", len(msg.Content), body)
	}
	if msg.Content[0].Type != "thinking" || msg.Content[0].Thinking != "thinking..." {
		t.Errorf("thinking block = %+v", msg.Content[0])
	}
	if msg.Content[1].Type != "text" || msg.Content[1].Text != "Hello ACP" {
		t.Errorf("text block = %+v", msg.Content[1])
	}
}

func TestMessagesThinkingIgnoredWhenNotRequested(t *testing.T) {
	srv := testServer(t, "plain")
	defer srv.Close()

	_, body := post(t, srv.URL+"/mock/v1/messages", messageBody(false))
	var msg messageResponse
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatal(err)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" {
		t.Errorf("thoughts must be dropped without thinking enabled: %s", body)
	}
}

func TestMessagesStreaming(t *testing.T) {
	srv := testServer(t, "plain")
	defer srv.Close()

	resp, body := post(t, srv.URL+"/mock/v1/messages", func() map[string]any {
		b := messageBody(true)
		b["stream"] = true
		return b
	}())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}

	var events []string
	var data []string
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
		if strings.HasPrefix(line, "data: ") {
			data = append(data, strings.TrimPrefix(line, "data: "))
		}
	}
	want := []string{"message_start", "content_block_start", "content_block_delta",
		"content_block_delta", "content_block_stop", "content_block_start",
		"content_block_delta", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v", events)
	}
	// First block thinking, second text.
	var thinkingText, text strings.Builder
	for _, d := range data {
		var ev struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Delta struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				Thinking   string `json:"thinking"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(d), &ev); err != nil {
			t.Fatalf("bad event data %q: %v", d, err)
		}
		switch {
		case ev.Type == "content_block_delta" && ev.Delta.Type == "thinking_delta":
			thinkingText.WriteString(ev.Delta.Thinking)
		case ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta":
			text.WriteString(ev.Delta.Text)
		case ev.Type == "message_delta":
			if ev.Delta.StopReason != "end_turn" {
				t.Errorf("stop_reason = %q", ev.Delta.StopReason)
			}
		}
	}
	if thinkingText.String() != "thinking..." {
		t.Errorf("thinking stream = %q", thinkingText.String())
	}
	if text.String() != "Hello ACP" {
		t.Errorf("text stream = %q", text.String())
	}
}

func TestMessagesValidation(t *testing.T) {
	srv := testServer(t, "plain")
	defer srv.Close()

	// max_tokens is required.
	resp, body := post(t, srv.URL+"/mock/v1/messages", map[string]any{
		"model": "claude-x", "messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "max_tokens") {
		t.Errorf("status = %d, body = %s", resp.StatusCode, body)
	}

	// Unprefixed endpoint is rejected.
	resp, body = post(t, srv.URL+"/v1/messages", messageBody(false))
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "path prefix") {
		t.Errorf("status = %d, body = %s", resp.StatusCode, body)
	}

	// Unknown agent.
	resp, _ = post(t, srv.URL+"/missing/v1/messages", messageBody(false))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown agent status = %d, want 404", resp.StatusCode)
	}
}

func TestCountTokens(t *testing.T) {
	srv := testServer(t, "plain")
	defer srv.Close()

	resp, body := post(t, srv.URL+"/mock/v1/messages/count_tokens", messageBody(false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.InputTokens <= 0 {
		t.Errorf("input_tokens = %d, want a rough estimate", out.InputTokens)
	}
}

func TestBuildPrompt(t *testing.T) {
	sys, _ := json.Marshal([]map[string]any{{"type": "text", "text": "be brief"}})
	req := messageRequest{
		System: sys,
		Messages: []messageParam{
			{Role: "user", Content: json.RawMessage(`"first"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"hi"}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","content":"42"}]`)},
		},
	}
	got := buildPrompt(req)
	want := "[System instructions]\nbe brief\n\nUser: first\nAssistant: hi\nUser: [tool result] 42"
	if got != want {
		t.Errorf("buildPrompt = %q, want %q", got, want)
	}
}
