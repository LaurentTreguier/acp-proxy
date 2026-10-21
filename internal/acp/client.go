// Package acp is the proxy's agent domain. A Pool keeps at most one live
// process per ACP agent, spawned lazily on first use and kept for the
// lifetime of the proxy; every request opens a cheap ACP session on the
// agent's pooled process. The package also defines the agent-domain
// contract shared by the API packages and the registry: AgentInfo,
// Resolver and UnknownAgentError.
package acp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"time"

	sdk "github.com/coder/acp-go-sdk"
)

// ClientInfo identifies this proxy to agents.
var ClientInfo = sdk.Implementation{Name: "acp-proxy", Version: "0.1.0"}

// Stop reasons reported by Prompt.
const (
	StopEndTurn   = string(sdk.StopReasonEndTurn)
	StopCancelled = string(sdk.StopReasonCancelled)
)

// sessionCloseTimeout bounds the best-effort session/close call.
const sessionCloseTimeout = 10 * time.Second

// Agent describes how to spawn an ACP agent.
type Agent struct {
	// ID identifies the agent in logs and pool bookkeeping.
	ID string
	// Command is the executable to spawn as the ACP agent.
	Command string
	// Args are the command-line arguments passed to the agent.
	Args []string
	// Dir is the working directory; "" means the server's cwd.
	Dir string
	// Env lists extra KEY=VALUE entries layered over the inherited
	// environment.
	Env []string
	// Permissions is "cancel" (default) or "auto"; see package config.
	Permissions string
}

// Update is a decoded session/update notification, pre-filtered to the
// fields the proxy consumes.
type Update struct {
	Kind       string
	Text       string
	MessageID  string
	ToolCallID string
	Status     string
}

// logStderr surfaces agent stderr lines in the proxy log, prefixed with
// the agent id. Agents log diagnostics (auth failures, startup errors)
// on stderr; without this their deaths are silent.
func logStderr(r io.Reader, id string) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if id != "" {
			log.Printf("acp[%s] stderr: %s", id, line)
		} else {
			log.Printf("acp stderr: %s", line)
		}
	}
}

// Pool lazily spawns agent processes and reuses them. At most one
// process per agent id exists at a time; dead processes are replaced on
// the next request.
type Pool struct {
	mu    sync.Mutex
	procs map[string]*Process
}

// NewPool creates an empty pool.
func NewPool() *Pool {
	return &Pool{procs: make(map[string]*Process)}
}

// Session returns a new ACP session on the agent's pooled process,
// spawning the process on first use. Process lifetime is independent of
// ctx; only the session handshake uses it.
func (p *Pool) Session(ctx context.Context, id string, agent Agent) (*Session, error) {
	proc, err := p.get(ctx, id, agent)
	if err != nil {
		return nil, err
	}
	sess, err := proc.NewSession(ctx)
	if err == nil {
		return sess, nil
	}
	// The process may have died between the health check and the
	// handshake; replace it and retry once.
	p.mu.Lock()
	if p.procs[id] == proc {
		delete(p.procs, id)
	}
	p.mu.Unlock()
	proc.Close()
	if proc, err = p.get(ctx, id, agent); err != nil {
		return nil, err
	}
	return proc.NewSession(ctx)
}

// get returns the live process for id, spawning it if needed.
func (p *Pool) get(ctx context.Context, id string, agent Agent) (*Process, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if proc, ok := p.procs[id]; ok {
		if !proc.dead() {
			return proc, nil
		}
		proc.Close()
		delete(p.procs, id)
	}
	// The process outlives the request context.
	proc, err := spawn(context.Background(), agent)
	if err != nil {
		return nil, err
	}
	p.procs[id] = proc
	return proc, nil
}

// Close terminates every pooled process.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, proc := range p.procs {
		proc.Close()
		delete(p.procs, id)
	}
}

// Process is one spawned ACP agent with its SDK connection.
type Process struct {
	conn    *sdk.ClientSideConnection
	process *exec.Cmd
	client  *clientImpl
	cwd     string
	// closeSupported reports whether the agent advertised session/close
	// support in its initialize response.
	closeSupported bool

	closeOnce sync.Once
}

