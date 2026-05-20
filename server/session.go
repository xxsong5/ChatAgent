package server

import (
	"chatAgent/agent"
	"chatAgent/skill"
	"chatAgent/tool"
	"chatAgent/types"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/sashabaranov/go-openai"
)

// Session 一次完整的会话
// 会话记录在 cfg.SessionsDir / Session.ID 目录中
// 包括历史交互消息、压缩的记忆等
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"` // 用户ID，用于隔离不同用户的会话
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	IsActive  bool      `json:"is_active"`

	Agent  *agent.Agent       `json:"-"`
	ctx    context.Context    `json:"-"`
	cancel context.CancelFunc `json:"-"`
}

// SessionManager 会话管理器
// 负责会话的创建、查找、恢复、路由、清理等生命周期管理
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	cfg      *types.Config

	toolRegistry  *tool.Registry
	skillRegistry *skill.Registry

	stopCh    chan struct{} // 用于停止后台清理协程
	closeOnce sync.Once     // 确保 stopCh 只被关闭一次
}

// NewSessionManager 创建会话管理器
func NewSessionManager(cfg *types.Config, toolReg *tool.Registry, skillReg *skill.Registry) *SessionManager {
	m := &SessionManager{
		sessions:      make(map[string]*Session),
		cfg:           cfg,
		toolRegistry:  toolReg,
		skillRegistry: skillReg,
		stopCh:        make(chan struct{}),
	}

	// 启动后台协程，定期清理空闲超时的会话
	go m.sessionCleanLoop()

	return m
}

// GetSessionsDir 获取会话存储目录
func (m *SessionManager) GetSessionsDir() string {
	sessionsDir := m.cfg.SessionsDir
	if sessionsDir == "" {
		sessionsDir = "config/sessions"
	}
	return sessionsDir
}

// GetSessionDir 获取指定会话的存储目录
func (m *SessionManager) GetSessionDir(sessionID string) string {
	return filepath.Join(m.GetSessionsDir(), sessionID)
}

// CreateSession 创建新会话 (Case 3: 前端请求没有sessionID)
// 创建新的sessionID，创建新的agent循环，并启动运行
// userID 用于隔离不同用户的会话
func (m *SessionManager) CreateSession(ctx context.Context, userID string, systemPrompt string) (*Session, error) {
	sessionID := uuid.New().String()

	// 创建会话存储目录
	sessionDir := m.GetSessionDir(sessionID)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return nil, fmt.Errorf("创建会话目录失败: %w", err)
	}

	// 创建新的Agent
	myAgent, err := agent.NewAgent(sessionID, systemPrompt, m.toolRegistry, m.skillRegistry, m.cfg)
	if err != nil {
		return nil, fmt.Errorf("创建Agent失败: %w", err)
	}

	// 创建可取消的context
	// 使用 context.Background() 而非 ctx（HTTP请求上下文），
	// 确保会话生命周期独立于单个HTTP请求，避免后续LLM调用因请求结束而失败
	sessCtx, cancel := context.WithCancel(context.Background())

	session := &Session{
		ID:        sessionID,
		UserID:    userID,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		IsActive:  true,
		Agent:     myAgent,
		ctx:       sessCtx,
		cancel:    cancel,
	}

	// 启动Agent循环
	myAgent.Run(sessCtx)

	// 保存Session元信息
	if err := m.saveSessionMeta(session); err != nil {
		log.Warn().Err(err).Str("sessionID", sessionID).Msg("保存会话元信息失败")
	}

	// 注册到管理器
	m.mu.Lock()
	m.sessions[sessionID] = session
	m.mu.Unlock()

	log.Info().Str("sessionID", sessionID).Msg("创建新会话")
	return session, nil
}

// GetSession 获取会话
func (m *SessionManager) GetSession(id string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[id]
}

