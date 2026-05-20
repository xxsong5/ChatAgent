package tool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"chatAgent/sandbox"

	"github.com/rs/zerolog/log"
)

// StashTool 文件暂存工具
// 提供文件短时暂存，并返回HTTP下载链接服务
type StashTool struct {
	BaseTool
	stashDir string
	baseURL  string
}

// NewStashTool 创建文件暂存工具
// stashDir: 暂存文件存放目录
// baseURL: 服务器基础URL，用于构造下载链接，例如 http://localhost:8080
func NewStashTool(stashDir, baseURL string) Tool {
	// 确保暂存目录存在
	if err := os.MkdirAll(stashDir, 0755); err != nil {
		log.Fatal().Err(err).Str("dir", stashDir).Msg("创建暂存目录失败")
	}

	// 启动定时清理协程（30分钟TTL）
	go autoCleanStash(stashDir, 30*time.Minute)

	return &StashTool{
		BaseTool: BaseTool{
			name:        "file_stash",
			description: "文件暂存。将本地文件暂存到服务器并返回HTTP下载链接（30分钟有效）。",
			parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"file_path": map[string]interface{}{
						"type":        "string",
						"description": "文件绝对路径，例：/mnt/workdir/xxx.txt。路径必须真实存在，使用前请先通过 ls -la 确认，如果有多个文件请先压缩成一个文件。",
					},
				},
				"required": []string{"file_path"},
			},
		},
		stashDir: stashDir,
		baseURL:  strings.TrimRight(baseURL, "/"),
	}
}

// makeStashPath 生成唯一文件名和目标路径
func (t *StashTool) makeStashPath(originalPath string) (string, string) {
	randBytes := make([]byte, 16)
	if _, err := rand.Read(randBytes); err != nil {
		log.Error().Err(err).Msg("生成随机文件名失败，使用时间戳兜底")
		uniqueName := fmt.Sprintf("%d_%s", time.Now().UnixNano(), filepath.Base(originalPath))
		return uniqueName, filepath.Join(t.stashDir, uniqueName)
	}
	uniqueName := hex.EncodeToString(randBytes) + "_" + filepath.Base(originalPath)
	return uniqueName, filepath.Join(t.stashDir, uniqueName)
}

// Execute 执行文件暂存
func (t *StashTool) Execute(ctx context.Context, params json.RawMessage) (string, error) {
	var input struct {
		FilePath string `json:"file_path"`
	}

	if err := json.Unmarshal(params, &input); err != nil {
		return "", fmt.Errorf("参数解析失败: %w", err)
	}

	if input.FilePath == "" {
		return "", fmt.Errorf("file_path 不能为空")
	}

	// 打开源文件
	srcFile, err := os.Open(input.FilePath)
	if err != nil {
		// 本地文件不存在时，从ctx中获取container instance再次尝试
		instance := sandbox.GetContainer(ctx)
		if instance != nil {
			log.Debug().Str("path", input.FilePath).Msg("本地文件不存在，尝试从sandbox容器复制")
			return t.stashFromContainer(ctx, instance, input.FilePath)
		}
		return "", fmt.Errorf("文件不存在: %s，请先用 ls -la 确认文件真实路径后再重试", input.FilePath)
	}
	defer srcFile.Close()

	// 获取文件信息
	srcInfo, err := srcFile.Stat()
	if err != nil {
		return "", fmt.Errorf("获取文件信息失败: %w", err)
	}

	// 生成唯一文件名
	uniqueName, dstPath := t.makeStashPath(input.FilePath)

	// 创建目标文件
	dstFile, err := os.Create(dstPath)
	if err != nil {
		return "", fmt.Errorf("创建暂存文件失败: %w", err)
	}
	defer dstFile.Close()

	// 复制文件内容
	written, err := io.Copy(dstFile, srcFile)
	if err != nil {
		return "", fmt.Errorf("复制文件失败: %w", err)
	}

	log.Info().
		Str("src", input.FilePath).
		Str("dst", uniqueName).
		Int64("size", written).
		Int64("original_size", srcInfo.Size()).
		Msg("文件已暂存")

	// 检查复制是否完整
	if written != srcInfo.Size() {
		log.Warn().
			Int64("written", written).
			Int64("expected", srcInfo.Size()).
			Msg("文件复制大小不匹配")
	}

	downloadURL := fmt.Sprintf("%s/stash/%s", t.baseURL, uniqueName)
	return fmt.Sprintf("文件已暂存，下载链接（30分钟内有效）:\n%s\n\n文件大小: %d 字节\n原始路径: %s",
		downloadURL, written, input.FilePath), nil
}

// stashFromContainer 从sandbox容器中复制文件到暂存目录
func (t *StashTool) stashFromContainer(ctx context.Context, instance sandbox.Container, filePath string) (string, error) {
	// 生成唯一文件名和目标路径
	uniqueName, dstPath := t.makeStashPath(filePath)

	// 使用Docker的CopyFromContainer API直接复制文件
	if err := instance.CopyFile(ctx, filePath, dstPath); err != nil {
		return "", fmt.Errorf("从容器复制文件失败: %w", err)
	}

	// 获取文件大小
	fileInfo, err := os.Stat(dstPath)
	if err != nil {
		log.Warn().Err(err).Str("dst", dstPath).Msg("获取暂存文件信息失败")
	}

	size := int64(0)
	if err == nil {
		size = fileInfo.Size()
	}

	log.Info().
		Str("src", filePath).
		Str("dst", uniqueName).
		Int64("size", size).
		Msg("文件已暂存（从容器）")

	downloadURL := fmt.Sprintf("%s/stash/%s", t.baseURL, uniqueName)
	return fmt.Sprintf("文件已暂存，下载链接（30分钟内有效）:\n%s\n\n文件大小: %d 字节\n原始路径: %s",
		downloadURL, size, filePath), nil
}

// StashHandler 返回处理 /stash/ 请求的 HTTP handler
// 提供暂存文件的静态文件服务
func StashHandler(stashDir string) http.Handler {
	return http.StripPrefix("/stash/", http.FileServer(http.Dir(stashDir)))
}

// autoCleanStash 定时清理过期文件
func autoCleanStash(stashDir string, ttl time.Duration) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		entries, err := os.ReadDir(stashDir)
		if err != nil {
			if os.IsNotExist(err) {
				return
			}
			log.Error().Err(err).Msg("读取暂存目录失败")
			continue
		}

		now := time.Now()
		removedCount := 0
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				continue
			}

			if now.After(info.ModTime().Add(ttl)) {
				path := filepath.Join(stashDir, entry.Name())
				if err := os.Remove(path); err != nil {
					log.Error().Err(err).Str("file", entry.Name()).Msg("清理暂存文件失败")
				} else {
					removedCount++
					log.Debug().Str("file", entry.Name()).Msg("暂存文件已过期清理")
				}
			}
		}
		if removedCount > 0 {
			log.Info().Int("count", removedCount).Msg("暂存文件清理完成")
		}
	}
}
