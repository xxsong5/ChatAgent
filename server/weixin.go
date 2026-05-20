package server

import (
	"chatAgent/agent"
	"chatAgent/sandbox"
	"chatAgent/types"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	_ "embed"

	ilink "github.com/openilink/openilink-sdk-go"
	"github.com/rs/zerolog/log"
	qrcode "github.com/skip2/go-qrcode"
)

//go:embed webui/weixin.html
var weixinHTML string

// WeixinConnection 表示一个微信 Bot 连接
type WeixinConnection struct {
	ID        string             `json:"id"`        // 连接标识（用于管理多个微信账号）
	BotID     string             `json:"bot_id"`    // Bot ID
	UserID    string             `json:"user_id"`   // 登录用户的微信ID
	Connected bool               `json:"connected"` // 是否已连接
	Status    string             `json:"status"`    // disconnected | connecting | connected | error
	Error     string             `json:"error,omitempty"`
	LoginTime time.Time          `json:"login_time"`
	Client    *ilink.Client      `json:"-"` // SDK 客户端
	cancel    context.CancelFunc `json:"-"` // 用于取消该连接的上下文
}

// WeixinServer 微信消息服务器
// 使用 openilink-sdk-go 对接微信 iLink Bot API
// 支持多微信账号同时在线、扫码登录管理、消息自动回复
// 同时提供 HTTP API + Web SPA 页面进行管理
type WeixinServer struct {
	cfg        *types.Config
	sessionMgr *SessionManager

	server *http.Server // HTTP 管理 API 服务器

	mu          sync.RWMutex
	connections map[string]*WeixinConnection // key: connection ID
	lastQRCode  string                       // 最近生成的登录二维码（base64）

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// 用户微信ID到会话ID的映射，确保每个微信用户有独立的会话
	userSessions sync.Map
}

// NewWeixinServer 创建微信服务器
func NewWeixinServer(cfg *types.Config, sessionMgr *SessionManager) *WeixinServer {
	s := &WeixinServer{
		cfg:         cfg,
		sessionMgr:  sessionMgr,
		connections: make(map[string]*WeixinConnection),
	}
	return s
}

// Start 启动微信服务器
// 1. 启动 HTTP 管理 API（提供 SPA 页面和 REST 接口）
// 2. 如果配置了自动连接，则自动启动微信登录
// 3. 阻塞等待关闭信号
func (s *WeixinServer) Start() error {
	s.ctx, s.cancel = context.WithCancel(context.Background())

	mux := http.NewServeMux()

	// --- WeChat 管理 API ---
	mux.HandleFunc("/api/weixin/status", s.handleStatus)
	mux.HandleFunc("/api/weixin/connect", s.handleConnect)
	mux.HandleFunc("/api/weixin/disconnect", s.handleDisconnect)
	mux.HandleFunc("/api/weixin/connections", s.handleConnections)
	mux.HandleFunc("/api/weixin/connections/", s.handleConnectionByID)
	mux.HandleFunc("/api/weixin/qrcode", s.handleQRCode)
	mux.HandleFunc("/api/weixin/sessions/", s.handleWeixinSessions)

	stashDir := s.cfg.StashDir
	if stashDir == "" {
		stashDir = "config/stash"
	}
	mux.Handle("/stash/", http.StripPrefix("/stash/", http.FileServer(http.Dir(stashDir))))

	// --- 微信管理 SPA 页面 ---
	mux.HandleFunc("/weixin", s.handleWeixinIndex)
	mux.HandleFunc("/weixin/", s.handleWeixinIndex)

	// 静态资源（复用 webui 中的 js/css 库）
	mux.Handle("/assets/", http.FileServer(http.FS(webUISub)))

	addr := fmt.Sprintf("%s:%d", s.cfg.Server.Host, s.cfg.Server.Port)
	s.server = &http.Server{
		Addr:    addr,
		Handler: s.withCORS(mux),
	}

	log.Info().Str("addr", addr).Str("type", "weixin").Msg("启动微信服务器")
	fmt.Printf("\n  WeChat 管理页面: http://%s/weixin\n\n", addr)

	// 启动 HTTP 服务器
	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error().Err(err).Msg("微信HTTP服务器异常")
		}
	}()

	// 阻塞等待退出信号（Shutdown 时通过 cancel 触发）
	<-s.ctx.Done()
	return nil
}

