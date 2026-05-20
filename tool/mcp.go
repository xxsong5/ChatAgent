package tool

import (
	"chatAgent/mcp"
	"chatAgent/mcp/transport"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
)

// MCPToolAdapter 将 MCP 工具适配为本地 Tool 接口
type MCPToolAdapter struct {
	BaseTool
	manager    *mcp.Manager
	serverName string
	tool       transport.Tool
}

// NewMCPToolAdapter 创建适配器
func NewMCPToolAdapter(manager *mcp.Manager, serverName string, mcpTool transport.Tool) *MCPToolAdapter {
	return &MCPToolAdapter{
		BaseTool: BaseTool{
			name:        fmt.Sprintf("mcp_%s_%s", serverName, mcpTool.Name),
			description: fmt.Sprintf("[MCP:%s] %s", serverName, mcpTool.Description),
			parameters:  mcpTool.InputSchema,
		},
		manager:    manager,
		serverName: serverName,
		tool:       mcpTool,
	}
}

// Execute 执行工具
func (a *MCPToolAdapter) Execute(ctx context.Context, params json.RawMessage) (string, error) {
	var arguments map[string]interface{}
	if err := json.Unmarshal(params, &arguments); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}

	result, err := a.manager.CallTool(ctx, a.serverName, a.tool.Name, arguments)
	if err != nil {
		return "", err
	}

	if result.IsError {
		return "", fmt.Errorf("工具执行错误: %s", formatContent(result.Content))
	}

	return formatContent(result.Content), nil
}

// formatContent 格式化内容块
func formatContent(content []transport.ContentBlock) string {
	var parts []string

	for _, block := range content {
		switch block.Type {
		case "text":
			parts = append(parts, block.Text)
		case "image":
			parts = append(parts, fmt.Sprintf("[图片: %s]", block.MimeType))
			log.Debug().Str("type", "image").Str("mimeType", block.MimeType).Msg("格式化内容块")
		case "audio":
			parts = append(parts, fmt.Sprintf("[音频: %s]", block.MimeType))
			log.Debug().Str("type", "audio").Str("mimeType", block.MimeType).Msg("格式化内容块")
		case "resource":
			if block.Resource != nil {
				parts = append(parts, fmt.Sprintf("[资源: %s]\n%s", block.Resource.URI, block.Resource.Text))
				log.Debug().Str("type", "resource").Str("uri", block.Resource.URI).Msg("格式化内容块")
			}
		}
	}

	return strings.Join(parts, "\n")
}
