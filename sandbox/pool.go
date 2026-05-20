package sandbox

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"chatAgent/types"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// PoolManager 容器池管理器
type PoolManager struct {
	client      *DockerClient
	config      types.SandboxConfig
	warmupPool  chan Container // 预热容器池
	allocated   map[string]Container
	mu          sync.Mutex
	totalCount  int64
	warmupCount int64
	shutdownCh  chan struct{}
	wg          sync.WaitGroup
}

// NewPoolManager 创建池管理器
func NewPoolManager(client *DockerClient, config types.SandboxConfig) *PoolManager {
	return &PoolManager{
		client:     client,
		config:     config,
		warmupPool: make(chan Container, config.MaxReplicas),
		allocated:  make(map[string]Container),
		shutdownCh: make(chan struct{}),
	}
}

// Init 初始化池管理器
func (pm *PoolManager) Init(ctx context.Context) error {
	// 设置默认值
	if pm.config.MountPath == "" {
		pm.config.MountPath = getWorkDir()
	}
	if pm.config.MaxReplicas == 0 {
		pm.config.MaxReplicas = 5
	}
	if pm.config.WarmupReplicas == 0 {
		pm.config.WarmupReplicas = 2
	}
	if pm.config.MemoryMB == 0 {
		pm.config.MemoryMB = 256
	}
	if pm.config.NanoCPUs == 0 {
		pm.config.NanoCPUs = 500_000_000
	}
	if pm.config.NetworkMode == "" {
		pm.config.NetworkMode = "bridge"
	}
	if pm.config.ContainerTTL == 0 {
		pm.config.ContainerTTL = 1800
	}
	if pm.config.StartTimeout == 0 {
		pm.config.StartTimeout = 30
	}
	if pm.config.ExecTimeout == 0 {
		pm.config.ExecTimeout = 10
	}

	log.Info().
		Str("image", pm.config.Image).
		Int64("warmupReplicas", pm.config.WarmupReplicas).
		Int64("maxReplicas", pm.config.MaxReplicas).
		Str("mountPath", pm.config.MountPath).
		Msg("初始化容器池管理器")

	// 拉取镜像
	if err := pm.client.PullImage(ctx, pm.config); err != nil {
		log.Warn().Err(err).Msg("拉取镜像失败，继续尝试创建容器")
	}

	// 预热容器
	for i := int64(0); i < pm.config.WarmupReplicas; i++ {
		instance, err := pm.createAndStartContainer(ctx)
		if err != nil {
			log.Error().Err(err).Int64("index", i).Msg("预热容器失败")
			continue
		}
		pm.warmupPool <- instance
		atomic.AddInt64(&pm.warmupCount, 1)
		atomic.AddInt64(&pm.totalCount, 1)
	}

	log.Info().
		Int64("warmupCount", pm.warmupCount).
		Int64("totalCount", pm.totalCount).
		Msg("容器池初始化完成")

	// 启动清理协程, 该功能应该迁移到session超时后,同步释放container
	//pm.wg.Add(1)
	//go pm.cleanupLoop()

	return nil
}

// getWorkDir 获取工作目录
func getWorkDir() string {
	return "."
}

// createAndStartContainer 创建并启动容器
func (pm *PoolManager) createAndStartContainer(ctx context.Context) (*ContainerInstance, error) {
	name := fmt.Sprintf("sandbox-%s", uuid.New().String()[:8])

	// 创建容器
	containerID, err := pm.client.CreateContainer(ctx, name, pm.config)
	if err != nil {
		return nil, fmt.Errorf("创建容器失败: %w", err)
	}

	// 启动容器
	if err := pm.client.StartContainer(ctx, containerID); err != nil {
		pm.client.RemoveContainer(ctx, containerID, true)
		return nil, fmt.Errorf("启动容器失败: %w", err)
	}

	// 等待容器就绪
	timeout := time.Duration(pm.config.StartTimeout) * time.Second
	if err := pm.client.WaitForHealthy(ctx, containerID, timeout); err != nil {
		log.Warn().Err(err).Str("container", containerID).Msg("等待容器健康超时")
	}

	return &ContainerInstance{
		containerID: containerID,
		Name:        name,
		Allocated:   false,
		client:      pm.client,
		config:      pm.config,
	}, nil
}