// Shutdown 关闭微信服务器
func (s *WeixinServer) Shutdown() error {
	log.Info().Msg("正在关闭微信服务器...")

	// 取消所有微信连接
	s.mu.RLock()
	for _, conn := range s.connections {
		if conn.cancel != nil {
			conn.cancel()
		}
	}
	s.mu.RUnlock()

	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()

	// 关闭 HTTP 服务器
	if s.server != nil {
		s.server.Close()
	}

	s.sessionMgr.Shutdown()
	sandbox.DestroyManager()
	return nil
}

// ============================================================================
// HTTP 中间件
// ============================================================================

func (s *WeixinServer) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ============================================================================
// HTTP API: 微信连接管理
// ============================================================================

// handleStatus 返回整体运行状态
func (s *WeixinServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	total := len(s.connections)
	connected := 0
	for _, conn := range s.connections {
		if conn.Connected {
			connected++
		}
	}
	s.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":            "running",
		"server_type":       "weixin",
		"total_connections": total,
		"connected_count":   connected,
		"uptime":            time.Now().Format(time.RFC3339),
	})
}

// handleConnect 发起一个新的微信连接（生成二维码）
// POST /api/weixin/connect
// 请求体: {"id": "optional-connection-id"} 如果不提供则自动生成
func (s *WeixinServer) handleConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read error: %v", err), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req struct {
		ID string `json:"id"`
	}
	if len(body) > 0 {
		json.Unmarshal(body, &req)
	}

	connID := req.ID
	if connID == "" {
		connID = fmt.Sprintf("weixin_%d", time.Now().Unix())
	}

	// 检查是否已存在相同 id 的连接
	s.mu.RLock()
	if _, exists := s.connections[connID]; exists {
		s.mu.RUnlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "连接ID已存在", "id": connID})
		return
	}
	s.mu.RUnlock()

	// 创建连接记录
	conn := &WeixinConnection{
		ID:     connID,
		Status: "connecting",
	}
	s.mu.Lock()
	s.connections[connID] = conn
	s.mu.Unlock()

	// 异步启动登录流程
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.startWeixinConnection(s.ctx, connID)
	}()

	writeJSON(w, http.StatusOK, map[string]string{
		"id":     connID,
		"status": "connecting",
		"msg":    "正在启动微信登录流程，请稍后查看二维码",
	})
}

// handleDisconnect 断开指定连接
// POST /api/weixin/disconnect 请求体: {"id": "connection-id"}
func (s *WeixinServer) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("read error: %v", err), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.ID == "" {
		http.Error(w, `{"error":"缺少连接ID"}`, http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	conn, exists := s.connections[req.ID]
	if !exists {
		s.mu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "连接不存在"})
		return
	}

	if conn.cancel != nil {
		conn.cancel()
	}
	delete(s.connections, req.ID)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected", "id": req.ID})
}

// handleConnections 列出所有微信连接
// GET /api/weixin/connections
func (s *WeixinServer) handleConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.RLock()
	connList := make([]map[string]interface{}, 0, len(s.connections))
	for _, conn := range s.connections {
		connList = append(connList, map[string]interface{}{
			"id":         conn.ID,
			"bot_id":     conn.BotID,
			"user_id":    conn.UserID,
			"connected":  conn.Connected,
			"status":     conn.Status,
			"error":      conn.Error,
			"login_time": conn.LoginTime,
		})
	}
	s.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"connections": connList,
		"count":       len(connList),
	})
}

