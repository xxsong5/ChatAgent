package server

import (
	"chatAgent/agent"
	"chatAgent/sandbox"
	"chatAgent/types"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sashabaranov/go-openai"
)

const (
	userIDCookieName = "chat_user_id"
)

// generateUserID 生成一个新的用户ID
func generateUserID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// getUserID 从请求中获取用户ID，如果没有则创建新的
func (s *WebServer) getUserID(r *http.Request) string {
	cookie, err := r.Cookie(userIDCookieName)
	if err == nil && cookie.Value != "" {
		return cookie.Value
	}
	return ""
}

// ensureUserID 确保请求有用户ID，并返回响应writer设置cookie
func (s *WebServer) ensureUserID(w http.ResponseWriter, r *http.Request) string {
	userID := s.getUserID(r)
	if userID == "" {
		var err error
		userID, err = generateUserID()
		if err != nil {
			// 生成失败时使用时间戳作为后备
			userID = fmt.Sprintf("user_%d", time.Now().UnixNano())
		}
		// 设置cookie，有效期7天
		http.SetCookie(w, &http.Cookie{
			Name:     userIDCookieName,
			Value:    userID,
			Path:     "/",
			MaxAge:   7 * 24 * 60 * 60,
			HttpOnly: false, // 允许前端JS访问
			SameSite: http.SameSiteLaxMode,
		})
	}
	return userID
}

// getUserSessions 获取指定用户的会话列表
func (s *WebServer) getUserSessions(userID string) []*Session {
	allSessions := s.sessionMgr.ListSessions()
	userSessions := make([]*Session, 0, len(allSessions))
	for _, sess := range allSessions {
		if sess.UserID == userID {
			userSessions = append(userSessions, sess)
		}
	}
	return userSessions
}

//go:embed webui
var webUI embed.FS

// webUISub is the webui directory as a rooted filesystem (for serving static assets)
var webUISub = func() fs.FS {
	s, err := fs.Sub(webUI, "webui")
	if err != nil {
		panic("fs.Sub(webUI, webui): " + err.Error())
	}
	return s
}()

// WebServer HTTP UI交互服务器
// 提供Web管理界面，支持用户通过浏览器进行多轮会话
type WebServer struct {
	cfg        *types.Config
	sessionMgr *SessionManager
	server     *http.Server
}

// WebMessage Web界面消息结构
type WebMessage struct {
	Role       string            `json:"role"`
	Content    string            `json:"content"`
	ToolCalls  []openai.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
}

// NewWebServer 创建Web服务器
func NewWebServer(cfg *types.Config, sessionMgr *SessionManager) *WebServer {
	return &WebServer{
		cfg:        cfg,
		sessionMgr: sessionMgr,
	}
}

// Start 启动HTTP服务器
func (s *WebServer) Start() error {
	mux := http.NewServeMux()

	// API端点
	mux.HandleFunc("/api/chat", s.handleChat)
	mux.HandleFunc("/api/chat/stream", s.handleChatStream)
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/sessions/", s.handleSessionByID)

	// 文件暂存下载端点
	stashDir := s.cfg.StashDir
	if stashDir == "" {
		stashDir = "config/stash"
	}
	mux.Handle("/stash/", http.StripPrefix("/stash/", http.FileServer(http.Dir(stashDir))))

	// 静态资源文件
	mux.Handle("/assets/", http.FileServer(http.FS(webUISub)))

	// Web界面
	mux.HandleFunc("/", s.handleIndex)

	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	s.server = &http.Server{
		Addr:    addr,
		Handler: s.withMiddleware(mux),
	}

	log.Info().Str("addr", addr).Str("type", "web").Msg("启动Web服务器")
	fmt.Printf("\n  Web UI: http://%s\n\n", addr)
	return s.server.ListenAndServe()
}

// Shutdown 关闭服务器
func (s *WebServer) Shutdown() error {
	if s.server != nil {
		s.sessionMgr.Shutdown()
		sandbox.DestroyManager()
		return s.server.Close()
	}
	return nil
}

// withMiddleware 中间件
func (s *WebServer) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Session-Id")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// handleIndex 提供Web界面
func (s *WebServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	tmplContent, err := webUI.ReadFile("webui/index.html")
	if err != nil {
		http.Error(w, fmt.Sprintf("read template error: %v", err), http.StatusInternalServerError)
		return
	}

	tmpl := template.Must(template.New("index").Parse(string(tmplContent)))
	tmpl.Execute(w, map[string]interface{}{
		"Model": s.cfg.Model,
	})
}

// webChatRequest Web聊天请求
type webChatRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id"`
}

