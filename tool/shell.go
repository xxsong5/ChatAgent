package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"chatAgent/sandbox"
)

// ShellTool Shell命令执行工具
type ShellTool struct {
	BaseTool
}

// NewShellTool 创建Shell工具
func NewShellTool() Tool {
	return &ShellTool{
		BaseTool: BaseTool{
			name:        "shell",
			description: "执行Shell命令并返回输出结果",
			parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"command": map[string]interface{}{
						"type":        "string",
						"description": "要执行的Shell命令",
					},
				},
				"required": []string{"command"},
			},
		},
	}
}

func (t *ShellTool) Execute(ctx context.Context, params json.RawMessage) (string, error) {
	var input struct {
		Command string `json:"command"`
	}

	if err := json.Unmarshal(params, &input); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}

	// 从context中获取sandbox instance（由agent层通过sandbox.WithContainer注入）
	instance := sandbox.GetContainer(ctx)
	if instance == nil {
		return "", fmt.Errorf("sandbox container instance未设置")
	}

	return instance.Exec(ctx, input.Command)
}
