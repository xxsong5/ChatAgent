package transport

import (
	"encoding/json"
	"strconv"
)

// ==================== JSON-RPC 基础类型 ====================

// JSONRPCMessage JSON-RPC 消息基础结构
type JSONRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"` // 固定 "2.0"
	ID      RPCID           `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

type RPCID string

func (id *RPCID) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*id = RPCID(s)
		return nil
	}
	var n float64
	if err := json.Unmarshal(data, &n); err == nil {
		*id = RPCID(strconv.FormatFloat(n, 'f', -1, 64))
		return nil
	}
	*id = ""
	return nil
}

// JSONRPCError JSON-RPC 错误
type JSONRPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// 标准 JSON-RPC 错误码
const (
	ErrCodeParseError     = -32700
	ErrCodeInvalidRequest = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternalError  = -32603
)

// ==================== MCP 协议类型 ====================

// MCP 方法常量
const (
	MethodInitialize       = "initialize"
	MethodInitialized      = "notifications/initialized"
	MethodToolsList        = "tools/list"
	MethodToolsCall        = "tools/call"
	MethodToolsListChanged = "notifications/tools/list_changed"
)

// ==================== 初始化相关 ====================

// InitializeParams 初始化请求参数
type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      ClientInfo         `json:"clientInfo"`
}

// ClientCapabilities 客户端能力
type ClientCapabilities struct {
	Roots    *RootsCapability    `json:"roots,omitempty"`
	Sampling *SamplingCapability `json:"sampling,omitempty"`
}

// RootsCapability 根目录能力
type RootsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// SamplingCapability 采样能力
type SamplingCapability struct{}

// ClientInfo 客户端信息
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResult 初始化响应
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      ServerInfo         `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

// ServerCapabilities 服务端能力
type ServerCapabilities struct {
	Tools     *ToolsCapability     `json:"tools,omitempty"`
	Resources *ResourcesCapability `json:"resources,omitempty"`
	Prompts   *PromptsCapability   `json:"prompts,omitempty"`
	Logging   *LoggingCapability   `json:"logging,omitempty"`
}

// ToolsCapability 工具能力
type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ResourcesCapability 资源能力
type ResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// PromptsCapability 提示词能力
type PromptsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// LoggingCapability 日志能力
type LoggingCapability struct{}

// ServerInfo 服务端信息
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ==================== 工具相关 ====================

// ToolsListParams 工具列表请求参数
type ToolsListParams struct {
	Cursor string `json:"cursor,omitempty"`
}

// ToolsListResult 工具列表响应
type ToolsListResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// Tool 工具定义
type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"inputSchema"`
	Annotations *ToolAnnotations       `json:"annotations,omitempty"`
}

// ToolAnnotations 工具注解
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint,omitempty"`
	DestructiveHint bool   `json:"destructiveHint,omitempty"`
	IdempotentHint  bool   `json:"idempotentHint,omitempty"`
	OpenWorldHint   bool   `json:"openWorldHint,omitempty"`
}

// ToolsCallParams 工具调用请求参数
type ToolsCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments,omitempty"`
}

// ToolsCallResult 工具调用响应
type ToolsCallResult struct {
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// ContentBlock 内容块
type ContentBlock struct {
	Type     string `json:"type"` // "text" | "image" | "audio" | "resource"
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"` // base64 编码
	MimeType string `json:"mimeType,omitempty"`
	// Resource 类型
	Resource *ResourceContent `json:"resource,omitempty"`
}

// ResourceContent 资源内容
type ResourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"` // base64
}
