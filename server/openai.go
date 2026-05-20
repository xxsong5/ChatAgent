package server

import (
	"chatAgent/agent"
	"chatAgent/sandbox"
	"chatAgent/types"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/sashabaranov/go-openai"
)

// OpenAIServer OpenAI兼容API服务器
// 提供与OpenAI API兼容的HTTP接口，支持流式和非流式响应
// 通过x-session-id header或user字段中的session://前缀实现多轮会话
type OpenAIServer struct {
	cfg        *types.Config
	sessionMgr *SessionManager
	server     *http.Server
}

// NewOpenAIServer 创建OpenAI兼容API服务器
func NewOpenAIServer(cfg *types.Config, sessionMgr *SessionManager) *OpenAIServer {
	s := &OpenAIServer{
		cfg:        cfg,
		sessionMgr: sessionMgr,
	}
	return s
}

// Start 启动HTTP服务器
func (s *OpenAIServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/sessions", s.handleSessions)

	// 文件暂存下载端点
	stashDir := s.cfg.StashDir
	if stashDir == "" {
		stashDir = "config/stash"
	}
	mux.Handle("/stash/", http.StripPrefix("/stash/", http.FileServer(http.Dir(stashDir))))

	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	s.server = &http.Server{
		Addr:    addr,
		Handler: s.withCORS(s.withAuth(mux)),
	}

	log.Info().Str("addr", addr).Str("type", "openai").Msg("启动OpenAI兼容API服务器")
	return s.server.ListenAndServe()
}

// Shutdown 关闭服务器
func (s *OpenAIServer) Shutdown() error {
	if s.server != nil {
		s.sessionMgr.Shutdown()
		sandbox.DestroyManager()
		return s.server.Close()
	}
	return nil
}

// withCORS 跨域中间件
func (s *OpenAIServer) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-Id")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withAuth 认证中间件
func (s *OpenAIServer) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Server.Auth != "" {
			authHeader := r.Header.Get("Authorization")
			expected := "Bearer " + s.cfg.Server.Auth
			if authHeader != expected && authHeader != s.cfg.Server.Auth {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// handleModels 处理模型列表请求
func (s *OpenAIServer) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	resp := struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
	}{
		Object: "list",
		Data: []modelEntry{
			{ID: s.cfg.Model, Object: "model", Created: time.Now().Unix(), OwnedBy: "chat-agent"},
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// chatRequest OpenAI兼容的聊天请求结构
type chatRequest struct {
	Model       string                         `json:"model"`
	Messages    []openai.ChatCompletionMessage `json:"messages"`
	Stream      bool                           `json:"stream"`
	Temperature float64                        `json:"temperature"`
	MaxTokens   int                            `json:"max_tokens"`
	User        string                         `json:"user"` // 可用于传递sessionID
}

// extractSessionID 从请求中提取sessionID
// 优先级: X-Session-Id header > user字段中的session://前缀
func extractSessionID(r *http.Request, req *chatRequest) string {
	// 优先从header获取
	sessionID := r.Header.Get("X-Session-Id")
	if sessionID != "" {
		return sessionID
	}

	// 从user字段获取（支持 session://session-id 格式）
	if strings.HasPrefix(req.User, "session://") {
		return strings.TrimPrefix(req.User, "session://")
	}

	return ""
}

// handleChatCompletions 处理聊天补全请求
// 支持流式(SSE)和非流式响应
func (s *OpenAIServer) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 读取请求体
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"读取请求体失败: %s"}`, err.Error()), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"解析请求体失败: %s"}`, err.Error()), http.StatusBadRequest)
		return
	}

	if len(req.Messages) == 0 {
		http.Error(w, `{"error":"messages不能为空"}`, http.StatusBadRequest)
		return
	}

	// 获取最后一条用户消息
	lastUserMsg := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == openai.ChatMessageRoleUser {
			lastUserMsg = req.Messages[i].Content
			break
		}
	}

	if lastUserMsg == "" {
		http.Error(w, `{"error":"没有找到用户消息"}`, http.StatusBadRequest)
		return
	}

	// 构建system prompt（可以有多条system消息拼接）
	systemPrompt := ""
	for _, msg := range req.Messages {
		if msg.Role == openai.ChatMessageRoleSystem {
			if systemPrompt != "" {
				systemPrompt += "\n"
			}
			systemPrompt += msg.Content
		}
	}

	// 提取sessionID
	sessionID := extractSessionID(r, &req)

	// 路由消息到SessionManager
	ctx := r.Context()
	session, eventCh, err := s.sessionMgr.RouteMessage(ctx, sessionID, "", lastUserMsg, systemPrompt)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"路由消息失败: %s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	if req.Stream {
		s.handleStreamResponse(w, session, eventCh)
	} else {
		s.handleNonStreamResponse(w, session, eventCh)
	}
}

