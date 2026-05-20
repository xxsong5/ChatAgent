package agent

import (
	"chatAgent/sandbox"
	"chatAgent/skill"
	"chatAgent/tool"
	"chatAgent/types"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sashabaranov/go-openai"
)

// UserInputData 用户输入数据，包含消息内容和可选的元数据
// 元数据可携带：来源(source)、用户ID、附件信息等附加上下文
type UserInputData struct {
	Content  string                 `json:"content"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// Agent AI Agent核心结构
type Agent struct {
	sessionID     string
	toolRegistry  *tool.Registry
	skillRegistry *skill.Registry
	systemPrompt  string
	maxIterations int

	llmClient *openai.Client
	cfg       *types.Config

	sandboxManager  *sandbox.Manager
	sandboxInstance sandbox.Container

	// 事件通道
	inputChan     chan UserInputData
	eventChan     chan AgentEvent
	subscribeCh   chan chan AgentEvent
	unsubscribeCh chan chan AgentEvent

	// 同步控制
	wg sync.WaitGroup
}

// NewAgent 创建Agent
func NewAgent(id, systemPrompt string, toolReg *tool.Registry, skillReg *skill.Registry, cfg *types.Config) (*Agent, error) {

	client := NewOpenAIClient(cfg.APIKey, cfg.BaseURL, cfg.Timeout)
	sandboxManager, err := sandbox.GetManager()
	if err != nil {
		return nil, err
	}

	agent := &Agent{
		sessionID:      id,
		toolRegistry:   toolReg,
		skillRegistry:  skillReg,
		systemPrompt:   systemPrompt,
		maxIterations:  cfg.MaxIterations,
		llmClient:      client,
		cfg:            cfg,
		sandboxManager: sandboxManager,
		inputChan:      make(chan UserInputData, 5),

		eventChan:     make(chan AgentEvent, 64),
		subscribeCh:   make(chan chan AgentEvent, 8),
		unsubscribeCh: make(chan chan AgentEvent, 8),
	}
	go agent.startEventBus()

	// 分配容器
	if sandboxManager != nil {
		instance, err := sandboxManager.Allocate(context.Background())
		if err != nil {
			return nil, err
		}
		agent.sandboxInstance = instance
	}

	return agent, nil
}

// ReleaseSandbox 释放sandbox容器
func (a *Agent) ReleaseSandbox() {
	if a.sandboxInstance != nil && a.sandboxManager != nil {
		a.sandboxManager.Release(a.sandboxInstance)
	}
}

// NewOpenAIClient 创建OpenAI客户端
func NewOpenAIClient(apiKey, baseURL string, timeoutSeconds int) *openai.Client {
	cfg := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}
	if timeoutSeconds > 0 {
		cfg.HTTPClient = &http.Client{
			Timeout: time.Duration(timeoutSeconds) * time.Second,
		}
	}

	return openai.NewClientWithConfig(cfg)
}

// startEventBus 启动内部事件总线，将事件分发到所有订阅者
func (a *Agent) startEventBus() {
	subscribers := make(map[chan AgentEvent]struct{})
	for {
		select {
		case event, ok := <-a.eventChan:
			if !ok {
				return
			}
			for ch := range subscribers {
				// 非阻塞发送，避免慢消费者阻塞事件分发
				select {
				case ch <- event:
				default:
				}
			}
		case ch := <-a.subscribeCh:
			subscribers[ch] = struct{}{}
		case ch := <-a.unsubscribeCh:
			delete(subscribers, ch)
			close(ch)
		}
	}
}

// UserInput 用户输入请求 Request Message（最简接口，仅传入文本）
func (a *Agent) UserInput(input string) {
	a.inputChan <- UserInputData{Content: input}
}

// UserInputWithMeta 用户输入请求，附带元数据
// metadata 可用于传递来源信息、用户ID、附件等附加上下文
func (a *Agent) UserInputWithMeta(input string, metadata map[string]interface{}) {
	a.inputChan <- UserInputData{Content: input, Metadata: metadata}
}

// Subscribe 订阅Agent事件，返回一个接收事件的channel

func (a *Agent) Subscribe() chan AgentEvent {
	ch := make(chan AgentEvent, 64)
	a.subscribeCh <- ch
	return ch
}

// Unsubscribe 取消订阅
func (a *Agent) Unsubscribe(ch chan AgentEvent) {
	a.unsubscribeCh <- ch
}

// emitEvent 发送一个事件到事件总线
func (a *Agent) emitEvent(event AgentEvent) {
	select {
	case a.eventChan <- event:
	default:
	}
}

// Stop 停止Agent处理
func (a *Agent) Stop() {
	close(a.eventChan)
	close(a.inputChan)
}

// Wait 等待Agent的loop goroutine结束
func (a *Agent) Wait() {
	a.wg.Wait()
}

// saveHistory 保存会话历史消息到磁盘
// 排除system消息，保存user/assistant/tool消息
func (a *Agent) saveHistory(messages []openai.ChatCompletionMessage) {
	sessionDir := filepath.Join(a.cfg.SessionsDir, a.sessionID)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		log.Warn().Err(err).Str("sessionID", a.sessionID).Msg("创建会话目录失败")
		return
	}

	historyPath := filepath.Join(sessionDir, "history.json")

	// 过滤掉system消息，保留user/assistant/tool消息
	var history []openai.ChatCompletionMessage
	for _, msg := range messages {
		if msg.Role != openai.ChatMessageRoleSystem {
			history = append(history, msg)
		}
	}

	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		log.Warn().Err(err).Str("sessionID", a.sessionID).Msg("序列化历史消息失败")
		return
	}

	if err := os.WriteFile(historyPath, data, 0644); err != nil {
		log.Warn().Err(err).Str("sessionID", a.sessionID).Msg("写入历史消息文件失败")
	}
}

// loadMemory 从磁盘加载历史记忆
// 优先加载压缩记忆(memory.json)，若无则加载完整历史(history.json)
func (a *Agent) loadMemory() []openai.ChatCompletionMessage {
	sessionDir := filepath.Join(a.cfg.SessionsDir, a.sessionID)

	// 优先加载压缩记忆
	memoryPath := filepath.Join(sessionDir, "memory.json")
	if data, err := os.ReadFile(memoryPath); err == nil {
		var messages []openai.ChatCompletionMessage
		if err := json.Unmarshal(data, &messages); err == nil {
			log.Info().Str("sessionID", a.sessionID).Int("count", len(messages)).Msg("加载压缩记忆成功")
			return messages
		}
	}

	// 无压缩记忆，加载完整历史
	historyPath := filepath.Join(sessionDir, "history.json")
	if data, err := os.ReadFile(historyPath); err == nil {
		var messages []openai.ChatCompletionMessage
		if err := json.Unmarshal(data, &messages); err == nil {
			log.Info().Str("sessionID", a.sessionID).Int("count", len(messages)).Msg("加载历史消息成功")
			return messages
		}
	}

	log.Info().Str("sessionID", a.sessionID).Msg("无历史消息可加载")
	return nil
}

// compactHistory 压缩历史消息：将较旧的非工具调用消息压缩为摘要记忆
// 保留最近N轮交互的完整消息，将更早的消息压缩
// TODO: 调用LLM对早期消息进行摘要总结，保存为memory.json
func (a *Agent) compactHistory(messages []openai.ChatCompletionMessage) {
	const keepRecentRounds = 5 // 保留最近5轮交互

	if len(messages) <= keepRecentRounds*2 {
		return // 消息数量不多，不需要压缩
	}

	sessionDir := filepath.Join(a.cfg.SessionsDir, a.sessionID)

	// 将早期的消息（除system外）标记并准备压缩
	// 此处先不真正调用LLM做摘要，先保存原始数据到memory作为占位
	// TODO: 实际调用LLM对早期消息做摘要总结
	compressedPath := filepath.Join(sessionDir, "memory.json")

	// 保留最近的N轮交互和system消息
	cutIndex := len(messages) - keepRecentRounds*2
	var preserved []openai.ChatCompletionMessage
	preserved = append(preserved, messages[0]) // system message
	preserved = append(preserved, messages[cutIndex:]...)

	// 将完整的消息列表保存到history
	a.saveHistory(messages)

	// 将压缩后的消息保存到memory
	data, err := json.MarshalIndent(preserved, "", "  ")
	if err != nil {
		log.Warn().Err(err).Str("sessionID", a.sessionID).Msg("序列化压缩记忆失败")
		return
	}
	if err := os.WriteFile(compressedPath, data, 0644); err != nil {
		log.Warn().Err(err).Str("sessionID", a.sessionID).Msg("写入压缩记忆文件失败")
	}

	log.Info().Str("sessionID", a.sessionID).
		Int("original", len(messages)).
		Int("compressed", len(preserved)).
		Msg("历史消息已压缩")
}

// GetSessionID 获取会话ID
func (a *Agent) GetSessionID() string {
	return a.sessionID
}
