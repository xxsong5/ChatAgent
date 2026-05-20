// internal/mcp/manager.go
package mcp

import (
	"chatAgent/mcp/transport"
	"chatAgent/types"
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog/log"
)

// Manager MCP 服务器管理器
type Manager struct {
	clients map[string]*Client // 服务器名称 -> 客户端
	mu      sync.RWMutex
}

// NewManager 创建管理器
func NewManager() *Manager {

	return &Manager{
		clients: make(map[string]*Client),
	}
}

// AddServer 添加服务器
func (m *Manager) AddServer(config *types.MCPConfig) error {
	if !config.Enabled {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.clients[config.Name]; exists {
		log.Warn().Str("server", config.Name).Msg("服务器已存在")
		return fmt.Errorf("服务器已存在: %s", config.Name)
	}

	client, err := NewClient(config)
	if err != nil {
		log.Error().Err(err).Str("server", config.Name).Msg("创建客户端失败")
		return fmt.Errorf("创建客户端失败: %w", err)
	}

	m.clients[config.Name] = client
	return nil
}

// ConnectAll 连接所有服务器
func (m *Manager) ConnectAll(ctx context.Context) error {
	m.mu.RLock()
	clients := make([]*Client, 0, len(m.clients))
	for _, client := range m.clients {
		clients = append(clients, client)
	}
	m.mu.RUnlock()

	var wg sync.WaitGroup
	errCh := make(chan error, len(clients))

	for _, client := range clients {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			log.Info().Str("server", c.GetServerName()).Msg("[MCP] 尝试连接")
			if err := c.Connect(ctx); err != nil {
				errCh <- fmt.Errorf("连接 %s 失败: %w", c.GetServerName(), err)
			}
		}(client)
	}

	wg.Wait()
	close(errCh)

	// 收集错误（非致命，只打印警告）
	for err := range errCh {
		log.Warn().Err(err).Msg("[MCP] 警告")
	}

	return nil
}

// GetAllTools 获取所有服务器的工具
func (m *Manager) GetAllTools() []MCPTool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var allTools []MCPTool

	for serverName, client := range m.clients {
		tools := client.GetTools()
		for _, tool := range tools {
			allTools = append(allTools, MCPTool{
				ServerName: serverName,
				Tool:       tool,
			})
		}
	}

	return allTools
}

// MCPTool 带服务器信息的工具
type MCPTool struct {
	ServerName string
	Tool       transport.Tool
}

// CallTool 调用指定服务器的工具
func (m *Manager) CallTool(ctx context.Context, serverName, toolName string, arguments map[string]interface{}) (*transport.ToolsCallResult, error) {
	m.mu.RLock()
	client, ok := m.clients[serverName]
	m.mu.RUnlock()

	if !ok {
		log.Error().Str("server", serverName).Msg("服务器不存在")
		return nil, fmt.Errorf("服务器不存在: %s", serverName)
	}

	return client.CallTool(ctx, toolName, arguments)
}
