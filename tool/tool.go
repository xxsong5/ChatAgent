package tool

import (
	"context"
	"encoding/json"
)

// Tool 工具接口
type Tool interface {
	// Name 返回工具名称
	Name() string

	// Description 返回工具描述
	Description() string

	// Parameters 返回参数Schema（JSON Schema格式）
	Parameters() map[string]interface{}

	// Execute 执行工具
	Execute(ctx context.Context, params json.RawMessage) (string, error)
}

// ToolResult 工具执行结果
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
	IsError    bool   `json:"is_error"`
}

// BaseTool 基础工具实现
type BaseTool struct {
	name        string
	description string
	parameters  map[string]interface{}
}

func (t *BaseTool) Name() string {
	return t.name
}

func (t *BaseTool) Description() string {
	return t.description
}

func (t *BaseTool) Parameters() map[string]interface{} {
	return t.parameters
}