// spawn starts the agent process and performs the initialize handshake.
func spawn(ctx context.Context, agent Agent) (*Process, error) {
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

	p := &Process{
		client: &clientImpl{
			permissions: agent.Permissions,
			handlers:    make(map[string]func(Update)),
		},
		cwd: cwd,
	}
	p.conn = sdk.NewClientSideConnection(p.client, stdin, stdout)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp: start agent %s: %w", agent.Command, err)
	}
	p.process = cmd
	go logStderr(stderr, agent.ID)

	initResp, err := p.conn.Initialize(ctx, sdk.InitializeRequest{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		ClientInfo:      &ClientInfo,
	})
	if err != nil {
		p.Close()
		return nil, fmt.Errorf("acp: initialize: %w", err)
	}
	if initResp.ProtocolVersion != sdk.ProtocolVersionNumber {
		p.Close()
		return nil, fmt.Errorf("acp: agent speaks protocol version %d, client supports %d",
			initResp.ProtocolVersion, sdk.ProtocolVersionNumber)
	}
	p.closeSupported = initResp.AgentCapabilities.SessionCapabilities.Close != nil
	return p, nil
}

// NewSession opens a fresh ACP session on the process.
func (p *Process) NewSession(ctx context.Context) (*Session, error) {
	// An explicit empty list, not nil: several agents validate
	// mcpServers as a list and reject JSON null.
	resp, err := p.conn.NewSession(ctx, sdk.NewSessionRequest{
		Cwd:        p.cwd,
		McpServers: []sdk.McpServer{},
	})
	if err != nil {
		return nil, fmt.Errorf("acp: session/new: %w", err)
	}
	return &Session{proc: p, conn: p.conn, sessionID: string(resp.SessionId)}, nil
}

// dead reports whether the agent process has disconnected.
func (p *Process) dead() bool {
	select {
	case <-p.conn.Done():
		return true
	default:
		return false
	}
}

// Close terminates the agent process.
func (p *Process) Close() {
	p.closeOnce.Do(func() {
		if p.process != nil && p.process.Process != nil {
			_ = p.process.Process.Kill()
			go func() { _ = p.process.Wait() }()
		}
	})
}

// Session is one ACP conversation on a pooled agent process. Sessions
// are cheap; the underlying process is shared and pooled.
type Session struct {
	proc      *Process
	conn      *sdk.ClientSideConnection
	sessionID string
}

// SessionID returns the ACP session identifier.
func (s *Session) SessionID() string { return s.sessionID }

// Close releases the session on the agent. It is best-effort: it only
// sends session/close when the agent advertised support for it, and
// failures are ignored — the pooled agent process keeps running. The
// call does not outlive a short timeout, so it never hangs a request.
func (s *Session) Close() {
	if !s.proc.closeSupported {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionCloseTimeout)
	defer cancel()
	_, _ = s.conn.CloseSession(ctx, sdk.CloseSessionRequest{
		SessionId: sdk.SessionId(s.sessionID),
	})
}

// Prompt sends the prompt text and blocks until the agent finishes the
// turn, streaming session updates to onUpdate (which may be nil). It
// returns the stop reason. Updates are routed to this session only.
//
// When ctx is cancelled, the SDK sends session/cancel and Prompt returns
// StopCancelled.
func (s *Session) Prompt(ctx context.Context, promptText string, onUpdate func(Update)) (string, error) {
	s.proc.client.setHandler(s.sessionID, onUpdate)
	defer s.proc.client.removeHandler(s.sessionID)

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

// clientImpl implements sdk.Client for a process: the headless proxy
// answers permission requests according to the configured policy,
// rejects fs/terminal requests and routes session updates to the
// session that subscribed to them.
type clientImpl struct {
	permissions string
	mu          sync.Mutex
	handlers    map[string]func(Update)
}

var _ sdk.Client = (*clientImpl)(nil)

func (c *clientImpl) setHandler(sessionID string, fn func(Update)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if fn != nil {
		c.handlers[sessionID] = fn
	}
}

func (c *clientImpl) removeHandler(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.handlers, sessionID)
}

func (c *clientImpl) SessionUpdate(ctx context.Context, params sdk.SessionNotification) error {
	c.mu.Lock()
	fn := c.handlers[string(params.SessionId)]
	c.mu.Unlock()
	if fn != nil {
		fn(decodeUpdate(params))
	}
	return nil
}

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
