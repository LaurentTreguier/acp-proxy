// Package acp implements a minimal client for the Agent Client Protocol
// (ACP): JSON-RPC 2.0 over the stdin/stdout of a spawned agent process.
package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// ClientInfo identifies this proxy to agents.
var ClientInfo = implementation{Name: "acp-proxy", Version: "0.1.0"}

// Stop reasons reported by Prompt.
const (
	StopEndTurn   = "end_turn"
	StopCancelled = "cancelled"
)

// Agent describes how to spawn an ACP agent.
type Agent struct {
	Command     string
	Args        []string
	Dir         string // working directory; "" means the server's cwd
	Env         []string // extra KEY=VALUE entries layered over the inherited environment
	Permissions string   // "cancel" (default) or "auto"; see package config
}

// Session is a live agent process with one initialized ACP session.
type Session struct {
	conn        *conn
	process     *exec.Cmd
	sessionID   string
	permissions string
}

// promptCancelGrace is how long Prompt waits for the agent to acknowledge
// session/cancel before giving up on the turn.
const promptCancelGrace = 5 * time.Second

// Start spawns the agent, performs the initialize handshake and creates a
// new session. The returned Session owns the process; call Close when done.
func Start(ctx context.Context, agent Agent) (*Session, error) {
	cwd := agent.Dir
	if cwd == "" {
		cwd = "."
	}

	cmd := exec.CommandContext(ctx, agent.Command, agent.Args...)
	cmd.Dir = cwd
	if len(agent.Env) > 0 {
		cmd.Env = append(cmd.Environ(), agent.Env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: agent stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: agent stdout: %w", err)
	}
	// Drain stderr so a chatty agent cannot block on a full pipe.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: agent stderr: %w", err)
	}

	s := &Session{permissions: agent.Permissions}
	s.conn = newConn(stdout, stdin, clientHandler{
		onRequest: s.handleClientRequest,
		onUpdate:  func(Update) {}, // replaced by Prompt
	})
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp: start agent %s: %w", agent.Command, err)
	}
	s.process = cmd
	go io.Copy(io.Discard, stderr)

	var initResult initializeResult
	if err := s.conn.call("initialize", initializeParams{
		ProtocolVersion:    ProtocolVersion,
		ClientCapabilities: clientCapabilities{},
		ClientInfo:         ClientInfo,
	}, &initResult); err != nil {
		s.Close()
		return nil, err
	}
	if initResult.ProtocolVersion != ProtocolVersion {
		s.Close()
		return nil, fmt.Errorf("acp: agent speaks protocol version %d, client supports %d",
			initResult.ProtocolVersion, ProtocolVersion)
	}

	var newResult newSessionResult
	if err := s.conn.call("session/new", newSessionParams{Cwd: cwd}, &newResult); err != nil {
		s.Close()
		return nil, err
	}
	s.sessionID = newResult.SessionID
	return s, nil
}

// SessionID returns the ACP session identifier.
func (s *Session) SessionID() string { return s.sessionID }

// Prompt sends the prompt text and blocks until the agent finishes the
// turn, streaming session updates to onUpdate (which may be nil). It
// returns the stop reason.
//
// When ctx is cancelled, Prompt sends session/cancel, waits up to
// promptCancelGrace for the agent to end the turn, and returns
// StopCancelled otherwise.
func (s *Session) Prompt(ctx context.Context, promptText string, onUpdate func(Update)) (string, error) {
	s.conn.handler.onUpdate = onUpdate

	promptDone := make(chan promptResult, 1)
	promptErr := make(chan error, 1)
	go func() {
		var result promptResult
		err := s.conn.call("session/prompt", promptParams{
			SessionID: s.sessionID,
			Prompt:    []contentBlock{{Type: "text", Text: promptText}},
		}, &result)
		if err != nil {
			promptErr <- err
			return
		}
		promptDone <- result
	}()

	if ctx.Done() == nil {
		// A background context: wait without cancellation support.
		select {
		case res := <-promptDone:
			return res.StopReason, nil
		case err := <-promptErr:
			return "", err
		}
	}

	select {
	case res := <-promptDone:
		return res.StopReason, nil
	case err := <-promptErr:
		return "", err
	case <-ctx.Done():
		_ = s.conn.notify("session/cancel", cancelParams{SessionID: s.sessionID})
		timer := time.NewTimer(promptCancelGrace)
		defer timer.Stop()
		select {
		case res := <-promptDone:
			return res.StopReason, nil
		case err := <-promptErr:
			return "", err
		case <-timer.C:
			return StopCancelled, nil
		}
	}
}

// Close tears down the agent process.
func (s *Session) Close() {
	if s.process != nil && s.process.Process != nil {
		_ = s.process.Process.Kill()
		go func() { _ = s.process.Wait() }()
	}
	s.conn.failAll(errConnClosed)
}

// handleClientRequest answers agent->client requests.
func (s *Session) handleClientRequest(method string, params json.RawMessage) (any, error) {
	switch method {
	case "session/request_permission":
		var req requestPermissionParams
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, &RPCError{Code: codeInternalError, Message: "invalid params"}
		}
		if s.permissions == "auto" {
			if optionID, ok := selectAllowOption(req.Options); ok {
				return requestPermissionResult{Outcome: selectedOutcome{
					Outcome:  "selected",
					OptionID: optionID,
				}}, nil
			}
		}
		return cancelledPermissionResult(), nil
	case "fs/read_text_file", "fs/write_text_file",
		"terminal/create", "terminal/output", "terminal/release",
		"terminal/wait_for_exit", "terminal/kill", "elicitation/create":
		return nil, &RPCError{Code: codeMethodNotFound, Message: "method not supported"}
	default:
		return nil, &RPCError{Code: codeMethodNotFound, Message: "method not supported"}
	}
}

// selectAllowOption picks the first allow_* permission option, falling
// back to the first option.
func selectAllowOption(options []permissionOption) (string, bool) {
	if len(options) == 0 {
		return "", false
	}
	for _, o := range options {
		if o.Kind == "allow_once" || o.Kind == "allow_always" {
			return o.OptionID, true
		}
	}
	return options[0].OptionID, true
}
