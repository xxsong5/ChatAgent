package sandbox

import (
	"chatAgent/types"
	"context"
	"fmt"
	"time"
)

// Container 容器接口
type Container interface {
	// ID 返回容器ID
	ID() string

	// Status 获取容器状态
	Status(ctx context.Context) (ContainerStatus, error)

	// Exec 在容器中执行命令
	Exec(ctx context.Context, cmd string) (string, error)

	// CopyFile 从容器中复制文件到本地路径
	CopyFile(ctx context.Context, srcPath, dstPath string) error

	// IsHealthy 检查容器是否健康
	IsHealthy(ctx context.Context) bool

	// IsAllocated 检查是否已被分配
	IsAllocated() bool

	// SetAllocated 设置分配状态
	SetAllocated(allocated bool)

	// GetAllocTime 获取分配时间
	GetAllocTime() time.Time
}

// ContainerStatus 容器状态
type ContainerStatus string

const (
	StatusRunning ContainerStatus = "running"
	StatusStopped ContainerStatus = "stopped"
	StatusError   ContainerStatus = "error"
)

// ContainerInstance 容器实例
type ContainerInstance struct {
	containerID string    // 容器ID (Docker container ID)
	Name        string    // 容器名称
	Allocated   bool      // 是否已被分配使用
	AllocTime   time.Time // 分配时间

	client *DockerClient       // Docker客户端引用
	config types.SandboxConfig // 容器配置
}

// ID 返回容器ID
func (c *ContainerInstance) ID() string {
	return c.containerID
}

// Status 获取容器状态
func (c *ContainerInstance) Status(ctx context.Context) (ContainerStatus, error) {
	return c.client.GetContainerStatus(ctx, c.containerID)
}

// IsHealthy 检查容器是否健康
func (c *ContainerInstance) IsHealthy(ctx context.Context) bool {
	status, err := c.Status(ctx)
	if err != nil {
		return false
	}
	return status == StatusRunning
}

// IsAllocated 检查是否已被分配
func (c *ContainerInstance) IsAllocated() bool {
	return c.Allocated
}

// SetAllocated 设置分配状态
func (c *ContainerInstance) SetAllocated(allocated bool) {
	c.Allocated = allocated
	if allocated {
		c.AllocTime = time.Now()
	}
}

// GetAllocTime 获取分配时间
func (c *ContainerInstance) GetAllocTime() time.Time {
	return c.AllocTime
}

// Exec 在容器中执行命令
func (c *ContainerInstance) Exec(ctx context.Context, cmd string) (string, error) {
	// 从配置中获取超时时间，默认10秒
	timeout := time.Duration(c.config.ExecTimeout) * time.Second
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	stdout, stderr, err := c.client.ExecInContainer(ctx, c.containerID, cmd, timeout)

	output := "--- Output ---"

	if err != nil {
		output += fmt.Sprintf("\n[ExitCode: error] %v", err)
	}

	// 添加标准输出
	if stdout != "" {
		output += "\n" + stdout
	}
	// 添加标准错误输出
	if stderr != "" {
		output += "\n" + stderr
	}
	output += "\n--- Output End ---\n"

	return output, err
}

// CopyFile 从容器中复制文件到本地路径
// 使用Docker的CopyFromContainer API，通过tar流直接提取文件
func (c *ContainerInstance) CopyFile(ctx context.Context, srcPath, dstPath string) error {
	return c.client.CopyFileFromContainer(ctx, c.containerID, srcPath, dstPath)
}