// Allocate 分配容器
func (pm *PoolManager) Allocate(ctx context.Context) (Container, error) {
	pm.mu.Lock()

	// 检查是否达到最大限制
	if atomic.LoadInt64(&pm.totalCount) >= pm.config.MaxReplicas {
		pm.mu.Unlock()
		return nil, fmt.Errorf("已达到最大容器数限制: %d", pm.config.MaxReplicas)
	}

	// share instance for all agents
	if pm.config.ShareReplica {
		if instance, ok := <-pm.warmupPool; ok {
			pm.warmupPool <- instance
			return instance, nil
		} else if pm.warmupCount < pm.totalCount {
			if instance, err := pm.createAndStartContainer(ctx); err == nil {
				pm.warmupPool <- instance
				atomic.AddInt64(&pm.warmupCount, 1)
				atomic.AddInt64(&pm.totalCount, 1)
				return instance, nil
			}
		}
		return nil, fmt.Errorf("warmupConatiner is empty")
	}

	// 尝试从预热池获取
	select {
	case instance := <-pm.warmupPool:
		instance.SetAllocated(true)
		pm.allocated[instance.ID()] = instance
		atomic.AddInt64(&pm.warmupCount, -1)
		pm.mu.Unlock()

		log.Debug().
			Str("id", instance.ID()).
			Msg("从预热池分配容器")
		return instance, nil
	default:
		// 预热池为空，创建新容器
		pm.mu.Unlock()

		// 再次检查（避免竞态）
		if atomic.LoadInt64(&pm.totalCount) >= pm.config.MaxReplicas {
			return nil, fmt.Errorf("已达到最大容器数限制: %d", pm.config.MaxReplicas)
		}

		instance, err := pm.createAndStartContainer(ctx)
		if err != nil {
			return nil, err
		}

		pm.mu.Lock()
		instance.SetAllocated(true)
		pm.allocated[instance.ID()] = instance
		atomic.AddInt64(&pm.totalCount, 1)
		pm.mu.Unlock()

		log.Debug().
			Str("id", instance.ID()).
			Msg("创建新容器")

		return instance, nil
	}
}

// Release 释放容器回预热池
func (pm *PoolManager) Release(instance Container) error {
	if pm.config.ShareReplica {
		return nil
	}

	ci := instance.(*ContainerInstance)
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if !ci.IsAllocated() {
		return fmt.Errorf("容器未被分配")
	}

	delete(pm.allocated, ci.ID())
	ci.SetAllocated(false)

	// 检查容器是否健康
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	status, err := pm.client.GetContainerStatus(ctx, ci.ID())
	if err != nil || status != StatusRunning {
		// 容器不健康，删除并创建新的
		log.Warn().Str("id", ci.ID()).Msg("容器不健康，将被替换")
		pm.client.RemoveContainer(context.Background(), ci.ID(), true)

		newInstance, err := pm.createAndStartContainer(context.Background())
		if err != nil {
			return fmt.Errorf("替换容器失败: %w", err)
		}

		pm.warmupPool <- newInstance
		atomic.AddInt64(&pm.warmupCount, 1)
		return nil
	}

	// 放回预热池
	select {
	case pm.warmupPool <- ci:
		atomic.AddInt64(&pm.warmupCount, 1)
		log.Debug().Str("id", ci.ID()).Msg("容器释放到预热池")
		return nil
	default:
		// 预热池已满，删除容器
		pm.client.RemoveContainer(context.Background(), ci.ID(), true)
		atomic.AddInt64(&pm.totalCount, -1)
		log.Debug().Str("id", ci.ID()).Msg("预热池已满，删除容器")
		return nil
	}
}

// GetStatus 获取池状态
func (pm *PoolManager) GetStatus() PoolStatus {
	return PoolStatus{
		TotalCount:  atomic.LoadInt64(&pm.totalCount),
		WarmupCount: atomic.LoadInt64(&pm.warmupCount),
		Allocated:   int64(len(pm.allocated)),
		MaxReplicas: pm.config.MaxReplicas,
	}
}

// cleanupLoop 清理过期容器
func (pm *PoolManager) cleanupLoop() {
	defer pm.wg.Done()

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-pm.shutdownCh:
			return
		case <-ticker.C:
			pm.cleanup()
		}
	}
}

// cleanup 清理过期容器
func (pm *PoolManager) cleanup() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	deadline := time.Now().Add(-time.Duration(pm.config.ContainerTTL) * time.Second)

	for id, instance := range pm.allocated {
		if instance.GetAllocTime().Before(deadline) {
			log.Info().Str("id", id).Msg("清理过期容器")
			pm.client.RemoveContainer(context.Background(), id, true)
			delete(pm.allocated, id)
			atomic.AddInt64(&pm.totalCount, -1)
		}
	}
}

// Shutdown 关闭池管理器
func (pm *PoolManager) Shutdown(ctx context.Context) error {
	log.Info().Msg("关闭容器池管理器")

	close(pm.shutdownCh)
	pm.wg.Wait()

	pm.mu.Lock()
	defer pm.mu.Unlock()

	// 删除所有容器
	for id := range pm.allocated {
		pm.client.RemoveContainer(ctx, id, true)
	}

	for {
		select {
		case instance := <-pm.warmupPool:
			pm.client.RemoveContainer(ctx, instance.ID(), true)
		default:
			goto done
		}
	}

done:
	log.Info().Msg("容器池管理器已关闭")
	return nil
}

// PoolStatus 池状态
type PoolStatus struct {
	TotalCount  int64
	WarmupCount int64
	Allocated   int64
	MaxReplicas int64
}