// handleStreamResponse 处理流式SSE响应
func (s *OpenAIServer) handleStreamResponse(w http.ResponseWriter, session *Session, eventCh chan agent.AgentEvent) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"不支持流式响应"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Session-Id", session.ID)

	// 发送开始事件
	responseID := uuid.New().String()
	writeSSE(w, &openai.ChatCompletionStreamResponse{
		ID:      responseID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   s.cfg.Model,
		Choices: []openai.ChatCompletionStreamChoice{
			{
				Index: 0,
				Delta: openai.ChatCompletionStreamChoiceDelta{
					Role: openai.ChatMessageRoleAssistant,
				},
			},
		},
	})
	flusher.Flush()

	// 收集完整内容
	var fullContent strings.Builder

	// 监听事件并发送SSE
	for event := range eventCh {
		switch event.Type {
		case agent.EventChunk:
			content, _ := event.Data.(string)
			fullContent.WriteString(content)
			writeSSE(w, &openai.ChatCompletionStreamResponse{
				ID:      responseID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   s.cfg.Model,
				Choices: []openai.ChatCompletionStreamChoice{
					{
						Index: 0,
						Delta: openai.ChatCompletionStreamChoiceDelta{
							Content: content,
						},
					},
				},
			})
			flusher.Flush()

		case agent.EventToolCall:
			// 工具调用信息通过事件传递（但不是标准OpenAI流中的部分）
		case agent.EventToolResult:
			// 工具结果事件

		case agent.EventFinal:
			// 最终消息已经通过EventChunk发送完毕

		case agent.EventRoundEnd:
			session.Agent.Unsubscribe(eventCh)
			goto afterStreamLoop

		case agent.EventError:
			errData, _ := json.Marshal(event.Data)
			writeSSE(w, &openai.ChatCompletionStreamResponse{
				ID:      responseID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   s.cfg.Model,
				Choices: []openai.ChatCompletionStreamChoice{
					{
						Index: 0,
						Delta: openai.ChatCompletionStreamChoiceDelta{
							Content: fmt.Sprintf("\n\n[错误: %s]", string(errData)),
						},
					},
				},
			})
			flusher.Flush()
		}
	}
afterStreamLoop:

	// 发送结束标记
	writeSSE(w, &openai.ChatCompletionStreamResponse{
		ID:      responseID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   s.cfg.Model,
		Choices: []openai.ChatCompletionStreamChoice{
			{
				Index:        0,
				Delta:        openai.ChatCompletionStreamChoiceDelta{},
				FinishReason: openai.FinishReasonStop,
			},
		},
	})
	flusher.Flush()

	// 发送[DONE]标记
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// handleNonStreamResponse 处理非流式响应
func (s *OpenAIServer) handleNonStreamResponse(w http.ResponseWriter, session *Session, eventCh chan agent.AgentEvent) {
	var fullContent strings.Builder

	// 收集所有事件直到完成
	for event := range eventCh {
		switch event.Type {
		case agent.EventChunk:
			content, _ := event.Data.(string)
			fullContent.WriteString(content)
		case agent.EventFinal:
			// 内容已通过chunk收集
		case agent.EventRoundEnd:
			session.Agent.Unsubscribe(eventCh)
			goto doneNonStream
		case agent.EventError:
			errData, _ := json.Marshal(event.Data)
			fullContent.WriteString(fmt.Sprintf("\n\n[错误: %s]", string(errData)))
		}
	}
doneNonStream:

	resp := openai.ChatCompletionResponse{
		ID:      uuid.New().String(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   s.cfg.Model,
		Choices: []openai.ChatCompletionChoice{
			{
				Index: 0,
				Message: openai.ChatCompletionMessage{
					Role:    openai.ChatMessageRoleAssistant,
					Content: fullContent.String(),
				},
				FinishReason: openai.FinishReasonStop,
			},
		},
		Usage: openai.Usage{
			PromptTokens:     0,
			CompletionTokens: 0,
			TotalTokens:      0,
		},
	}

	w.Header().Set("X-Session-Id", session.ID)
	writeJSON(w, http.StatusOK, resp)
}

// handleSessions 会话管理端点
func (s *OpenAIServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// 列出所有活跃会话
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
		// 删除指定会话
		sessionID := r.URL.Query().Get("session_id")
		if sessionID == "" {
			http.Error(w, `{"error":"缺少session_id参数"}`, http.StatusBadRequest)
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

// ========== 工具函数 ==========

// writeJSON 写JSON响应
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// writeSSE 写SSE事件的统一方法
// 将 OpenAI ChatCompletionStreamResponse 序列化为 SSE data 行
func writeSSE(w http.ResponseWriter, data interface{}) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", jsonData)
}

// ParseChatRequest 从JSON字节流解析OpenAI兼容的聊天请求
func ParseChatRequest(data []byte) (*chatRequest, error) {
	var req chatRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("解析聊天请求失败: %w", err)
	}
	return &req, nil
}
