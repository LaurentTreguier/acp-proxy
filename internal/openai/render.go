package openai

import (
	"encoding/json"
	"fmt"
	"strings"
)

// buildPrompt renders the OpenAI message list as a single ACP prompt text
// block. Every request starts a fresh ACP session, so prior conversation
// is included as a transcript for context.
func buildPrompt(messages []Message) string {
	var sb strings.Builder
	for _, m := range messages {
		text := extractText(m.Content)
		switch m.Role {
		case "system":
			fmt.Fprintf(&sb, "[System instructions]\n%s\n\n", text)
		case "user":
			fmt.Fprintf(&sb, "User: %s\n", text)
		case "assistant":
			fmt.Fprintf(&sb, "Assistant: %s\n", text)
		case "tool":
			fmt.Fprintf(&sb, "[tool result] %s\n", text)
		}
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// inputItem is one item of a Responses API input array: a message with a
// role, or a raw item like function_call_output.
type inputItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	CallID  string          `json:"call_id"`
	Output  string          `json:"output"`
}

// renderResponsesInput turns the Responses input field into a prompt
// transcript: instructions first, then the input items in order. The
// second return reports whether any text content was found; requests
// without renderable content are rejected by the caller instead of
// reaching the agent as an empty conversation.
func renderResponsesInput(instructions string, input json.RawMessage) (string, bool) {
	var sb strings.Builder
	if instructions != "" {
		fmt.Fprintf(&sb, "[System instructions]\n%s\n\n", instructions)
	}
	if len(input) == 0 {
		return strings.TrimSuffix(sb.String(), "\n"), instructions != ""
	}
	var s string
	if err := json.Unmarshal(input, &s); err == nil {
		fmt.Fprintf(&sb, "User: %s\n", s)
		return strings.TrimSuffix(sb.String(), "\n"), true
	}
	var items []inputItem
	if err := json.Unmarshal(input, &items); err != nil {
		return strings.TrimSuffix(sb.String(), "\n"), instructions != ""
	}
	hasContent := instructions != ""
	for _, it := range items {
		switch {
		case it.Type == "function_call_output":
			fmt.Fprintf(&sb, "[tool result for %s] %s\n", it.CallID, it.Output)
			hasContent = true
		case it.Role != "":
			text := extractText(it.Content)
			fmt.Fprintf(&sb, "%s: %s\n", roleLabel(it.Role), text)
			if text != "" {
				hasContent = true
			}
		}
	}
	return strings.TrimSuffix(sb.String(), "\n"), hasContent
}

// turnsFromInput extracts conversation turns from a Responses input
// field, for transcript persistence.
func turnsFromInput(input json.RawMessage) []turn {
	var s string
	if err := json.Unmarshal(input, &s); err == nil {
		if s != "" {
			return []turn{{Role: "user", Text: s}}
		}
		return nil
	}
	var items []inputItem
	if err := json.Unmarshal(input, &items); err != nil {
		return nil
	}
	var turns []turn
	for _, it := range items {
		switch {
		case it.Type == "function_call_output":
			turns = append(turns, turn{Role: "tool", Text: it.Output})
		case it.Role != "":
			turns = append(turns, turn{Role: it.Role, Text: extractText(it.Content)})
		}
	}
	return turns
}

// renderTranscript renders stored turns back into a prompt, for replaying
// a conversation into a fresh ACP session after a restart.
func renderTranscript(turns []turn) string {
	var sb strings.Builder
	for _, t := range turns {
		switch t.Role {
		case "system":
			fmt.Fprintf(&sb, "[System instructions]\n%s\n\n", t.Text)
		case "tool":
			fmt.Fprintf(&sb, "[tool result] %s\n", t.Text)
		default:
			fmt.Fprintf(&sb, "%s: %s\n", roleLabel(t.Role), t.Text)
		}
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

func roleLabel(role string) string {
	switch role {
	case "system", "developer":
		return "System"
	case "user", "assistant":
		return strings.ToUpper(role[:1]) + role[1:]
	default:
		return role
	}
}
