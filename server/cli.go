package server

import (
	"bufio"
	"chatAgent/agent"
	"chatAgent/sandbox"
	"chatAgent/types"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/rs/zerolog/log"
)

// CLIServer 命令行交互服务器
// 支持用户在终端中进行多轮会话
type CLIServer struct {
	cfg        *types.Config
	sessionMgr *SessionManager
}

// NewCLIServer 创建CLI服务器
func NewCLIServer(cfg *types.Config, sessionMgr *SessionManager) *CLIServer {
	return &CLIServer{
		cfg:        cfg,
		sessionMgr: sessionMgr,
	}
}

// Start 启动CLI交互
func (s *CLIServer) Start() error {
	log.Info().Str("type", "cli").Msg("启动命令行交互模式")
	log.Info().Msg("输入 'exit' 退出，输入 'new' 开始新会话，输入 'list' 查看活跃会话")

	sessionID := ""
	ctx := context.Background()

	reader := bufio.NewReader(os.Stdin)

	// 显示欢迎信息
	fmt.Println("\n===================================")
	fmt.Println("  Chat Agent CLI 交互模式")
	fmt.Println("  支持多轮会话 / 输入 /new 新建会话")
	fmt.Println("===================================")

	for {
		// 显示当前会话信息
		if sessionID != "" {
			shortID := sessionID
			if len(shortID) > 8 {
				shortID = shortID[:8]
			}
			fmt.Printf("\n[会话: %s]\n", shortID)
		} else {
			fmt.Print("\n[新建会话]\n")
		}

		fmt.Print(">>> ")
		input, err := reader.ReadString('\n')
		if err != nil {
			log.Error().Err(err).Msg("读取输入失败")
			break
		}

		input = strings.TrimSpace(input)

		// 处理特殊命令
		if input == "exit" || input == "quit" {
			fmt.Println("再见！")
			break
		}

		if input == "new" || input == "/new" {
			// 开始新会话
			sessionID = ""
			fmt.Println("\n--- 已创建新会话 ---")
			continue
		}

		if input == "list" || input == "/list" {
			sessions := s.sessionMgr.ListSessions()
			if len(sessions) == 0 {
				fmt.Println("没有活跃会话")
			} else {
				fmt.Println("\n--- 活跃会话列表 ---")
				for _, sess := range sessions {
					shortID := sess.ID
					if len(shortID) > 8 {
						shortID = shortID[:8]
					}
					fmt.Printf("  %s (创建: %s, 活跃: %v)\n",
						shortID,
						sess.CreatedAt.Format("15:04:05"),
						sess.IsActive)
				}
			}
			continue
		}

		if input == "" {
			continue
		}

		// 路由消息到SessionManager
		session, eventCh, err := s.sessionMgr.RouteMessage(ctx, sessionID, "", input, "")
		if err != nil {
			fmt.Printf("错误: %v\n", err)
			continue
		}

		// 记录当前会话ID
		if sessionID == "" {
			sessionID = session.ID
		}

		// 显示流式输出
		fmt.Print("\nAssistant: ")
	loop:
		for event := range eventCh {
			switch event.Type {
			case agent.EventChunk:
				content, ok := event.Data.(string)
				if ok {
					fmt.Print(content)
				}

			case agent.EventToolCall:
				toolData, _ := json.Marshal(event.Data)
				fmt.Printf("\n[工具调用: %s]", string(toolData))

			case agent.EventToolResult:
				fmt.Print("\n[工具执行完成]")

			case agent.EventFinal:
				fmt.Println()

			case agent.EventRoundEnd:
				session.Agent.Unsubscribe(eventCh)
				break loop

			case agent.EventError:
				errData, _ := json.Marshal(event.Data)
				fmt.Printf("\n[错误: %s]\n", string(errData))
			}
		}
		fmt.Println()
	}

	return nil
}

// Shutdown 关闭CLI服务器
func (s *CLIServer) Shutdown() error {
	log.Info().Msg("CLI服务器已关闭")
	s.sessionMgr.Shutdown()
	sandbox.DestroyManager()
	return nil
}
