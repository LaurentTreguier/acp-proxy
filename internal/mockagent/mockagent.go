// Package mockagent is a minimal ACP agent used by tests. It reads
// newline-delimited JSON-RPC 2.0 on stdin and writes it to stdout,
// implementing just enough of ACP to exercise the proxy's client.
//
// Behavior is selected with the ACP_MOCK_MODE environment variable:
//
//	plain      reply to session/prompt with a thought chunk, two message
//	           chunks, and stopReason "end_turn"
//	permission ask session/request_permission before answering; on
//	           "allow" answer "Hello ACP", on "cancelled" answer "denied"
//	hang       stream one chunk, then wait for session/cancel and finish
//	           with stopReason "cancelled"
package mockagent

import (
	"bufio"
	"encoding/json"
	"os"
	"time"
)

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
}

type updateBody struct {
	SessionUpdate string         `json:"sessionUpdate"`
	Content       map[string]any `json:"content"`
}

// Run executes the mock agent loop on stdin/stdout. It never returns.
func Run() {
	mode := os.Getenv("ACP_MOCK_MODE")
	if mode == "" {
		mode = "plain"
	}

	out := json.NewEncoder(os.Stdout)
	var nextID int64
	var promptID int64
	responses := make(chan *rpcMessage, 4)

	write := func(v any) { _ = out.Encode(v) }
	notify := func(method string, params any) {
		write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	}
	reply := func(id int64, result any) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	chunk := func(kind, text string) {
		notify("session/update", map[string]any{
			"sessionId": "sess-1",
			"update": updateBody{
				SessionUpdate: kind,
				Content:       map[string]any{"type": "text", "text": text},
			},
		})
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var msg rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		switch {
		case msg.Method == "initialize":
			reply(*msg.ID, map[string]any{
				"protocolVersion":    1,
				"agentCapabilities":  map[string]any{},
				"authMethods":        []any{},
			})
		case msg.Method == "session/new":
			reply(*msg.ID, map[string]any{"sessionId": "sess-1"})
		case msg.Method == "session/prompt":
			promptID = *msg.ID
			switch mode {
			case "plain":
				chunk("agent_thought_chunk", "thinking...")
				chunk("agent_message_chunk", "Hello ")
				chunk("agent_message_chunk", "ACP")
				reply(promptID, map[string]any{"stopReason": "end_turn"})
			case "permission":
				nextID++
				permID := nextID
				write(map[string]any{
					"jsonrpc": "2.0", "id": permID,
					"method": "session/request_permission",
					"params": map[string]any{
						"sessionId": "sess-1",
						"toolCall":  map[string]any{"toolCallId": "call-1"},
						"options": []map[string]any{
							{"optionId": "allow", "name": "Allow", "kind": "allow_once"},
							{"optionId": "deny", "name": "Deny", "kind": "reject_once"},
						},
					},
				})
				go func(promptID int64) {
					select {
					case res := <-responses:
						var body struct {
							Outcome struct {
								Outcome  string `json:"outcome"`
								OptionID string `json:"optionId"`
							} `json:"outcome"`
						}
						if json.Unmarshal(res.Result, &body) != nil {
							return
						}
						if body.Outcome.Outcome == "cancelled" {
							chunk("agent_message_chunk", "denied")
						} else {
							chunk("agent_message_chunk", "Hello ")
							chunk("agent_message_chunk", "ACP")
						}
						reply(promptID, map[string]any{"stopReason": "end_turn"})
					case <-time.After(10 * time.Second):
						os.Stderr.WriteString("mockagent: permission response timeout\n")
					}
				}(promptID)
			case "hang":
				chunk("agent_message_chunk", "partial")
			}
		case msg.Method == "session/cancel":
			if promptID != 0 {
				reply(promptID, map[string]any{"stopReason": "cancelled"})
				promptID = 0
			}
		case msg.Method == "" && msg.ID != nil:
			responses <- &msg
		}
	}
}