// handleConnectionByID 查询单个连接详情
// GET /api/weixin/connections/{id}
func (s *WeixinServer) handleConnectionByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	connID := strings.TrimPrefix(r.URL.Path, "/api/weixin/connections/")
	if connID == "" {
		http.Error(w, "missing connection id", http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	conn, exists := s.connections[connID]
	s.mu.RUnlock()

	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "连接不存在"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":         conn.ID,
		"bot_id":     conn.BotID,
		"user_id":    conn.UserID,
		"connected":  conn.Connected,
		"status":     conn.Status,
		"error":      conn.Error,
		"login_time": conn.LoginTime,
	})
}

// handleQRCode 获取最近一次的登录二维码
// GET /api/weixin/qrcode?conn_id=xxx
func (s *WeixinServer) handleQRCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	connID := r.URL.Query().Get("conn_id")

	// 尝试从指定的连接获取二维码
	if connID != "" {
		s.mu.RLock()
		conn, exists := s.connections[connID]
		s.mu.RUnlock()

		if !exists {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "连接不存在"})
			return
		}
		if conn.Connected {
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"connected": true,
				"msg":       "已登录，无需二维码",
			})
			return
		}
	}

	if s.lastQRCode == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "暂无二维码"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"qrcode": s.lastQRCode,
		"msg":    "请使用微信扫描二维码登录",
	})
}

// handleWeixinSessions 微信用户的会话管理
// GET /api/weixin/sessions/{weixin_user_id} - 获取微信用户的消息历史
func (s *WeixinServer) handleWeixinSessions(w http.ResponseWriter, r *http.Request) {
	// 路径: /api/weixin/sessions/{weixin_user_id}
	pathParts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/weixin/sessions/"), "/")
	if len(pathParts) == 0 || pathParts[0] == "" {
		http.Error(w, "missing weixin user id", http.StatusBadRequest)
		return
	}
	weixinUserID := pathParts[0]

	switch r.Method {
	case http.MethodGet:
		// 获取微信用户的消息历史
		sessionID := "wx_" + sanitizeID(weixinUserID)
		session := s.sessionMgr.GetSession(sessionID)
		if session == nil {
			// 返回空历史
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"weixin_user_id": weixinUserID,
				"session_id":     sessionID,
				"messages":       []interface{}{},
			})
			return
		}

		history, err := s.sessionMgr.LoadHistoryFromDisk(sessionID)
		if err != nil {
			http.Error(w, fmt.Sprintf("load history error: %v", err), http.StatusInternalServerError)
			return
		}

		messages := make([]WebMessage, 0, len(history))
		for _, msg := range history {
			messages = append(messages, WebMessage{
				Role:    msg.Role,
				Content: stripMetaPrefix(msg.Content),
			})
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"weixin_user_id": weixinUserID,
			"session_id":     sessionID,
			"messages":       messages,
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ============================================================================
// SPA 页面
// ============================================================================

// handleWeixinIndex 提供微信管理 SPA 页面
func (s *WeixinServer) handleWeixinIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/weixin" && r.URL.Path != "/weixin/" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, weixinHTML)
}

// ============================================================================
// 微信连接生命周期
// ============================================================================

