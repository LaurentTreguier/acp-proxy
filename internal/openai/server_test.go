package openai

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

// fakeResolver implements Resolver with a single "mock" agent that
// re-execs the test binary as the mock ACP agent.
type fakeResolver struct{}

func (f *fakeResolver) List(ctx context.Context) ([]AgentInfo, error) {
	return []AgentInfo{{ID: "mock", Name: "Mock", Version: "1.0", Description: "mock agent"}}, nil
}

func (f *fakeResolver) Resolve(ctx context.Context, id string) (acp.Agent, error) {
	if id != "mock" {
		return acp.Agent{}, &UnknownAgentError{ID: id}
	}
	return acp.Agent{
		Command:     os.Args[0],
		Args:        []string{"-test.run=TestMockAgentProcess"},
		Permissions: os.Getenv("MOCK_PERMISSIONS"),
	}, nil
}

func testServer(t *testing.T, mode, permissions string) *httptest.Server {
	t.Helper()
	t.Setenv("ACP_MOCK_AGENT", "1")
	t.Setenv("ACP_MOCK_MODE", mode)
	t.Setenv("MOCK_PERMISSIONS", permissions)
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

func chatBody(model string, stream bool) map[string]any {
	return map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
		"stream":   stream,
	}
}

func TestAgentsListing(t *testing.T) {
	srv := testServer(t, "plain", "cancel")
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(body), "mock\n"; got != want {
		t.Errorf("agents listing = %q, want %q", got, want)
	}

	// The per-agent models endpoint remains OpenAI-shaped.
	resp2, err := http.Get(srv.URL + "/mock/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var body2 struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&body2); err != nil {
		t.Fatal(err)
	}
	if len(body2.Data) != 1 || body2.Data[0].ID != "mock" {
		t.Errorf("unexpected prefixed models response: %+v", body2)
	}

	// Unknown prefix -> 404.
	resp3, err := http.Get(srv.URL + "/nope/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Errorf("unknown agent models status = %d, want 404", resp3.StatusCode)
	}
}

func TestChatCompletionNonStreaming(t *testing.T) {
	srv := testServer(t, "plain", "cancel")
	defer srv.Close()

	resp, body := post(t, srv.URL+"/mock/v1/chat/completions", chatBody("mock", false))
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

func TestChatCompletionUnprefixedRejected(t *testing.T) {
	srv := testServer(t, "plain", "cancel")
	defer srv.Close()

	// The unprefixed endpoint is not supported; requests must select an
	// agent through the path prefix.
	resp, body := post(t, srv.URL+"/v1/chat/completions", chatBody("mock", false))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body = %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "path prefix") {
		t.Errorf("error should point at the prefix requirement: %s", body)
	}
}

func TestChatCompletionStreaming(t *testing.T) {
	srv := testServer(t, "plain", "cancel")
	defer srv.Close()

	resp, body := post(t, srv.URL+"/mock/v1/chat/completions", chatBody("mock", true))
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

func TestUnknownAgent(t *testing.T) {
	srv := testServer(t, "plain", "cancel")
	defer srv.Close()

	resp, body := post(t, srv.URL+"/missing/v1/chat/completions", chatBody("missing", false))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404, body = %s", resp.StatusCode, body)
	}
}

func TestPermissionAutoPolicy(t *testing.T) {
	srv := testServer(t, "permission", "auto")
	defer srv.Close()

	_, body := post(t, srv.URL+"/mock/v1/chat/completions", chatBody("mock", false))
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

	_, body := post(t, srv.URL+"/mock/v1/chat/completions", chatBody("mock", false))
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