// GetOrCreateSession 获取或创建会话
// 如果sessionID存在且活跃，返回现有会话
// 如果sessionID存在但已不活跃，恢复会话
// 如果sessionID为空，创建新会话
// userID 用于隔离不同用户的会话
func (m *SessionManager) GetOrCreateSession(ctx context.Context, sessionID string, userID string, systemPrompt string) (*Session, error) {
	if sessionID == "" {
		// Case 3: 无sessionID，创建新会话
		return m.CreateSession(ctx, userID, systemPrompt)
	}

	session := m.GetSession(sessionID)
	if session != nil && session.IsActive {
		// Case 2: session对应的agent还在运行，直接返回
		return session, nil
	}

	if session != nil && !session.IsActive {
		// Session存在但已停止，需要重新激活
		return m.recoverSession(ctx, session, systemPrompt)
	}

	// Case 1: session不在内存中（可能因为时间久远已经丢失），尝试从磁盘恢复
	return m.loadSession(ctx, sessionID, systemPrompt)
}

// recoverSession 恢复已停止的会话
func (m *SessionManager) recoverSession(ctx context.Context, session *Session, systemPrompt string) (*Session, error) {
	log.Info().Str("sessionID", session.ID).Msg("恢复已停止的会话")

	// 创建新的Agent
	myAgent, err := agent.NewAgent(session.ID, systemPrompt, m.toolRegistry, m.skillRegistry, m.cfg)
	if err != nil {
		return nil, fmt.Errorf("恢复Agent失败: %w", err)
	}

	// 创建新的可取消context
	// 使用 context.Background() 而非 ctx（HTTP请求上下文），
	// 确保会话生命周期独立于单个HTTP请求
	sessCtx, cancel := context.WithCancel(context.Background())

	session.Agent = myAgent
	session.ctx = sessCtx
	session.cancel = cancel
	session.IsActive = true
	session.UpdatedAt = time.Now()

	// 启动Agent循环
	myAgent.Run(sessCtx)

	// 更新Session元信息
	if err := m.saveSessionMeta(session); err != nil {
		log.Warn().Err(err).Str("sessionID", session.ID).Msg("恢复会话时保存元信息失败")
	}

	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()

	return session, nil
}

// loadSession 从磁盘加载会话 (Case 1)
// 由于时间久远，session已经不在运行，需要reload历史记忆,并处理新消息
func (m *SessionManager) loadSession(ctx context.Context, sessionID string, systemPrompt string) (*Session, error) {
	log.Info().Str("sessionID", sessionID).Msg("从磁盘加载会话")

	// 从磁盘加载会话元信息
	sessionDir := m.GetSessionDir(sessionID)
	metaPath := filepath.Join(sessionDir, "session.json")

	session := &Session{ID: sessionID}
	if data, err := os.ReadFile(metaPath); err == nil {
		if err := json.Unmarshal(data, session); err != nil {
			log.Warn().Err(err).Str("sessionID", sessionID).Msg("解析会话元信息失败")
		}
	} else {
		log.Warn().Err(err).Str("sessionID", sessionID).Msg("读取会话元信息失败，使用默认值")
		session.CreatedAt = time.Now()
	}

	// 创建新的Agent
	myAgent, err := agent.NewAgent(sessionID, systemPrompt, m.toolRegistry, m.skillRegistry, m.cfg)
	if err != nil {
		return nil, fmt.Errorf("创建Agent失败: %w", err)
	}

	// 创建可取消context
	// 使用 context.Background() 而非 ctx（HTTP请求上下文），
	// 确保会话生命周期独立于单个HTTP请求
	sessCtx, cancel := context.WithCancel(context.Background())

	session.Agent = myAgent
	session.ctx = sessCtx
	session.cancel = cancel
	session.IsActive = true
	session.UpdatedAt = time.Now()

	// 启动Agent循环
	myAgent.Run(sessCtx)

	// 保存Session元信息
	if err := m.saveSessionMeta(session); err != nil {
		log.Warn().Err(err).Str("sessionID", sessionID).Msg("加载会话时保存元信息失败")
	}

	m.mu.Lock()
	m.sessions[sessionID] = session
	m.mu.Unlock()

	log.Info().Str("sessionID", sessionID).Msg("会话加载完成")
	return session, nil
}

