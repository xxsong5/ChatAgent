package agent

import "encoding/json"

// AgentEventType represents the type of an agent event
type AgentEventType string

const (
	EventThought    AgentEventType = "thought"
	EventToolCall   AgentEventType = "tool_call"
	EventToolResult AgentEventType = "tool_result"
	EventFinal      AgentEventType = "final"
	EventError      AgentEventType = "error"
	EventChunk      AgentEventType = "chunk"     // 流式输出的文本片段
	EventRoundEnd   AgentEventType = "round_end" // 单轮处理结束
)

// AgentEvent represents an event emitted by the agent during execution
type AgentEvent struct {
	Type AgentEventType `json:"type"`
	Data interface{}    `json:"data"`
}

// ToolCallEventData represents data for a tool call event
type ToolCallEventData struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

// ToolResultEventData represents data for a tool result event
type ToolResultEventData struct {
	Tool   string `json:"tool"`
	Result string `json:"result"`
}

// ErrorEventData represents data for an error event
type ErrorEventData struct {
	Error string `json:"error"`
}
