package transport

import (
	"context"
)

// Transport 传输层接口
type Transport interface {
	// Connect 建立连接
	Connect(ctx context.Context) error

	// Close 关闭连接
	Close() error

	// Send 发送消息
	Send(ctx context.Context, msg *JSONRPCMessage) error

	// SendRequest 发送请求并获取响应
	SendRequest(ctx context.Context, msg *JSONRPCMessage) (*JSONRPCMessage, error)

	// OnNotification 设置通知处理器
	OnNotification(handler func(method string, params []byte))
}
