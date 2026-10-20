// Package acp adapts the coder/acp-go-sdk to the needs of the proxy: it
// spawns an ACP agent process, runs prompt turns and streams session
// updates to a callback.
package acp

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"

	sdk "github.com/coder/acp-go-sdk"
)

// ClientInfo identifies this proxy to agents.
var ClientInfo = sdk.Implementation{Name: "acp-proxy", Version: "0.1.0"}

// Stop reasons reported by Prompt.
const (
	StopEndTurn   = string(sdk.StopReasonEndTurn)
	StopCancelled = string(sdk.StopReasonCancelled)
)

// Update kinds surfaced to the proxy.
const (
	KindAgentMessage = "agent_message_chunk"
	KindAgentThought = "agent_thought_chunk"
	KindToolCall     = "tool_call"
	KindToolCallUpd  = "tool_call_update"
	KindPlan         = "plan"
	KindUserMessage  = "user_message_chunk"
)

// Agent describes how to spawn an ACP agent.
type Agent struct {
	Command     string
	Args        []string
	Dir         string   // working directory; "" means the server's cwd
	Env         []string // extra KEY=VALUE entries layered over the inherited environment
	Permissions string   // "cancel" (default) or "auto"; see package config
}

// Update is a decoded session/update notification, pre-filtered to the
// fields the proxy consumes.
type Update struct {
	// Kind is the sessionUpdate discriminator
	// (agent_message_chunk, agent_thought_chunk, tool_call, ...).
	Kind string
	// Text carries the content text for message/thought chunks.
	Text string
	// MessageID groups chunks belonging to the same message.
	MessageID string
	// ToolCallID identifies tool_call / tool_call_update updates.
	ToolCallID string
	// Status is the tool call status for tool_call* updates.
	Status string
}

// Session is a live agent process with one initialized ACP session.
type Session struct {
	conn      *sdk.ClientSideConnection
	process   *exec.Cmd
	client    *clientImpl
	sessionID string
	closeOnce sync.Once
}

// clientImpl implements sdk.Client: the headless proxy answers permission
// requests according to the configured policy and rejects fs/terminal
// requests.
type clientImpl struct {
	mu          sync.Mutex
	permissions string
	onUpdate    func(Update)
}

var _ sdk.Client = (*clientImpl)(nil)

func (c *clientImpl) RequestPermission(ctx context.Context, params sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	if c.permissions == "auto" {
		for _, opt := range params.Options {
			if opt.Kind == sdk.PermissionOptionKindAllowOnce ||
				opt.Kind == sdk.PermissionOptionKindAllowAlways {
				return selected(opt.OptionId), nil
			}
		}
		if len(params.Options) > 0 {
			return selected(params.Options[0].OptionId), nil
		}
	}
	return sdk.RequestPermissionResponse{
		Outcome: sdk.RequestPermissionOutcome{
			Cancelled: &sdk.RequestPermissionOutcomeCancelled{},
		},
	}, nil
}

func selected(optionID sdk.PermissionOptionId) sdk.RequestPermissionResponse {
	return sdk.RequestPermissionResponse{
		Outcome: sdk.RequestPermissionOutcome{
			Selected: &sdk.RequestPermissionOutcomeSelected{OptionId: optionID},
		},
	}
}

func (c *clientImpl) SessionUpdate(ctx context.Context, params sdk.SessionNotification) error {
	c.mu.Lock()
	onUpdate := c.onUpdate
	c.mu.Unlock()
	if onUpdate != nil {
		onUpdate(decodeUpdate(params))
	}
	return nil
}

func (c *clientImpl) ReadTextFile(ctx context.Context, params sdk.ReadTextFileRequest) (sdk.ReadTextFileResponse, error) {
	return sdk.ReadTextFileResponse{}, sdk.NewMethodNotFound("fs/read_text_file")
}

func (c *clientImpl) WriteTextFile(ctx context.Context, params sdk.WriteTextFileRequest) (sdk.WriteTextFileResponse, error) {
	return sdk.WriteTextFileResponse{}, sdk.NewMethodNotFound("fs/write_text_file")
}

func (c *clientImpl) CreateTerminal(ctx context.Context, params sdk.CreateTerminalRequest) (sdk.CreateTerminalResponse, error) {
	return sdk.CreateTerminalResponse{}, sdk.NewMethodNotFound("terminal/create")
}

func (c *clientImpl) KillTerminal(ctx context.Context, params sdk.KillTerminalRequest) (sdk.KillTerminalResponse, error) {
	return sdk.KillTerminalResponse{}, sdk.NewMethodNotFound("terminal/kill")
}

