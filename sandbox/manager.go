package sandbox

import (
	"context"
	"fmt"
	"sync"

	"chatAgent/types"
)

var (
	defaultManager     *Manager
	defaultManagerErr  error
	defaultManagerOnce sync.Once
)

// Manager Sandbox统一管理器
type Manager struct {
	pool     *PoolManager
	client   *DockerClient
	config   types.SandboxConfig
	disabled bool
}

// GetManager sandboxManager (线程安全单例)
func GetManager() (*Manager, error) {
	defaultManagerOnce.Do(func() {
		sandboxManager, err := NewManager(types.GetDefaultConfig().Sandbox)
		if err != nil {
			defaultManagerErr = err
			return
		}

		if !sandboxManager.IsDisabled() {
			if err := sandboxManager.Init(context.Background()); err != nil {
				defaultManagerErr = err
				return
			}
			defaultManager = sandboxManager
		}
	})
	return defaultManager, defaultManagerErr
}

// DestroyManager destroy default manager
func DestroyManager() error {
	if defaultManager != nil {
		if err := defaultManager.Shutdown(context.Background()); err == nil {
			defaultManager = nil
		} else {
			return err
		}
	}
	return nil
}

// NewManager 创建Sandbox管理器
func NewManager(config types.SandboxConfig) (*Manager, error) {
	// 如果sandbox未启用，返回禁用状态的管理器
	if !config.Enabled {
		return &Manager{disabled: true}, nil
	}

	if config.Image == "" {
		return nil, fmt.Errorf("sandbox镜像未配置")
	}

	client, err := NewDockerClient()
	if err != nil {
		return nil, fmt.Errorf("创建Docker客户端失败: %w", err)
	}

	pool := NewPoolManager(client, config)

	return &Manager{
		pool:   pool,
		client: client,
		config: config,
	}, nil
}

// Init 初始化管理器
func (m *Manager) Init(ctx context.Context) error {
	if m.disabled {
		return nil
	}
	return m.pool.Init(ctx)
}

// Allocate 分配容器
func (m *Manager) Allocate(ctx context.Context) (Container, error) {
	if m.disabled {
		return nil, fmt.Errorf("sandbox已禁用")
	}
	return m.pool.Allocate(ctx)
}

// Release 释放容器
func (m *Manager) Release(instance Container) error {
	if m.disabled {
		return nil
	}
	return m.pool.Release(instance.(*ContainerInstance))
}

// GetStatus 获取状态
func (m *Manager) GetStatus() PoolStatus {
	if m.disabled {
		return PoolStatus{}
	}
	return m.pool.GetStatus()
}

// Shutdown 关闭管理器
func (m *Manager) Shutdown(ctx context.Context) error {
	if m.disabled {
		return nil
	}
	if err := m.pool.Shutdown(ctx); err != nil {
		return err
	}
	return m.client.Close()
}

// IsDisabled 检查是否禁用
func (m *Manager) IsDisabled() bool {
	return m.disabled
}