// RouteMessage 路由用户消息到对应的session (消息路由核心功能)
// 根据sessionID找到对应的session，将消息投递到其agent循环
// 返回事件订阅channel，用于流式获取处理结果
// userID 用于隔离不同用户的会话
// metadata 可选，附加到消息中的额外元数据（如来源、附件信息、当前时间等），会传递给LLM
func (m *SessionManager) RouteMessage(ctx context.Context, sessionID string, userID string, message string, systemPrompt string, metadata ...map[string]interface{}) (*Session, chan agent.AgentEvent, error) {
	session, err := m.GetOrCreateSession(ctx, sessionID, userID, systemPrompt)
	if err != nil {
		return nil, nil, fmt.Errorf("获取会话失败: %w", err)
	}

	// 订阅Agent事件，用于流式输出
	eventCh := session.Agent.Subscribe()

	// 投递用户消息到Agent的输入通道（支持附带元数据）
	if len(metadata) > 0 && len(metadata[0]) > 0 {
		session.Agent.UserInputWithMeta(message, metadata[0])
	} else {
		session.Agent.UserInput(message)
	}

	// 更新会话时间
	session.UpdatedAt = time.Now()

	// 如果有第一条用户消息且标题为空，将用户消息截取作为会话标题
	if session.Title == "" {
		title := message
		if len([]rune(title)) > 30 {
			title = string([]rune(title)[:30]) + "..."
		}
		title = strings.ReplaceAll(title, "\n", " ")
		session.Title = title
	}

	return session, eventCh, nil
}

// StopSession 停止指定会话
// 关闭agent的事件总线、输入通道、释放sandbox资源
//func (m *SessionManager) StopSession(sessionID string) error {
//	m.mu.Lock()
//	session, exists := m.sessions[sessionID]
//	if !exists {
//		m.mu.Unlock()
//		return fmt.Errorf("会话不存在: %s", sessionID)
//	}
//
//	session.IsActive = false
//
//	// 停止Agent
//	session.Agent.Stop()
//
//	// 释放sandbox资源
//	session.Agent.ReleaseSandbox()
//
//	// 取消context
//	if session.cancel != nil {
//		session.cancel()
//	}
//
//	// 保存历史记录
//	if err := m.saveHistory(session); err != nil {
//		log.Warn().Err(err).Str("sessionID", sessionID).Msg("保存会话历史失败")
//	}
//
//	// 保存元信息
//	if err := m.saveSessionMeta(session); err != nil {
//		log.Warn().Err(err).Str("sessionID", sessionID).Msg("保存会话元信息失败")
//	}
//
//	m.mu.Unlock()
//
//	log.Info().Str("sessionID", sessionID).Msg("会话已停止")
//	return nil
//}

// DeleteSession 删除会话
// 停止并清理所有资源，并从管理器中移除
func (m *SessionManager) DeleteSession(sessionID string) error {
	m.mu.Lock()
	session, exists := m.sessions[sessionID]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("会话不存在: %s", sessionID)
	}

	// 停止Agent
	if session.IsActive {
		session.Agent.Stop()
		session.Agent.ReleaseSandbox()
		if session.cancel != nil {
			session.cancel()
		}
	}

	delete(m.sessions, sessionID)
	m.mu.Unlock()

	log.Info().Str("sessionID", sessionID).Msg("会话已删除")
	return nil
}

// ListSessions 列出所有活跃会话
func (m *SessionManager) ListSessions() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	return sessions
}

// ActiveCount 获取活跃会话数量
func (m *SessionManager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// Shutdown 关闭管理器，停止所有会话
func (m *SessionManager) Shutdown() {
	m.closeOnce.Do(func() {
		close(m.stopCh)
	})

	m.mu.Lock()
	defer m.mu.Unlock()

	for id, session := range m.sessions {
		if session.IsActive {
			session.Agent.Stop()
			session.Agent.ReleaseSandbox()
			if session.cancel != nil {
				session.cancel()
			}
		}
		// 保存历史
		if err := m.saveHistory(session); err != nil {
			log.Warn().Err(err).Str("sessionID", id).Msg("关闭时保存会话历史失败")
		}
		if err := m.saveSessionMeta(session); err != nil {
			log.Warn().Err(err).Str("sessionID", id).Msg("关闭时保存会话元信息失败")
		}
	}

	m.sessions = make(map[string]*Session)
	log.Info().Msg("会话管理器已关闭")
}

// ========== 持久化方法 ==========

// saveSessionMeta 保存会话元信息到磁盘
func (m *SessionManager) saveSessionMeta(session *Session) error {
	sessionDir := m.GetSessionDir(session.ID)
	metaPath := filepath.Join(sessionDir, "session.json")

	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化会话元信息失败: %w", err)
	}

	if err := os.WriteFile(metaPath, data, 0644); err != nil {
		return fmt.Errorf("写入会话元信息文件失败: %w", err)
	}

	return nil
}

