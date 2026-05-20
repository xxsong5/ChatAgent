package sandbox

import (
	"archive/tar"
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/rs/zerolog/log"

	apptypes "chatAgent/types"
)

// DockerClient Docker SDK客户端
type DockerClient struct {
	cli *client.Client
}

// NewDockerClient 创建Docker客户端
func NewDockerClient() (*DockerClient, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("创建Docker客户端失败: %w", err)
	}
	return &DockerClient{cli: cli}, nil
}

// Close 关闭客户端
func (d *DockerClient) Close() error {
	return d.cli.Close()
}

// PullImage 拉取镜像（如果本地不存在）
func (d *DockerClient) PullImage(ctx context.Context, config apptypes.SandboxConfig) error {
	// 先检查本地是否存在
	exists, err := d.ImageExists(ctx, config.Image)
	if err != nil {
		log.Warn().Err(err).Str("image", config.Image).Msg("检查镜像存在性失败，尝试拉取")
	} else if exists {
		log.Info().Str("image", config.Image).Msg("镜像已存在，无需拉取")
		return nil
	}

	log.Info().Str("image", config.Image).Msg("开始拉取镜像")

	reader, err := d.cli.ImagePull(ctx, config.Image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("拉取镜像失败: %w", err)
	}
	defer reader.Close()

	// 读取拉取进度
	_, err = io.Copy(io.Discard, reader)
	if err != nil {
		log.Warn().Err(err).Msg("读取镜像拉取进度失败")
	}

	log.Info().Str("image", config.Image).Msg("镜像拉取完成")
	return nil
}

// ImageExists 检查镜像是否存在于本地
func (d *DockerClient) ImageExists(ctx context.Context, image string) (bool, error) {
	_, err := d.cli.ImageInspect(ctx, image)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ContainerInfo 容器信息
type ContainerInfo struct {
	ID     string
	Status ContainerStatus
}

// CreateContainer 创建容器
func (d *DockerClient) CreateContainer(ctx context.Context, name string, config apptypes.SandboxConfig) (string, error) {
	// 构建绑定挂载
	var binds []string
	roMode := ":ro"
	if config.MountReadWrite {
		roMode = ":rw"
	}
	binds = append(binds, config.MountPath+":"+config.MountPath+roMode)

	log.Debug().
		Str("name", name).
		Str("image", config.Image).
		Str("mount", config.MountPath).
		Str("binds", fmt.Sprintf("%v", binds)).
		Msg("创建容器")

	resp, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      config.Image,
			Cmd:        []string{"sleep", "infinity"},
			WorkingDir: config.MountPath,
		},
		HostConfig: &container.HostConfig{
			Binds: binds,
			Resources: container.Resources{
				Memory:   config.MemoryMB * 1024 * 1024,
				NanoCPUs: config.NanoCPUs,
			},
			NetworkMode: container.NetworkMode(config.NetworkMode),
		},
		Name: name,
	})
	if err != nil {
		return "", fmt.Errorf("创建容器失败: %w", err)
	}

	log.Info().Str("id", resp.ID).Str("name", name).Msg("容器创建成功")
	return resp.ID, nil
}

// StartContainer 启动容器
func (d *DockerClient) StartContainer(ctx context.Context, containerID string) error {
	_, err := d.cli.ContainerStart(ctx, containerID, client.ContainerStartOptions{})
	return err
}

// StopContainer 停止容器
func (d *DockerClient) StopContainer(ctx context.Context, containerID string, timeout time.Duration) error {
	timeoutSecs := int(timeout.Seconds())
	_, err := d.cli.ContainerStop(ctx, containerID, client.ContainerStopOptions{
		Timeout: &timeoutSecs,
	})
	return err
}

// RemoveContainer 删除容器
func (d *DockerClient) RemoveContainer(ctx context.Context, containerID string, force bool) error {
	_, err := d.cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{
		Force: force,
	})
	return err
}

// GetContainerStatus 获取容器状态
func (d *DockerClient) GetContainerStatus(ctx context.Context, containerID string) (ContainerStatus, error) {
	result, err := d.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return StatusStopped, fmt.Errorf("检查容器失败: %w", err)
	}

	if result.Container.State.Running {
		return StatusRunning, nil
	}
	return StatusStopped, nil
}

