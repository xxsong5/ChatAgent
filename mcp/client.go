package mcp

import (
	"chatAgent/mcp/transport"
	"chatAgent/types"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog/log"
	// "agent_toy/internal/mcp/transport"
)

// Client MCP 客户端
type Client struct {
	config    *types.MCPConfig
	transport transport.Transport

	serverInfo   *transport.ServerInfo
	capabilities *transport.ServerCapabilities
	tools        []transport.Tool

	requestID int64
	mu        sync.RWMutex

	initialized bool
}

// NewClient 创建 MCP 客户端
func NewClient(config *types.MCPConfig) (*Client, error) {
	var trans transport.Transport

	switch config.Transport {
	case "stdio":
		trans = transport.NewStdioTransport(config)
	case "http":
		trans = transport.NewHTTPTransport(config)
	default:
		log.Error().Str("transport", config.Transport).Msg("不支持的传输类型")
		return nil, fmt.Errorf("不支持的传输类型: %s", config.Transport)
	}

	client := &Client{
		config:    config,
		transport: trans,
		tools:     make([]transport.Tool, 0),
	}

	// 设置通知处理器
	trans.OnNotification(client.handleNotification)

	return client, nil
}

// Connect 连接并初始化
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.initialized {
		return nil
	}

	// 建立传输连接
	if err := c.transport.Connect(ctx); err != nil {
		log.Error().Err(err).Str("server", c.config.Name).Msg("MCP连接失败")
		return fmt.Errorf("连接失败: %w", err)
	}

	// 发送初始化请求
	initParams := transport.InitializeParams{
		ProtocolVersion: "2025-03-26",
		Capabilities: transport.ClientCapabilities{
			Roots: &transport.RootsCapability{ListChanged: true},
		},
		ClientInfo: transport.ClientInfo{
			Name:    "go-agent",
			Version: "1.0.0",
		},
	}

	paramsBytes, _ := json.Marshal(initParams)

	initReq := &transport.JSONRPCMessage{
		JSONRPC: "2.0",
		ID:      c.nextID(),
		Method:  transport.MethodInitialize,
		Params:  paramsBytes,
	}

	resp, err := c.transport.SendRequest(ctx, initReq)
	if err != nil {
		log.Error().Err(err).Msg("初始化请求失败")
		return fmt.Errorf("初始化请求失败: %w", err)
	}

	if resp.Error != nil {
		log.Error().Str("error", resp.Error.Message).Msg("初始化错误")
		return fmt.Errorf("初始化错误: %s", resp.Error.Message)
	}

	// 解析初始化结果
	var initResult transport.InitializeResult
	if err := json.Unmarshal(resp.Result, &initResult); err != nil {
		log.Error().Err(err).Msg("解析初始化结果失败")
		return fmt.Errorf("解析初始化结果失败: %w", err)
	}

	c.serverInfo = &initResult.ServerInfo
	c.capabilities = &initResult.Capabilities

	log.Info().Str("server", initResult.ServerInfo.Name).Str("version", initResult.ServerInfo.Version).Msg("[MCP] 已连接到服务器")

	// 发送初始化完成通知
	initializedNotif := &transport.JSONRPCMessage{
		JSONRPC: "2.0",
		Method:  transport.MethodInitialized,
	}

	if err := c.transport.Send(ctx, initializedNotif); err != nil {
		return fmt.Errorf("发送初始化完成通知失败: %w", err)
	}

	c.initialized = true

	// 获取工具列表
	if c.capabilities.Tools != nil {
		if err := c.refreshTools(ctx); err != nil {
			log.Warn().Err(err).Msg("[MCP] 警告: 获取工具列表失败")
		}
	}

	return nil
}

// refreshTools 刷新工具列表
func (c *Client) refreshTools(ctx context.Context) error {
	var allTools []transport.Tool
	var cursor string

	for {
		params := transport.ToolsListParams{Cursor: cursor}
		paramsBytes, _ := json.Marshal(params)

		req := &transport.JSONRPCMessage{
			JSONRPC: "2.0",
			ID:      c.nextID(),
			Method:  transport.MethodToolsList,
			Params:  paramsBytes,
		}

		resp, err := c.transport.SendRequest(ctx, req)
		if err != nil {
			return err
		}

		if resp.Error != nil {
			log.Error().Str("error", resp.Error.Message).Msg("获取工具列表失败")
			return fmt.Errorf("获取工具列表失败: %s", resp.Error.Message)
		}

		var result transport.ToolsListResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return err
		}

		allTools = append(allTools, result.Tools...)

		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}

	c.tools = allTools
	log.Info().Str("server", c.config.Name).Int("count", len(allTools)).Msg("[MCP] 获取到工具列表")

	return nil
}

// handleNotification 处理服务器通知
func (c *Client) handleNotification(method string, params []byte) {
	switch method {
	case transport.MethodToolsListChanged:
		log.Info().Msg("[MCP] 工具列表已变更，正在刷新...")
		ctx := context.Background()
		if err := c.refreshTools(ctx); err != nil {
			log.Error().Err(err).Msg("[MCP] 刷新工具列表失败")
		}
	default:
		log.Info().Str("method", method).Msg("[MCP] 收到通知")
	}
}

// GetTools 获取工具列表
func (c *Client) GetTools() []transport.Tool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make([]transport.Tool, len(c.tools))
	copy(result, c.tools)
	return result
}

// CallTool 调用工具
func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]interface{}) (*transport.ToolsCallResult, error) {
	c.mu.RLock()
	if !c.initialized {
		c.mu.RUnlock()
		log.Error().Msg("客户端未初始化")
		return nil, fmt.Errorf("客户端未初始化")
	}
	c.mu.RUnlock()

	params := transport.ToolsCallParams{
		Name:      name,
		Arguments: arguments,
	}
	paramsBytes, _ := json.Marshal(params)

	req := &transport.JSONRPCMessage{
		JSONRPC: "2.0",
		ID:      c.nextID(),
		Method:  transport.MethodToolsCall,
		Params:  paramsBytes,
	}

	resp, err := c.transport.SendRequest(ctx, req)
	if err != nil {
		log.Error().Err(err).Str("tool", name).Msg("调用工具失败")
		return nil, fmt.Errorf("调用工具失败: %w", err)
	}

	if resp.Error != nil {
		log.Error().Str("error", resp.Error.Message).Msg("工具调用错误")
		return nil, fmt.Errorf("工具调用错误: %s", resp.Error.Message)
	}

	var result transport.ToolsCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		log.Error().Err(err).Msg("解析工具结果失败")
		return nil, fmt.Errorf("解析工具结果失败: %w", err)
	}

	return &result, nil
}

// GetServerName 获取服务器名称
func (c *Client) GetServerName() string {
	return c.config.Name
}

// nextID 生成下一个请求ID
func (c *Client) nextID() transport.RPCID {
	id := fmt.Sprintf("%d", atomic.AddInt64(&c.requestID, 1))
	log.Debug().Str("id", id).Msg("生成请求ID")
	return transport.RPCID(id)
}
