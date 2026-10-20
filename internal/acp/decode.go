package acp

import (
	sdk "github.com/coder/acp-go-sdk"
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
			Kind:       KindToolCall,
			ToolCallID: string(u.ToolCall.ToolCallId),
			Status:     string(u.ToolCall.Status),
		}
	case u.ToolCallUpdate != nil:
		upd := Update{
			Kind:       KindToolCallUpd,
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