// startWeixinConnection 启动一个微信连接的完整生命周期
// 包括扫码登录和消息监听
func (s *WeixinServer) startWeixinConnection(ctx context.Context, connID string) {
	token := s.cfg.Server.Auth
	if token == "" {
		s.setConnectionError(connID, "未配置微信 Bot Token（请在 config server.apikey 中设置）")
		return
	}

	client := ilink.NewClient(token)

	// 创建可取消的子上下文
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	// 更新连接状态
	s.mu.Lock()
	conn, exists := s.connections[connID]
	if exists {
		conn.Client = client
		conn.cancel = connCancel
	}
	s.mu.Unlock()

	if !exists {
		return
	}

	// 扫码登录
	log.Info().Str("connID", connID).Msg("正在获取微信登录二维码...")

	result, err := client.LoginWithQR(connCtx, &ilink.LoginCallbacks{
		OnQRCode: func(img string) {
			s.onQRCode(connID, img)
		},
		OnScanned: func() {
			log.Info().Str("connID", connID).Msg("已扫码，请在微信上确认登录...")
			s.updateConnectionStatus(connID, "scanned")
		},
		OnExpired: func(attempt, max int) {
			log.Info().Str("connID", connID).Int("attempt", attempt).Int("max", max).Msg("二维码已过期，正在刷新...")
			s.updateConnectionStatus(connID, "refreshing_qrcode")
		},
	})
	if err != nil {
		if err == context.Canceled {
			s.setConnectionStatus(connID, "disconnected", "")
			return
		}
		errMsg := fmt.Sprintf("微信登录失败: %v", err)
		log.Error().Str("connID", connID).Err(err).Msg(errMsg)
		s.setConnectionError(connID, errMsg)
		return
	}
	if !result.Connected {
		s.setConnectionError(connID, "微信登录未返回连接状态")
		return
	}

	// 登录成功，更新连接信息
	s.mu.Lock()
	if c, ok := s.connections[connID]; ok {
		c.BotID = result.BotID
		c.UserID = result.UserID
		c.Connected = true
		c.Status = "connected"
		c.LoginTime = time.Now()
		c.Client = client
	}
	s.mu.Unlock()

	log.Info().
		Str("connID", connID).
		Str("botID", result.BotID).
		Str("userID", result.UserID).
		Msg("微信已成功登录")

	// 开始消息监听
	s.wg.Add(1)
	go s.monitorMessages(connCtx, connID, client)

	// 等待连接上下文取消
	<-connCtx.Done()

	// 连接已断开
	s.mu.Lock()
	if c, ok := s.connections[connID]; ok {
		c.Connected = false
		c.Status = "disconnected"
	}
	s.mu.Unlock()
	log.Info().Str("connID", connID).Msg("微信连接已断开")
}

// onQRCode 处理二维码事件：将URL链接生成二维码图片(base64 PNG)，供前端显示
func (s *WeixinServer) onQRCode(connID string, url string) {
	// 将URL生成二维码PNG图片
	png, err := qrcode.Encode(url, qrcode.Medium, 256)
	if err != nil {
		log.Warn().Err(err).Str("connID", connID).Msg("生成二维码图片失败，直接使用URL")
		s.lastQRCode = url
	} else {
		s.lastQRCode = base64.StdEncoding.EncodeToString(png)
	}

	s.mu.Lock()
	if conn, ok := s.connections[connID]; ok {
		conn.Status = "waiting_scan"
	}
	s.mu.Unlock()

	log.Info().Str("connID", connID).Msg("微信登录二维码已生成，请刷新网页查看")
}

// updateConnectionStatus 更新连接状态
func (s *WeixinServer) updateConnectionStatus(connID string, status string) {
	s.mu.Lock()
	if conn, ok := s.connections[connID]; ok {
		conn.Status = status
	}
	s.mu.Unlock()
}

// setConnectionStatus 设置连接状态并清理错误
func (s *WeixinServer) setConnectionStatus(connID string, status string, errMsg string) {
	s.mu.Lock()
	if conn, ok := s.connections[connID]; ok {
		conn.Status = status
		conn.Error = errMsg
		conn.Connected = (status == "connected")
	}
	s.mu.Unlock()
}

// setConnectionError 设置连接错误状态
func (s *WeixinServer) setConnectionError(connID string, errMsg string) {
	s.mu.Lock()
	if conn, ok := s.connections[connID]; ok {
		conn.Status = "error"
		conn.Error = errMsg
		conn.Connected = false
	}
	s.mu.Unlock()
	log.Error().Str("connID", connID).Msg(errMsg)
}

// ============================================================================
// 消息监听与处理
// ============================================================================

