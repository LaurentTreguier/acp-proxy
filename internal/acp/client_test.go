package acp

import (
	"context"
	"os"
	"testing"
	"time"

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

func mockAgent(mode, permissions string) Agent {
	return Agent{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestMockAgentProcess"},
		Env:     []string{"ACP_MOCK_AGENT=1", "ACP_MOCK_MODE=" + mode},
		Permissions: permissions,
	}
}

func TestPromptEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, err := Start(ctx, mockAgent("plain", "cancel"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	if s.SessionID() != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", s.SessionID())
	}

	var texts, thoughts []string
	stop, err := s.Prompt(ctx, "hello", func(u Update) {
		switch u.Kind {
		case "agent_message_chunk":
			texts = append(texts, u.Text)
		case "agent_thought_chunk":
			thoughts = append(thoughts, u.Text)
		}
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if stop != StopEndTurn {
		t.Errorf("stop reason = %q, want %q", stop, StopEndTurn)
	}
	if got, want := len(texts), 2; got != want {
		t.Errorf("got %d message chunks, want %d", got, want)
	}
	if len(texts) == 2 && texts[0]+texts[1] != "Hello ACP" {
		t.Errorf("joined chunks = %q", texts[0]+texts[1])
	}
	if len(thoughts) != 1 || thoughts[0] != "thinking..." {
		t.Errorf("thought chunks = %v", thoughts)
	}
}

func TestPermissionAuto(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, err := Start(ctx, mockAgent("permission", "auto"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	var sb textCollector
	stop, err := s.Prompt(ctx, "hello", sb.append)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if stop != StopEndTurn {
		t.Errorf("stop reason = %q, want %q", stop, StopEndTurn)
	}
	if got := sb.String(); got != "Hello ACP" {
		t.Errorf("content = %q, want %q", got, "Hello ACP")
	}
}

func TestPermissionCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, err := Start(ctx, mockAgent("permission", "cancel"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	var sb textCollector
	stop, err := s.Prompt(ctx, "hello", sb.append)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if stop != StopEndTurn {
		t.Errorf("stop reason = %q, want %q", stop, StopEndTurn)
	}
	if got := sb.String(); got != "denied" {
		t.Errorf("content = %q, want %q (default policy cancels permissions)", got, "denied")
	}
}

func TestContextCancellation(t *testing.T) {
	s, err := Start(context.Background(), mockAgent("hang", "cancel"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	stop, err := s.Prompt(ctx, "hello", func(Update) {})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if stop != StopCancelled {
		t.Errorf("stop reason = %q, want %q", stop, StopCancelled)
	}
}

type textCollector struct{ b []byte }

func (c *textCollector) append(u Update) {
	if u.Kind == "agent_message_chunk" {
		c.b = append(c.b, u.Text...)
	}
}

func (c *textCollector) String() string { return string(c.b) }