// handleChat 处理聊天请求（非流式）
func (s *WebServer) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 确保用户有ID
	userID := s.ensureUserID(w, r)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read error: %v", err), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req webChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("parse error: %v", err), http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(req.Message) == "" {
		http.Error(w, `{"error":"message is required"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	session, eventCh, err := s.sessionMgr.RouteMessage(ctx, req.SessionID, userID, req.Message, "", map[string]interface{}{
		"timestamp": time.Now().Format("2006-01-02 15:04:05") + " " + time.Now().Weekday().String(),
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("route error: %v", err), http.StatusInternalServerError)
		return
	}

	// 收集完整响应

	var fullContent strings.Builder
	for event := range eventCh {
		switch event.Type {
		case agent.EventFinal:
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

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"session_id": session.ID,
		"content":    fullContent.String(),
	})
}

// handleChatStream 处理流式聊天请求（SSE）
func (s *WebServer) handleChatStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 确保用户有ID
	userID := s.ensureUserID(w, r)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read error: %v", err), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req webChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("parse error: %v", err), http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(req.Message) == "" {
		http.Error(w, `{"error":"message is required"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	session, eventCh, err := s.sessionMgr.RouteMessage(ctx, req.SessionID, userID, req.Message, "", map[string]interface{}{
		"timestamp": time.Now().Format("2006-01-02 15:04:05") + " " + time.Now().Weekday().String(),
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("route error: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")

	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Session-Id", session.ID)

	// 发送session_id事件（JSON字符串格式，确保前端JSON.parse能正确解析）
	fmt.Fprintf(w, "event: session\ndata: \"%s\"\n\n", session.ID)
	flusher.Flush()

	// 发送Agent事件流
	for event := range eventCh {
		eventData, _ := json.Marshal(map[string]interface{}{
			"type": event.Type,
			"data": event.Data,
		})

		switch event.Type {
		case agent.EventChunk:
			fmt.Fprintf(w, "event: chunk\ndata: %s\n\n", eventData)
		case agent.EventToolCall:
			fmt.Fprintf(w, "event: tool_call\ndata: %s\n\n", eventData)
		case agent.EventToolResult:
			fmt.Fprintf(w, "event: tool_result\ndata: %s\n\n", eventData)
		//case agent.EventFinal:
		//	fmt.Fprintf(w, "event: final\ndata: %s\n\n", eventData)
		case agent.EventRoundEnd:
			session.Agent.Unsubscribe(eventCh)
			goto afterStreamLoop
		case agent.EventError:
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", eventData)
		}
		flusher.Flush()
	}
afterStreamLoop:

	// 发送完成事件
	fmt.Fprintf(w, "event: done\ndata: {}\n\n")
	flusher.Flush()
}

// stripMetaPrefix 去除消息内容中的元数据前缀（如 [消息元数据: {...}]\n）
func stripMetaPrefix(content string) string {
	if strings.HasPrefix(content, "[消息元数据:") {
		if idx := strings.Index(content, "]\n"); idx > 0 {
			return content[idx+2:]
		}
	}
	return content
}

// handleSessions 会话列表管理
func (s *WebServer) handleSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// 确保用户有ID
		userID := s.ensureUserID(w, r)
		sessions := s.getUserSessions(userID)
		sessionList := make([]map[string]interface{}, 0, len(sessions))
		for _, sess := range sessions {
			sessionList = append(sessionList, map[string]interface{}{
				"id":         sess.ID,
				"title":      sess.Title,
				"created_at": sess.CreatedAt,
				"updated_at": sess.UpdatedAt,
				"is_active":  sess.IsActive,
			})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"sessions": sessionList,
			"count":    len(sessionList),
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleSessionByID 单个会话操作
func (s *WebServer) handleSessionByID(w http.ResponseWriter, r *http.Request) {
	// 解析URL: /api/sessions/{id}
	pathParts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/sessions/"), "/")
	if len(pathParts) == 0 || pathParts[0] == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}
	sessionID := pathParts[0]

	// 确保用户有ID
	userID := s.ensureUserID(w, r)

	switch r.Method {
	case http.MethodGet:
		// 获取会话历史
		session := s.sessionMgr.GetSession(sessionID)
		if session == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}

		// 验证会话属于当前用户
		if session.UserID != userID {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}

		// 从磁盘加载历史消息
		history, err := s.sessionMgr.LoadHistoryFromDisk(sessionID)
		if err != nil {
			http.Error(w, fmt.Sprintf("load history error: %v", err), http.StatusInternalServerError)
			return
		}

		// 转换历史消息为Web消息格式（去除元数据前缀）
		messages := make([]WebMessage, 0, len(history))
		for _, msg := range history {
			messages = append(messages, WebMessage{
				Role:       msg.Role,
				Content:    stripMetaPrefix(msg.Content),
				ToolCalls:  msg.ToolCalls,
				ToolCallID: msg.ToolCallID,
			})
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"session": map[string]interface{}{
				"id":         session.ID,
				"created_at": session.CreatedAt,
				"updated_at": session.UpdatedAt,
				"is_active":  session.IsActive,
			},
			"messages": messages,
		})

	case http.MethodDelete:
		// 验证会话属于当前用户
		session := s.sessionMgr.GetSession(sessionID)
		if session == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		if session.UserID != userID {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}

		if err := s.sessionMgr.DeleteSession(sessionID); err != nil {
			http.Error(w, fmt.Sprintf("delete error: %v", err), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
