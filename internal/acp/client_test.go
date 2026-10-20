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
		Command:     os.Args[0],
		Args:        []string{"-test.run=TestMockAgentProcess"},
		Env:         []string{"ACP_MOCK_AGENT=1", "ACP_MOCK_MODE=" + mode},
		Permissions: permissions,
	}
}

// session opens a pooled session for a fresh pool.
func session(t *testing.T, mode, permissions string) (*Pool, *Session) {
	t.Helper()
	pool := NewPool()
	sess, err := pool.Session(context.Background(), "mock", mockAgent(mode, permissions))
	if err != nil {
		pool.Close()
		t.Fatalf("Session: %v", err)
	}
	t.Cleanup(pool.Close)
	if sess.SessionID() != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", sess.SessionID())
	}
	return pool, sess
}

func promptText(t *testing.T, sess *Session) string {
	t.Helper()
	var sb textCollector
	stop, err := sess.Prompt(context.Background(), "hello", sb.append)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if stop != StopEndTurn {
		t.Errorf("stop reason = %q, want %q", stop, StopEndTurn)
	}
	return sb.String()
}

func TestPromptEndToEnd(t *testing.T) {
	_, sess := session(t, "plain", "cancel")

	var texts, thoughts []string
	stop, err := sess.Prompt(context.Background(), "hello", func(u Update) {
		switch u.Kind {
		case KindAgentMessage:
			texts = append(texts, u.Text)
		case KindAgentThought:
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

func TestPoolReusesProcess(t *testing.T) {
	pool, _ := session(t, "plain", "cancel")

	sess1, err := pool.Session(context.Background(), "mock", mockAgent("plain", "cancel"))
	if err != nil {
		t.Fatal(err)
	}
	sess2, err := pool.Session(context.Background(), "mock", mockAgent("plain", "cancel"))
	if err != nil {
		t.Fatal(err)
	}
	if got := promptText(t, sess1); got != "Hello ACP" {
		t.Errorf("sess1 content = %q", got)
	}
	if got := promptText(t, sess2); got != "Hello ACP" {
		t.Errorf("sess2 content = %q", got)
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.procs) != 1 {
		t.Errorf("pool holds %d processes, want 1 (one per agent)", len(pool.procs))
	}
}

func TestPoolRespawnsDeadProcess(t *testing.T) {
	pool, sess := session(t, "plain", "cancel")
	if got := promptText(t, sess); got != "Hello ACP" {
		t.Fatalf("first turn content = %q", got)
	}

	// Kill the pooled process behind the pool's back.
	pool.mu.Lock()
	proc := pool.procs["mock"]
	pool.mu.Unlock()
	proc.Close()

	sess2, err := pool.Session(context.Background(), "mock", mockAgent("plain", "cancel"))
	if err != nil {
		t.Fatalf("Session after agent death: %v", err)
	}
	if got := promptText(t, sess2); got != "Hello ACP" {
		t.Errorf("respawned turn content = %q, want Hello ACP", got)
	}
}

func TestPoolRespawnsAfterCrash(t *testing.T) {
	pool, _ := session(t, "plain", "cancel")
	pool.mu.Lock()
	proc := pool.procs["mock"]
	pool.mu.Unlock()

	// The pool evicts a process that dies mid-handshake.
	proc.process.Process.Kill()
	time.Sleep(100 * time.Millisecond)

	sess2, err := pool.Session(context.Background(), "mock", mockAgent("plain", "cancel"))
	if err != nil {
		t.Fatalf("Session after crash: %v", err)
	}
	if got := promptText(t, sess2); got != "Hello ACP" {
		t.Errorf("content after respawn = %q", got)
	}
}

func TestPermissionAuto(t *testing.T) {
	_, sess := session(t, "permission", "auto")
	if got := promptText(t, sess); got != "Hello ACP" {
		t.Errorf("content = %q, want Hello ACP (auto policy selects allow)", got)
	}
}

func TestPermissionCancel(t *testing.T) {
	_, sess := session(t, "permission", "cancel")
	if got := promptText(t, sess); got != "denied" {
		t.Errorf("content = %q, want denied (default policy cancels permissions)", got)
	}
}

func TestContextCancellation(t *testing.T) {
	_, sess := session(t, "hang", "cancel")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	stop, err := sess.Prompt(ctx, "hello", func(Update) {})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if stop != StopCancelled {
		t.Errorf("stop reason = %q, want %q", stop, StopCancelled)
	}
}

type textCollector struct{ b []byte }

func (c *textCollector) append(u Update) {
	if u.Kind == KindAgentMessage {
		c.b = append(c.b, u.Text...)
	}
}

func (c *textCollector) String() string { return string(c.b) }
