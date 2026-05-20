package server

import (
	"chatAgent/agent"
	"chatAgent/sandbox"
	"chatAgent/types"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// MCPServer MCP HTTP服务器
// 使用MCP协议（Model Context Protocol）提供会话交互服务
// 支持通过 MCP 标准传输格式进行消息交互
type MCPServer struct {
	cfg        *types.Config
	sessionMgr *SessionManager
	server     *http.Server
}

// MCPRequest MCP协议请求结构
type MCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// MCPResponse MCP协议响应结构
type MCPResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      string      `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *MCPError   `json:"error,omitempty"`
}

// MCPError MCP协议错误结构
type MCPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCPChatParams 聊天方法参数
type MCPChatParams struct {
	Message      string `json:"message"`
	SessionID    string `json:"session_id,omitempty"`
	SystemPrompt string `json:"system_prompt,omitempty"`
}

// MCPResult 聊天方法结果
type MCPResult struct {
	SessionID string `json:"session_id"`
	Content   string `json:"content"`
}

// NewMCPServer 创建MCP HTTP服务器
func NewMCPServer(cfg *types.Config, sessionMgr *SessionManager) *MCPServer {
	return &MCPServer{
		cfg:        cfg,
		sessionMgr: sessionMgr,
	}
}

// Start 启动MCP HTTP服务器
func (s *MCPServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/v1/chat", s.handleChat)
	mux.HandleFunc("/mcp/v1/sessions", s.handleSessions)
	mux.HandleFunc("/mcp/v1/stream", s.handleStream)

	// 文件暂存下载端点
	stashDir := s.cfg.StashDir
	if stashDir == "" {
		stashDir = "config/stash"
	}
	mux.Handle("/stash/", http.StripPrefix("/stash/", http.FileServer(http.Dir(stashDir))))

	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	s.server = &http.Server{
		Addr:    addr,
		Handler: s.withCORS(mux),
	}

	log.Info().Str("addr", addr).Str("type", "mcp").Msg("启动MCP HTTP服务器")
	return s.server.ListenAndServe()
}

// Shutdown 关闭服务器
func (s *MCPServer) Shutdown() error {
	if s.server != nil {
		s.sessionMgr.Shutdown()
		sandbox.DestroyManager()
		return s.server.Close()
	}
	return nil
}

// withCORS 跨域中间件
func (s *MCPServer) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Session-Id")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleChat 处理MCP聊天请求
func (s *MCPServer) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, s.mcpError("", -32601, "方法不支持"))
		return
	}

	var mcpReq MCPRequest
	if err := json.NewDecoder(r.Body).Decode(&mcpReq); err != nil {
		writeJSON(w, http.StatusBadRequest, s.mcpError("", -32700, fmt.Sprintf("解析请求失败: %v", err)))
		return
	}
	defer r.Body.Close()

	// 解析参数
	var params MCPChatParams
	if mcpReq.Params != nil {
		if err := json.Unmarshal(mcpReq.Params, &params); err != nil {
			writeJSON(w, http.StatusBadRequest, s.mcpError(mcpReq.ID, -32602, fmt.Sprintf("参数解析失败: %v", err)))
			return
		}
	}

	if strings.TrimSpace(params.Message) == "" {
		writeJSON(w, http.StatusBadRequest, s.mcpError(mcpReq.ID, -32602, "消息不能为空"))
		return
	}

	// 路由消息到SessionManager
	ctx := r.Context()
	session, eventCh, err := s.sessionMgr.RouteMessage(ctx, params.SessionID, "", params.Message, params.SystemPrompt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, s.mcpError(mcpReq.ID, -32603, fmt.Sprintf("路由消息失败: %v", err)))
		return
	}

	// 收集完整响应
	var fullContent strings.Builder
	for event := range eventCh {
		switch event.Type {
		case agent.EventChunk:
			content, _ := event.Data.(string)
			fullContent.WriteString(content)
		case agent.EventError:
			errData, _ := json.Marshal(event.Data)
			fullContent.WriteString(fmt.Sprintf("\n\n[错误: %s]", string(errData)))
		case agent.EventRoundEnd:
			session.Agent.Unsubscribe(eventCh)
			goto doneNonStream
		}
	}
doneNonStream:

	// 返回MCP响应
	writeJSON(w, http.StatusOK, MCPResponse{
		JSONRPC: "2.0",
		ID:      mcpReq.ID,
		Result: MCPResult{
			SessionID: session.ID,
			Content:   fullContent.String(),
		},
	})
}

// handleStream 处理MCP流式聊天请求（SSE）
func (s *MCPServer) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	var mcpReq MCPRequest
	if err := json.NewDecoder(r.Body).Decode(&mcpReq); err != nil {
		http.Error(w, fmt.Sprintf("parse error: %v", err), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var params MCPChatParams
	if mcpReq.Params != nil {
		json.Unmarshal(mcpReq.Params, &params)
	}

	if strings.TrimSpace(params.Message) == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	session, eventCh, err := s.sessionMgr.RouteMessage(ctx, params.SessionID, "", params.Message, params.SystemPrompt)
	if err != nil {
		http.Error(w, fmt.Sprintf("route error: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Session-Id", session.ID)

	// 发送MCP流式事件
	for event := range eventCh {
		if event.Type == agent.EventRoundEnd {
			session.Agent.Unsubscribe(eventCh)
			break
		}
		mcpEvent := map[string]interface{}{
			"jsonrpc":    "2.0",
			"id":         mcpReq.ID,
			"session_id": session.ID,
			"event_type": event.Type,
			"data":       event.Data,
		}

		data, _ := json.Marshal(mcpEvent)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	// 发送完成事件
	doneEvent := map[string]interface{}{
		"jsonrpc":    "2.0",
		"id":         mcpReq.ID,
		"session_id": session.ID,
		"event_type": "done",
	}
	data, _ := json.Marshal(doneEvent)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// handleSessions 会话管理端点
func (s *MCPServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sessions := s.sessionMgr.ListSessions()
		sessionList := make([]map[string]interface{}, 0, len(sessions))
		for _, sess := range sessions {
			sessionList = append(sessionList, map[string]interface{}{
				"id":         sess.ID,
				"created_at": sess.CreatedAt,
				"updated_at": sess.UpdatedAt,
				"is_active":  sess.IsActive,
			})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"sessions": sessionList,
			"count":    len(sessionList),
		})

	case http.MethodDelete:
		sessionID := r.URL.Query().Get("session_id")
		if sessionID == "" {
			http.Error(w, `{"error":"missing session_id"}`, http.StatusBadRequest)
			return
		}
		if err := s.sessionMgr.DeleteSession(sessionID); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// mcpError 创建MCP错误响应
func (s *MCPServer) mcpError(id string, code int, message string) MCPResponse {
	return MCPResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &MCPError{
			Code:    code,
			Message: message,
		},
	}
}

// ========== MCP Server工具函数 ==========

// Ensure uuid is used
var _ = uuid.New
var _ = time.Now
