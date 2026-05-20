// internal/mcp/transport/stdio.go
package transport

import (
	"bufio"
	"chatAgent/types"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/rs/zerolog/log"
)

// StdioTransport stdio 传输实现
type StdioTransport struct {
	config *types.MCPConfig

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr io.ReadCloser

	connected bool
	mu        sync.Mutex

	// 消息处理
	pending   map[RPCID]chan *JSONRPCMessage
	pendingMu sync.Mutex

	// 通知处理器
	notificationHandler func(method string, params []byte)

	// 关闭信号
	done chan struct{}
}

// NewStdioTransport 创建 stdio 传输
func NewStdioTransport(config *types.MCPConfig) *StdioTransport {
	return &StdioTransport{
		config:  config,
		pending: make(map[RPCID]chan *JSONRPCMessage),
		done:    make(chan struct{}),
	}
}

// Connect 启动子进程并建立连接
func (t *StdioTransport) Connect(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.connected {
		return nil
	}

	// 创建命令
	t.cmd = exec.CommandContext(ctx, t.config.Command, t.config.Args...)

	// 设置环境变量
	t.cmd.Env = os.Environ()
	for k, v := range t.config.Env {
		// 支持环境变量引用
		expandedV := os.ExpandEnv(v)
		t.cmd.Env = append(t.cmd.Env, fmt.Sprintf("%s=%s", k, expandedV))
		log.Debug().Str("env", fmt.Sprintf("%s=%s", k, expandedV)).Msg("设置环境变量")
	}

	// 获取 stdin/stdout/stderr
	var err error
	t.stdin, err = t.cmd.StdinPipe()
	if err != nil {
		log.Error().Err(err).Msg("获取stdin失败")
		return fmt.Errorf("获取stdin失败: %w", err)
	}

	stdout, err := t.cmd.StdoutPipe()
	if err != nil {
		log.Error().Err(err).Msg("获取stdout失败")
		return fmt.Errorf("获取stdout失败: %w", err)
	}
	t.stdout = bufio.NewReader(stdout)

	t.stderr, err = t.cmd.StderrPipe()
	if err != nil {
		log.Error().Err(err).Msg("获取stderr失败")
		return fmt.Errorf("获取stderr失败: %w", err)
	}

	// 启动进程
	if err := t.cmd.Start(); err != nil {
		log.Error().Err(err).Msg("启动进程失败")
		return fmt.Errorf("启动进程失败: %w", err)
	}

	t.connected = true

	// 启动消息读取协程
	go t.readLoop()

	// 启动 stderr 读取协程（日志）
	go t.readStderr()

	return nil
}

// readLoop 读取消息循环
func (t *StdioTransport) readLoop() {
	for {
		select {
		case <-t.done:
			return
		default:
		}

		// 读取一行
		line, err := t.stdout.ReadBytes('\n')

		if err != nil {
			if err == io.EOF {
				t.mu.Lock()
				t.connected = false
				t.mu.Unlock()
			}
			return
		}

		// 解析 JSON-RPC 消息
		var msg JSONRPCMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			log.Error().Err(err).Msg("[MCP] 解析消息失败")
			continue
		}

		// 判断是请求/通知还是响应
		if msg.Method != "" && msg.ID == "" {
			// 通知
			if t.notificationHandler != nil {
				t.notificationHandler(msg.Method, msg.Params)
			}
		} else if msg.ID != "" {
			// 响应
			t.pendingMu.Lock()
			if ch, ok := t.pending[msg.ID]; ok {
				ch <- &msg
				delete(t.pending, msg.ID)
			}
			t.pendingMu.Unlock()
		}
	}
}

// readStderr 读取 stderr（日志）
func (t *StdioTransport) readStderr() {
	reader := bufio.NewReader(t.stderr)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		log.Info().Str("server", t.config.Name).Str("output", line).Msg("[MCP Server]")
	}
}

// Send 发送消息并等待响应
func (t *StdioTransport) Send(ctx context.Context, msg *JSONRPCMessage) error {
	t.mu.Lock()
	if !t.connected {
		t.mu.Unlock()
		log.Error().Msg("未连接")
		return fmt.Errorf("未连接")
	}
	t.mu.Unlock()

	// 序列化
	data, err := json.Marshal(msg)
	if err != nil {
		log.Error().Err(err).Msg("序列化失败")
		return fmt.Errorf("序列化失败: %w", err)
	}

	// 发送（添加换行符）
	if _, err := t.stdin.Write(append(data, '\n')); err != nil {
		log.Error().Err(err).Msg("发送失败")
		return fmt.Errorf("发送失败: %w", err)
	}

	return nil
}

// SendRequest 发送请求并等待响应
func (t *StdioTransport) SendRequest(ctx context.Context, msg *JSONRPCMessage) (*JSONRPCMessage, error) {
	// 创建响应通道
	respCh := make(chan *JSONRPCMessage, 1)

	t.pendingMu.Lock()
	t.pending[msg.ID] = respCh
	t.pendingMu.Unlock()

	// 发送请求
	if err := t.Send(ctx, msg); err != nil {
		t.pendingMu.Lock()
		delete(t.pending, msg.ID)
		t.pendingMu.Unlock()
		log.Error().Err(err).Msg("发送请求失败")
		return nil, err
	}

	// 等待响应
	select {
	case resp := <-respCh:
		return resp, nil
	case <-ctx.Done():
		t.pendingMu.Lock()
		delete(t.pending, msg.ID)
		t.pendingMu.Unlock()
		return nil, ctx.Err()
	}
}

// Close 关闭连接
func (t *StdioTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.connected {
		log.Error().Msg("未连接，无法关闭")
		return nil
	}

	close(t.done)

	if t.stdin != nil {
		t.stdin.Close()
	}

	if t.cmd != nil && t.cmd.Process != nil {
		t.cmd.Process.Kill()
		t.cmd.Wait()
		log.Info().Str("server", t.config.Name).Msg("[MCP] 进程已关闭")
	}

	t.connected = false
	return nil
}

// OnNotification 设置通知处理器
func (t *StdioTransport) OnNotification(handler func(method string, params []byte)) {
	t.notificationHandler = handler
}