// monitorMessages 启动长轮询消息监听
func (s *WeixinServer) monitorMessages(ctx context.Context, connID string, client *ilink.Client) {
	defer s.wg.Done()

	bufFile := fmt.Sprintf("weixin_sync_buf_%s.dat", sanitizeID(connID))

	// 读取上次的同步游标
	var initialBuf string
	if data, err := os.ReadFile(bufFile); err == nil {
		initialBuf = string(data)
		log.Info().Str("connID", connID).Int("bufLen", len(initialBuf)).Msg("已加载微信同步游标")
	}

	err := client.Monitor(ctx, func(msg ilink.WeixinMessage) {
		s.wg.Add(1)
		go func(m ilink.WeixinMessage) {
			defer s.wg.Done()
			s.handleMessage(connID, m)
		}(msg)
	}, &ilink.MonitorOptions{
		InitialBuf: initialBuf,
		OnBufUpdate: func(buf string) {
			if err := os.WriteFile(bufFile, []byte(buf), 0600); err != nil {
				log.Warn().Err(err).Str("connID", connID).Msg("持久化微信同步游标失败")
			}
		},
		OnError: func(err error) {
			log.Error().Err(err).Str("connID", connID).Msg("微信消息监听异常")
		},
		OnSessionExpired: func() {
			log.Error().Str("connID", connID).Msg("微信会话已过期，需要重新登录")
			s.setConnectionStatus(connID, "session_expired", "会话已过期，请重新登录")
		},
	})

	if err != nil && err != context.Canceled {
		log.Error().Err(err).Str("connID", connID).Msg("微信消息监听已停止")
		s.setConnectionError(connID, fmt.Sprintf("消息监听停止: %v", err))
	}
}

// handleMessage 处理收到的单条微信消息
func (s *WeixinServer) handleMessage(connID string, msg ilink.WeixinMessage) {
	// 提取文本内容（支持引用消息和语音转文字）
	text := ilink.ExtractText(&msg)
	if text == "" {
		log.Info().
			Str("connID", connID).
			Str("fromUserID", msg.FromUserID).
			Str("msgType", fmt.Sprintf("%d", msg.MessageType)).
			Msg("收到非文本微信消息，忽略")
		return
	}

	// 提取消息中的媒体信息
	hasMedia := false
	mediaTypes := make([]string, 0)
	for _, item := range msg.ItemList {
		switch item.Type {
		case ilink.ItemImage:
			hasMedia = true
			mediaTypes = append(mediaTypes, "image")
		case ilink.ItemVoice:
			hasMedia = true
			mediaTypes = append(mediaTypes, "voice")
		case ilink.ItemFile:
			hasMedia = true
			mediaTypes = append(mediaTypes, "file")
		case ilink.ItemVideo:
			hasMedia = true
			mediaTypes = append(mediaTypes, "video")
		}
	}

	log.Info().
		Str("connID", connID).
		Str("fromUserID", msg.FromUserID).
		Str("text", truncateText(text, 60)).
		Bool("hasMedia", hasMedia).
		Msg("收到微信消息")

	// 获取或创建该用户的会话（使用微信用户 ID 作为会话标识）
	sessionID := s.getOrCreateSessionID(msg.FromUserID)

	// 构建附带消息
	extraInfo := text
	if hasMedia {
		extraInfo = fmt.Sprintf("%s\n[消息包含媒体类型: %s]", text, strings.Join(mediaTypes, ", "))
	}

	// 构建元数据
	metadata := map[string]interface{}{
		"source":      "weixin",
		"conn_id":     connID,
		"from_user":   msg.FromUserID,
		"weixin_msg":  true,
		"msg_type":    msg.MessageType,
		"has_media":   hasMedia,
		"media_types": mediaTypes,
		"timestamp":   time.Now().Format("2006-01-02 15:04:05") + " " + time.Now().Weekday().String(),
	}

	// 路由消息到 Agent 处理
	ctx := context.Background()
	session, eventCh, err := s.sessionMgr.RouteMessage(ctx, sessionID, msg.FromUserID, extraInfo, "", metadata)
	if err != nil {
		log.Error().Err(err).Str("fromUserID", msg.FromUserID).Msg("路由消息到Agent失败")
		return
	}

	// 收集 Agent 回复
	response := s.collectResponse(eventCh, session, msg.FromUserID)
	if response == "" {
		log.Warn().Str("fromUserID", msg.FromUserID).Msg("Agent回复内容为空")
		return
	}

	// 推送给微信用户（使用缓存的 contextToken）
	client := s.getClient(connID)
	if client == nil {
		log.Error().Str("connID", connID).Msg("无法获取微信客户端，发送失败")
		return
	}
	if _, err := client.Push(ctx, msg.FromUserID, response); err != nil {
		log.Error().Err(err).Str("fromUserID", msg.FromUserID).
			Str("connID", connID).
			Int("responseLen", len(response)).
			Msg("发送微信回复失败")
		return
	}

	log.Info().Str("fromUserID", msg.FromUserID).
		Str("connID", connID).
		Int("responseLen", len(response)).
		Msg("微信回复已发送")
}

