package openai

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"acp-proxy/internal/config"
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

func testServer(t *testing.T, mode, permissions string) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		Addr: "127.0.0.1:0",
		Agents: []config.AgentConfig{{
			Model:      "mock",
			Command:    os.Args[0],
			Args:       []string{"-test.run=TestMockAgentProcess"},
			Env:        map[string]string{"ACP_MOCK_AGENT": "1", "ACP_MOCK_MODE": mode},
			Permissions: permissions,
		}},
	}
	mux := http.NewServeMux()
	New(cfg).Register(mux)
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

func TestModels(t *testing.T) {
	srv := testServer(t, "plain", "cancel")
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Object != "list" || len(body.Data) != 1 || body.Data[0].ID != "mock" {
		t.Errorf("unexpected models response: %+v", body)
	}
}

func TestChatCompletionNonStreaming(t *testing.T) {
	srv := testServer(t, "plain", "cancel")
	defer srv.Close()

	resp, body := post(t, srv.URL+"/v1/chat/completions", map[string]any{
		"model":    "mock",
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var cc ChatCompletionResponse
	if err := json.Unmarshal([]byte(body), &cc); err != nil {
		t.Fatal(err)
	}
	if cc.Object != "chat.completion" || len(cc.Choices) != 1 {
		t.Errorf("unexpected completion: %s", body)
	}
	ch := cc.Choices[0]
	if ch.Message.Role != "assistant" || ch.Message.Content != "Hello ACP" {
		t.Errorf("message = %+v", ch.Message)
	}
	if ch.FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", ch.FinishReason)
	}
}

func TestChatCompletionStreaming(t *testing.T) {
	srv := testServer(t, "plain", "cancel")
	defer srv.Close()

	resp, body := post(t, srv.URL+"/v1/chat/completions", map[string]any{
		"model":    "mock",
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
		"stream":   true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}

	var content strings.Builder
	finish := ""
	role := ""
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			continue
		}
		var chunk ChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", data, err)
		}
		if chunk.Object != "chat.completion.chunk" || len(chunk.Choices) != 1 {
			t.Errorf("unexpected chunk: %s", data)
		}
		c := chunk.Choices[0]
		if c.Delta.Role != "" {
			role = c.Delta.Role
		}
		content.WriteString(c.Delta.Content)
		if c.FinishReason != "" {
			finish = c.FinishReason
		}
	}
	if role != "assistant" {
		t.Errorf("first chunk role = %q, want assistant", role)
	}
	if got := content.String(); got != "Hello ACP" {
		t.Errorf("streamed content = %q, want %q", got, "Hello ACP")
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q, want stop", finish)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("stream must end with [DONE]: %q", body)
	}
}

func TestUnknownModel(t *testing.T) {
	srv := testServer(t, "plain", "cancel")
	defer srv.Close()

	resp, body := post(t, srv.URL+"/v1/chat/completions", map[string]any{
		"model":    "missing",
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body = %s", resp.StatusCode, body)
	}
}

func TestPermissionAutoPolicy(t *testing.T) {
	srv := testServer(t, "permission", "auto")
	defer srv.Close()

	_, body := post(t, srv.URL+"/v1/chat/completions", map[string]any{
		"model":    "mock",
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	})
	var cc ChatCompletionResponse
	if err := json.Unmarshal([]byte(body), &cc); err != nil {
		t.Fatal(err)
	}
	if got := cc.Choices[0].Message.Content; got != "Hello ACP" {
		t.Errorf("content = %q, want Hello ACP (auto policy selects allow)", got)
	}
}

func TestPermissionCancelPolicy(t *testing.T) {
	srv := testServer(t, "permission", "cancel")
	defer srv.Close()

	_, body := post(t, srv.URL+"/v1/chat/completions", map[string]any{
		"model":    "mock",
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	})
	var cc ChatCompletionResponse
	if err := json.Unmarshal([]byte(body), &cc); err != nil {
		t.Fatal(err)
	}
	if got := cc.Choices[0].Message.Content; got != "denied" {
		t.Errorf("content = %q, want denied (default policy cancels permissions)", got)
	}
}

func TestBuildPrompt(t *testing.T) {
	messages := []Message{
		{Role: "system", Content: json.RawMessage(`"be brief"`)},
		{Role: "user", Content: json.RawMessage(`"first"`)},
		{Role: "assistant", Content: json.RawMessage(`[{"type":"text","text":"hi"},{"type":"image_url"}]`)},
		{Role: "user", Content: json.RawMessage(`"second"`)},
	}
	got := buildPrompt(messages)
	want := "[System instructions]\nbe brief\n\nUser: first\nAssistant: hi\nUser: second"
	if got != want {
		t.Errorf("buildPrompt = %q, want %q", got, want)
	}
}
