package main

import (
	"chatAgent/mcp"
	"chatAgent/sandbox"
	"chatAgent/server"
	"chatAgent/skill"
	"chatAgent/tool"
	"chatAgent/types"
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})

	config := types.GetDefaultConfig()

	// 初始化工具注册表
	toolRegistry := tool.NewRegistry()
	toolRegistry.Register(tool.NewShellTool())

	// 注册文件暂存工具
	stashDir := config.StashDir
	if stashDir == "" {
		stashDir = "config/stash"
	}
	baseURL := fmt.Sprintf("http://%s:%d", config.Server.Host, config.Server.Port)
	toolRegistry.Register(tool.NewStashTool(stashDir, baseURL))

	// 连接MCP服务器
	manager := mcp.NewManager()
	for _, mc := range config.MCPServerConfig {
		log.Info().Str("server", mc.Name).Msg("[MCP] 配置服务器")
		manager.AddServer(mc)
	}
	manager.ConnectAll(context.Background())
	toolRegistry.RegisterMCPTools(manager)

	// 发现并注册Skill
	skillRegistry := skill.NewRegistry(config.SkillsDir)
	if err := skillRegistry.Discover(); err != nil {
		log.Fatal().Err(err).Msg("发现Skill失败")
	}
	toolRegistry.Register(tool.NewActivateSkillTool(skillRegistry))
	toolRegistry.Register(tool.NewListSkillsTool(skillRegistry))

	// 创建Session管理器
	sessionManager := server.NewSessionManager(config, toolRegistry, skillRegistry)
	defer cleanup(sessionManager)

	// 根据配置启动对应的服务器
	startServer(config, sessionManager)
}

func startServer(config *types.Config, sessionManager *server.SessionManager) {
	var srv interface {
		Start() error
		Shutdown() error
	}

	switch config.Server.Type {
	case types.ServerTypeOpenAI:
		srv = server.NewOpenAIServer(config, sessionManager)
	case types.ServerTypeMCP:
		srv = server.NewMCPServer(config, sessionManager)
	case types.ServerTypeCli:
		srv = server.NewCLIServer(config, sessionManager)
	case types.ServerTypeWeb:
		srv = server.NewWebServer(config, sessionManager)
	case types.ServerTypeWeixin:
		srv = server.NewWeixinServer(config, sessionManager)
	default:
		log.Fatal().Str("type", string(config.Server.Type)).Msg("不支持的服务器类型")
	}

	go func() {
		if err := srv.Start(); err != nil {
			log.Info().Err(err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Info().Msg("正在关闭服务器...")
	if err := srv.Shutdown(); err != nil {
		log.Error().Err(err).Msg("服务器关闭失败")
	}
}

func cleanup(sessionManager *server.SessionManager) {
	log.Info().Msg("正在清理会话管理器...")
	sessionManager.Shutdown()
	sandbox.DestroyManager()
}
