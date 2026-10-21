// Package openai implements the OpenAI-compatible HTTP surface of the
// proxy: /{agent}/v1/models and /{agent}/v1/chat/completions, plus the
// plain-text /agents listing.
package openai

import "encoding/json"

// Message is a chat message. Content is kept raw so string and
// content-part array forms are both accepted.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ChatCompletionRequest is the subset of the OpenAI Chat Completion API
// the proxy consumes.
type ChatCompletionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

// usage reports token usage. The proxy has no token accounting, so counts
// are zero.
type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type choiceMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type choice struct {
	Index        int           `json:"index"`
	Message      choiceMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

// ChatCompletionResponse is a non-streaming completion.
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
	Usage   usage    `json:"usage"`
}

type delta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type chunkChoice struct {
	Index        int    `json:"index"`
	Delta        delta  `json:"delta"`
	FinishReason string `json:"finish_reason"`
}

// ChatCompletionChunk is one SSE event in a streaming completion.
type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
}

type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// Error is the error envelope of the OpenAI API.
type Error struct {
	Error errorBody `json:"error"`
}

// extractText decodes a message Content field: plain string, null, or an
// array of content parts ({type:"text", text:"..."} supported).
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		out := ""
		for _, p := range parts {
			if p.Type == "text" {
				if out != "" {
					out += "\n"
				}
				out += p.Text
			}
		}
		return out
	}
	return ""
}