// saveHistory 保存会话历史消息到磁盘
// 保存时排除system消息，保留user/assistant/tool消息
func (m *SessionManager) saveHistory(session *Session) error {
	if session.Agent == nil {
		return nil
	}

	// Agent结构中的saveHistory是TODO，这里通过session层来保存
	// 会话历史保存路径: {sessionsDir}/{sessionID}/history.json
	sessionDir := m.GetSessionDir(session.ID)
	historyPath := filepath.Join(sessionDir, "history.json")

	// 从session的Agent中获取messages
	// 由于Agent的messages是内部变量，我们在session层通过事件保存记录
	// 这里创建一个历史记录文件占位，实际消息记录由Agent.saveHistory在loop中调用
	if _, err := os.Stat(historyPath); os.IsNotExist(err) {
		// 创建空的历史记录文件
		initialHistory := []map[string]interface{}{
			{
				"session_id": session.ID,
				"created_at": session.CreatedAt,
				"updated_at": session.UpdatedAt,
				"status":     "completed",
			},
		}
		data, err := json.MarshalIndent(initialHistory, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(historyPath, data, 0644); err != nil {
			return fmt.Errorf("写入历史记录文件失败: %w", err)
		}
	}

	return nil
}

// LoadHistoryFromDisk 从磁盘加载指定会话的历史消息
// 用于在恢复会话时，将历史消息注入到agent的消息列表中
// 返回历史消息列表，调用方可以将其作为初始上下文
func (m *SessionManager) LoadHistoryFromDisk(sessionID string) ([]openai.ChatCompletionMessage, error) {
	sessionDir := m.GetSessionDir(sessionID)
	historyPath := filepath.Join(sessionDir, "history.json")

	data, err := os.ReadFile(historyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []openai.ChatCompletionMessage{}, nil
		}
		return nil, fmt.Errorf("读取历史记录文件失败: %w", err)
	}

	var messages []openai.ChatCompletionMessage
	if err := json.Unmarshal(data, &messages); err != nil {
		return nil, fmt.Errorf("解析历史记录失败: %w", err)
	}

	return messages, nil
}

// ========== 清理和超时管理 ==========

// sessionCleanLoop 后台协程，定期检查并清理空闲超时的会话
func (m *SessionManager) sessionCleanLoop() {
	ttl := m.cfg.GetSessionTTL()
	// 每 TTL/2 的时间间隔执行一次清理，确保及时回收
	interval := ttl / 2
	if interval < time.Second {
		interval = 30 * time.Second
	}

	log.Info().Dur("ttl", ttl).Dur("interval", interval).Msg("会话空闲清理协程已启动")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			log.Info().Msg("会话空闲清理协程已停止")
			return
		case <-ticker.C:
			cleaned := m.CleanStaleSessions(ttl)
			if len(cleaned) > 0 {
				log.Info().Strs("cleaned", cleaned).Msg("已清理空闲超时会话")
			}
		}
	}
}

// CleanStaleSessions 清理长期不活跃的会话
// 超过指定duration未更新的会话将被停止并释放资源
func (m *SessionManager) CleanStaleSessions(maxIdle time.Duration) []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var cleaned []string
	now := time.Now()

	for id, session := range m.sessions {
		if session.IsActive && now.After(session.UpdatedAt.Add(maxIdle)) {
			log.Info().Str("sessionID", id).
				Time("updatedAt", session.UpdatedAt).
				Msg("清理不活跃会话")

			session.Agent.Stop()
			session.Agent.ReleaseSandbox()
			if session.cancel != nil {
				session.cancel()
			}
			session.IsActive = false

			if err := m.saveHistory(session); err != nil {
				log.Warn().Err(err).Str("sessionID", id).Msg("清理时保存会话历史失败")
			}
			if err := m.saveSessionMeta(session); err != nil {
				log.Warn().Err(err).Str("sessionID", id).Msg("清理时保存会话元信息失败")
			}

			cleaned = append(cleaned, id)
		}
	}

	if len(cleaned) > 0 {
		for _, id := range cleaned {
			delete(m.sessions, id)
		}
	}

	return cleaned
}
