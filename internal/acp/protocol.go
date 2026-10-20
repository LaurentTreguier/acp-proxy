package acp

import "encoding/json"

// ProtocolVersion is the ACP protocol version this client speaks.
const ProtocolVersion = 1

// JSON-RPC 2.0 error codes used by this client.
const (
	codeMethodNotFound = -32601
	codeInternalError  = -32603
)

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

// --- JSON-RPC envelope ---

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// inbound is a message received from the agent: a response to one of our
// requests (Result or Error set) or a request/notification (Method set).
type inbound struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *RPCError       `json:"error"`
}

// --- initialize ---

type fsCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

// clientCapabilities advertises nothing beyond the baseline: the proxy is
// headless and does not host files or terminals for the agent.
type clientCapabilities struct {
	FS       *fsCapabilities `json:"fs,omitempty"`
	Terminal *bool           `json:"terminal,omitempty"`
}

type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities clientCapabilities `json:"clientCapabilities"`
	ClientInfo         implementation     `json:"clientInfo"`
}

type initializeResult struct {
	ProtocolVersion int             `json:"protocolVersion"`
	AuthMethods     []string        `json:"authMethods"`
	Raw             json.RawMessage `json:"-"`
}

// --- session lifecycle ---

type newSessionParams struct {
	Cwd string `json:"cwd"`
}

type newSessionResult struct {
	SessionID string `json:"sessionId"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type promptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
}

type promptResult struct {
	StopReason string `json:"stopReason"`
}

type cancelParams struct {
	SessionID string `json:"sessionId"`
}

// --- session/update notifications ---

type sessionUpdateParams struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

type sessionUpdateBody struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       *contentBlock   `json:"content"`
	MessageID     string          `json:"messageId"`
	ToolCallID    string          `json:"toolCallId"`
	Title         string          `json:"title"`
	Status        string          `json:"status"`
	Raw           json.RawMessage `json:"-"`
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

// --- session/request_permission (agent -> client) ---

type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type requestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	Options   []permissionOption  `json:"options"`
	Raw       json.RawMessage     `json:"-"`
}

type selectedOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId"`
}

type requestPermissionResult struct {
	Outcome selectedOutcome `json:"outcome"`
}

func cancelledPermissionResult() requestPermissionResult {
	return requestPermissionResult{Outcome: selectedOutcome{Outcome: "cancelled"}}
}
