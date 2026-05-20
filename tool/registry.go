package tool

import (
	"chatAgent/mcp"
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/sashabaranov/go-openai"
)

// Registry 工具注册表
type Registry struct {
	tools map[string]Tool
	mu    sync.RWMutex
}

// NewRegistry 创建工具注册表
func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

// Register 注册工具
func (r *Registry) Register(tool Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := tool.Name()
	if _, exists := r.tools[name]; exists {
		log.Warn().Str("tool", name).Msg("工具已存在")
		return fmt.Errorf("工具已存在: %s", name)
	}

	r.tools[name] = tool
	return nil
}

// RegisterMCPTools 注册 MCP 工具
func (r *Registry) RegisterMCPTools(manager *mcp.Manager) {
	mcpTools := manager.GetAllTools()

	for _, mcpTool := range mcpTools {
		adapter := NewMCPToolAdapter(manager, mcpTool.ServerName, mcpTool.Tool)

		if err := r.Register(adapter); err != nil {
			log.Error().Err(err).Str("tool", adapter.Name()).Msg("[MCP] 注册工具失败")
		} else {
			log.Info().Str("tool", adapter.Name()).Msg("[MCP] 已注册工具")
		}
	}
}

// Get 获取工具
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tool, exists := r.tools[name]
	return tool, exists
}

// List 列出所有工具
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tools := make([]Tool, 0, len(r.tools))
	for _, tool := range r.tools {
		tools = append(tools, tool)
	}
	return tools
}

// ToLLMTools 转换为LLM工具定义
func (r *Registry) ToLLMTools() []openai.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	definitions := make([]openai.Tool, 0, len(r.tools))
	for _, tool := range r.tools {
		definitions = append(definitions, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        tool.Name(),
				Description: tool.Description(),
				Parameters:  tool.Parameters(),
			},
		})
	}
	return definitions
}

// Execute 执行指定工具
func (r *Registry) Execute(ctx context.Context, name string, params json.RawMessage) (string, error) {
	tool, exists := r.Get(name)
	if !exists {
		log.Error().Str("tool", name).Msg("工具不存在")
		return "", fmt.Errorf("工具不存在: %s", name)
	}

	return tool.Execute(ctx, params)
}
