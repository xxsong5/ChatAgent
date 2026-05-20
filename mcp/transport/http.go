// internal/mcp/transport/http.go
package transport

import (
	"bufio"
	"bytes"
	"chatAgent/types"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// HTTPTransport Streamable HTTP 传输实现
type HTTPTransport struct {
	config    *types.MCPConfig
	client    *http.Client
	sessionID string

	connected bool
	mu        sync.Mutex

	// SSE 连接
	sseConn   io.ReadCloser
	sseReader *bufio.Reader

	// 通知处理器
	notificationHandler func(method string, params []byte)

	// 关闭信号
	done chan struct{}
}

// NewHTTPTransport 创建 HTTP 传输
func NewHTTPTransport(config *types.MCPConfig) *HTTPTransport {
	timeout := time.Duration(config.Timeout) * time.Second
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &HTTPTransport{
		config: config,
		client: &http.Client{
			Timeout: timeout,
		},
		done: make(chan struct{}),
	}
}

// Connect 建立连接
func (t *HTTPTransport) Connect(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.connected {
		return nil
	}

	t.connected = true
	return nil
}

// Send 发送消息（POST）
func (t *HTTPTransport) Send(ctx context.Context, msg *JSONRPCMessage) error {
	_, err := t.SendRequest(ctx, msg)
	return err
}

// SendRequest 发送请求并获取响应
func (t *HTTPTransport) SendRequest(ctx context.Context, msg *JSONRPCMessage) (*JSONRPCMessage, error) {
	// 序列化请求
	reqBody, err := json.Marshal(msg)
	if err != nil {
		log.Error().Err(err).Msg("序列化请求失败")
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	// 创建 HTTP 请求
	req, err := http.NewRequestWithContext(ctx, "POST", t.config.URL, bytes.NewReader(reqBody))
	if err != nil {
		log.Error().Err(err).Str("url", t.config.URL).Msg("创建请求失败")
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	// 设置请求头
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	// 设置配置的请求头（支持环境变量）
	for k, v := range t.config.Headers {
		expandedV := os.ExpandEnv(v)
		req.Header.Set(k, expandedV)
	}

	// 设置 Session ID（如果有）
	if t.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", t.sessionID)
	}

	// 发送请求
	resp, err := t.client.Do(req)
	if err != nil {
		log.Error().Err(err).Msg("发送请求失败")
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 保存 Session ID
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.sessionID = sid
	}

	// 检查状态码
	if resp.StatusCode == http.StatusAccepted {
		// 202 Accepted（用于通知/响应）
		return nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Error().Int("status", resp.StatusCode).Str("body", string(body)).Msg("HTTP错误")
		return nil, fmt.Errorf("HTTP错误 %d: %s", resp.StatusCode, string(body))
	}

	// 检查内容类型
	contentType := resp.Header.Get("Content-Type")

	if strings.Contains(contentType, "text/event-stream") {
		// SSE 流响应
		return t.handleSSEResponse(resp.Body, msg.ID)
	}

	// JSON 响应
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Error().Err(err).Msg("读取响应失败")
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	var result JSONRPCMessage
	if err := json.Unmarshal(body, &result); err != nil {
		log.Error().Err(err).Msg("解析响应失败")
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	return &result, nil
}

// handleSSEResponse 处理 SSE 流响应
func (t *HTTPTransport) handleSSEResponse(body io.Reader, expectedID interface{}) (*JSONRPCMessage, error) {
	reader := bufio.NewReader(body)

	var dataBuffer strings.Builder

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Error().Err(err).Msg("读取SSE失败")
			return nil, fmt.Errorf("读取SSE失败: %w", err)
		}

		line = strings.TrimSpace(line)

		if line == "" {
			// 空行表示事件结束
			if dataBuffer.Len() > 0 {
				data := dataBuffer.String()
				dataBuffer.Reset()

				var msg JSONRPCMessage
				if err := json.Unmarshal([]byte(data), &msg); err != nil {
					continue
				}

				// 检查是否是我们等待的响应
				if msg.ID == expectedID {
					return &msg, nil
				}

				// 处理通知
				if msg.Method != "" && t.notificationHandler != nil {
					t.notificationHandler(msg.Method, msg.Params)
				}
			}
			continue
		}

		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimSpace(data)
			dataBuffer.WriteString(data)
		}
	}

	return nil, fmt.Errorf("未收到预期响应")
}

// OpenSSEStream 打开 SSE 流（用于接收服务器通知）
func (t *HTTPTransport) OpenSSEStream(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", t.config.URL, nil)
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}

	req.Header.Set("Accept", "text/event-stream")

	for k, v := range t.config.Headers {
		expandedV := os.ExpandEnv(v)
		req.Header.Set(k, expandedV)
	}

	if t.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", t.sessionID)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("打开SSE失败: %w", err)
	}

	if resp.StatusCode == http.StatusMethodNotAllowed {
		resp.Body.Close()
		return nil // 服务器不支持 SSE 流
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("SSE连接失败: %d", resp.StatusCode)
	}

	t.sseConn = resp.Body
	t.sseReader = bufio.NewReader(resp.Body)

	// 启动 SSE 读取协程
	go t.readSSELoop()

	return nil
}

// readSSELoop 读取 SSE 消息
func (t *HTTPTransport) readSSELoop() {
	if t.sseReader == nil {
		return
	}

	var dataBuffer strings.Builder

	for {
		select {
		case <-t.done:
			return
		default:
		}

		line, err := t.sseReader.ReadString('\n')
		if err != nil {
			return
		}

		line = strings.TrimSpace(line)

		if line == "" {
			if dataBuffer.Len() > 0 {
				data := dataBuffer.String()
				dataBuffer.Reset()

				var msg JSONRPCMessage
				if err := json.Unmarshal([]byte(data), &msg); err != nil {
					log.Error().Err(err).Msg("解析SSE消息失败")
					continue
				}

				if msg.Method != "" && t.notificationHandler != nil {
					t.notificationHandler(msg.Method, msg.Params)
				}
			}
			continue
		}

		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			dataBuffer.WriteString(strings.TrimSpace(data))
		}
	}
}

// Close 关闭连接
func (t *HTTPTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.connected {
		return nil
	}

	close(t.done)

	if t.sseConn != nil {
		t.sseConn.Close()
	}

	t.connected = false
	return nil
}

// OnNotification 设置通知处理器
func (t *HTTPTransport) OnNotification(handler func(method string, params []byte)) {
	t.notificationHandler = handler
}