// ExecInContainer 在容器中执行命令
func (d *DockerClient) ExecInContainer(ctx context.Context, containerID string, cmd string, timeout time.Duration) (string, string, error) {
	execConfig := client.ExecCreateOptions{
		Cmd:          []string{"sh", "-c", cmd},
		AttachStdout: true,
		AttachStderr: true,
	}

	// 创建exec实例
	resp, err := d.cli.ExecCreate(ctx, containerID, execConfig)
	if err != nil {
		return "", "", fmt.Errorf("创建exec失败: %w", err)
	}

	// 附加到exec
	attachResp, err := d.cli.ExecAttach(ctx, resp.ID, client.ExecAttachOptions{})
	if err != nil {
		return "", "", fmt.Errorf("附加exec失败: %w", err)
	}
	defer attachResp.Close()

	// 设置超时
	type execResult struct {
		stdout string
		stderr string
		err    error
	}

	resultCh := make(chan execResult, 1)

	go func() {
		reader := bufio.NewReader(attachResp.Reader)
		stdoutBuf := make([]byte, 65536)
		stderrBuf := make([]byte, 65536)

		stdoutLen, _ := reader.Read(stdoutBuf)
		stderrLen, _ := reader.Read(stderrBuf)

		resultCh <- execResult{
			stdout: string(stdoutBuf[:stdoutLen]),
			stderr: string(stderrBuf[:stderrLen]),
		}
	}()

	var stdout, stderr string

	select {
	case <-time.After(timeout):
		return "", "", fmt.Errorf("命令执行超时")
	case result := <-resultCh:
		stdout = result.stdout
		stderr = result.stderr
	case <-ctx.Done():
		return "", "", ctx.Err()
	}

	// 检查exec的退出码
	inspectResp, err := d.cli.ExecInspect(ctx, resp.ID, client.ExecInspectOptions{})

	if err != nil {
		return stdout, stderr, fmt.Errorf("检查exec状态失败: %w", err)
	}

	if inspectResp.ExitCode != 0 {
		return stdout, stderr, fmt.Errorf("命令执行失败, exit code: %d", inspectResp.ExitCode)
	}

	return stdout, stderr, nil
}

// ListContainers 列出所有容器
func (d *DockerClient) ListContainers(ctx context.Context, all bool) ([]ContainerInfo, error) {
	listResult, err := d.cli.ContainerList(ctx, client.ContainerListOptions{All: all})
	if err != nil {
		return nil, fmt.Errorf("列出容器失败: %w", err)
	}

	var result []ContainerInfo
	for _, c := range listResult.Items {
		status := StatusStopped
		if c.State == "running" {
			status = StatusRunning
		}
		result = append(result, ContainerInfo{
			ID:     c.ID,
			Status: status,
		})
	}

	return result, nil
}

// WaitForHealthy 等待容器健康
func (d *DockerClient) WaitForHealthy(ctx context.Context, containerID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		status, err := d.GetContainerStatus(ctx, containerID)
		if err != nil {
			log.Debug().Err(err).Str("container", containerID).Msg("检查容器状态失败")
			time.Sleep(500 * time.Millisecond)
			continue
		}

		if status == StatusRunning {
			return nil
		}

		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("容器启动超时")
}

// ContainerExists 检查容器是否存在
func (d *DockerClient) ContainerExists(ctx context.Context, name string) (bool, error) {
	_, err := d.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// CopyFileFromContainer 从容器中复制文件到本地路径
// 使用Docker的CopyFromContainer API获取tar流，然后解压提取文件
func (d *DockerClient) CopyFileFromContainer(ctx context.Context, containerID, srcPath, dstPath string) error {
	// 调用Docker API获取tar流
	result, err := d.cli.CopyFromContainer(ctx, containerID, client.CopyFromContainerOptions{
		SourcePath: srcPath,
	})
	if err != nil {
		return fmt.Errorf("从容器复制文件失败: %w", err)
	}
	defer result.Content.Close()

	// 确保目标目录存在
	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return fmt.Errorf("创建目标目录失败: %w", err)
	}

	// 解析tar流，提取文件
	tarReader := tar.NewReader(result.Content)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读取tar流失败: %w", err)
		}

		// 只提取普通文件
		if header.Typeflag == tar.TypeReg {
			outFile, err := os.Create(dstPath)
			if err != nil {
				return fmt.Errorf("创建目标文件失败: %w", err)
			}
			defer outFile.Close()

			if _, err := io.Copy(outFile, tarReader); err != nil {
				return fmt.Errorf("写入文件失败: %w", err)
			}

			log.Debug().
				Str("container", containerID).
				Str("src", srcPath).
				Str("dst", dstPath).
				Msg("容器文件已复制到本地")
			return nil
		}
	}

	return fmt.Errorf("tar流中未找到文件: %s", srcPath)
}