func (c *clientImpl) TerminalOutput(ctx context.Context, params sdk.TerminalOutputRequest) (sdk.TerminalOutputResponse, error) {
	return sdk.TerminalOutputResponse{}, sdk.NewMethodNotFound("terminal/output")
}

func (c *clientImpl) ReleaseTerminal(ctx context.Context, params sdk.ReleaseTerminalRequest) (sdk.ReleaseTerminalResponse, error) {
	return sdk.ReleaseTerminalResponse{}, sdk.NewMethodNotFound("terminal/release")
}

func (c *clientImpl) WaitForTerminalExit(ctx context.Context, params sdk.WaitForTerminalExitRequest) (sdk.WaitForTerminalExitResponse, error) {
	return sdk.WaitForTerminalExitResponse{}, sdk.NewMethodNotFound("terminal/wait_for_exit")
}

// decodeUpdate flattens an SDK session notification into an Update.
func decodeUpdate(n sdk.SessionNotification) Update {
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		return chunkUpdate(KindAgentMessage, u.AgentMessageChunk.Content, u.AgentMessageChunk.MessageId)
	case u.AgentThoughtChunk != nil:
		return chunkUpdate(KindAgentThought, u.AgentThoughtChunk.Content, u.AgentThoughtChunk.MessageId)
	case u.UserMessageChunk != nil:
		return chunkUpdate(KindUserMessage, u.UserMessageChunk.Content, u.UserMessageChunk.MessageId)
	case u.ToolCall != nil:
		return Update{
			Kind:      KindToolCall,
			ToolCallID: string(u.ToolCall.ToolCallId),
			Status:    string(u.ToolCall.Status),
		}
	case u.ToolCallUpdate != nil:
		upd := Update{
			Kind:      KindToolCallUpd,
			ToolCallID: string(u.ToolCallUpdate.ToolCallId),
		}
		if u.ToolCallUpdate.Status != nil {
			upd.Status = string(*u.ToolCallUpdate.Status)
		}
		return upd
	case u.Plan != nil:
		return Update{Kind: KindPlan}
	default:
		return Update{}
	}
}

func chunkUpdate(kind string, content sdk.ContentBlock, messageID *string) Update {
	upd := Update{Kind: kind}
	if content.Text != nil {
		upd.Text = content.Text.Text
	}
	if messageID != nil {
		upd.MessageID = *messageID
	}
	return upd
}

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

	s := &Session{client: &clientImpl{permissions: agent.Permissions}}
	s.conn = sdk.NewClientSideConnection(s.client, stdin, stdout)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp: start agent %s: %w", agent.Command, err)
	}
	s.process = cmd
	go io.Copy(io.Discard, stderr)

	initResp, err := s.conn.Initialize(ctx, sdk.InitializeRequest{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		ClientInfo:      &ClientInfo,
	})
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("acp: initialize: %w", err)
	}
	if initResp.ProtocolVersion != sdk.ProtocolVersionNumber {
		s.Close()
		return nil, fmt.Errorf("acp: agent speaks protocol version %d, client supports %d",
			initResp.ProtocolVersion, sdk.ProtocolVersionNumber)
	}

	newResp, err := s.conn.NewSession(ctx, sdk.NewSessionRequest{Cwd: cwd})
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("acp: session/new: %w", err)
	}
	s.sessionID = string(newResp.SessionId)
	return s, nil
}

// SessionID returns the ACP session identifier.
func (s *Session) SessionID() string { return s.sessionID }

// Prompt sends the prompt text and blocks until the agent finishes the
// turn, streaming session updates to onUpdate (which may be nil). It
// returns the stop reason.
//
// When ctx is cancelled, the SDK sends session/cancel and Prompt returns
// StopCancelled.
func (s *Session) Prompt(ctx context.Context, promptText string, onUpdate func(Update)) (string, error) {
	s.client.mu.Lock()
	s.client.onUpdate = onUpdate
	s.client.mu.Unlock()

	resp, err := s.conn.Prompt(ctx, sdk.PromptRequest{
		SessionId: sdk.SessionId(s.sessionID),
		Prompt:    []sdk.ContentBlock{sdk.TextBlock(promptText)},
	})
	if err != nil {
		if ctx.Err() != nil {
			return StopCancelled, nil
		}
		return "", fmt.Errorf("acp: session/prompt: %w", err)
	}
	return string(resp.StopReason), nil
}

// Close tears down the agent process.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		if s.process != nil && s.process.Process != nil {
			_ = s.process.Process.Kill()
			go func() { _ = s.process.Wait() }()
		}
	})
}