// getClient 获取指定连接的微信客户端
func (s *WeixinServer) getClient(connID string) *ilink.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if conn, ok := s.connections[connID]; ok {
		return conn.Client
	}
	return nil
}

// collectResponse 收集 Agent 事件，组装完整回复文本
func (s *WeixinServer) collectResponse(eventCh chan agent.AgentEvent, session *Session, fromUserID string) string {
	var fullContent strings.Builder

	for event := range eventCh {
		switch event.Type {
		case agent.EventChunk:
			content, _ := event.Data.(string)
			fullContent.WriteString(content)

		case agent.EventFinal:
			// 已通过 EventChunk 收集

		case agent.EventRoundEnd:
			session.Agent.Unsubscribe(eventCh)
			goto collectDone

		case agent.EventToolCall:
			if data, ok := event.Data.(agent.ToolCallEventData); ok {
				argsStr := string(data.Args)
				log.Info().Str("fromUserID", fromUserID).
					Str("tool", data.Tool).
					Str("args", truncateText(argsStr, 200)).
					Msg("[collectResponse] 工具调用")
			}

		case agent.EventToolResult:
			if data, ok := event.Data.(agent.ToolResultEventData); ok {
				log.Debug().Str("fromUserID", fromUserID).
					Str("tool", data.Tool).
					Str("result", truncateText(data.Result, 200)).
					Msg("[collectResponse] 工具结果")
			}

		case agent.EventError:
			errData, _ := json.Marshal(event.Data)
			fullContent.WriteString(fmt.Sprintf("\n\n[错误: %s]", string(errData)))

		default:
			log.Debug().Str("fromUserID", fromUserID).
				Str("eventType", string(event.Type)).
				Msg("[collectResponse] 未处理的 Agent 事件类型")
		}
	}
collectDone:

	return strings.TrimSpace(fullContent.String())
}

// ============================================================================
// 会话管理
// ============================================================================

// getOrCreateSessionID 获取或创建微信用户对应的会话 ID
func (s *WeixinServer) getOrCreateSessionID(weixinUserID string) string {
	if id, ok := s.userSessions.Load(weixinUserID); ok {
		return id.(string)
	}

	sessionID := "wx_" + sanitizeID(weixinUserID)
	s.userSessions.Store(weixinUserID, sessionID)
	log.Info().Str("weixinUserID", weixinUserID).Str("sessionID", sessionID).Msg("为微信用户创建新会话")
	return sessionID
}

// ============================================================================
// 工具函数
// ============================================================================

// truncateText 截断文本到指定长度（用于日志显示）
func truncateText(text string, maxLen int) string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	return string(runes[:maxLen]) + "..."
}

// sanitizeID 清理字符串中的特殊字符，使其可用作文件路径或会话 ID
func sanitizeID(id string) string {
	id = strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_",
		"*", "_", "?", "_", "\"", "_",
		"<", "_", ">", "_", "|", "_",
		" ", "_",
	).Replace(id)

	if len(id) > 64 {
		id = id[:64]
	}
	return id
}
