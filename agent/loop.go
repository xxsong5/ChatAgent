package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"chatAgent/sandbox"

	"github.com/rs/zerolog/log"
	"github.com/sashabaranov/go-openai"
)

// Run 运行Agent处理用户输入
func (a *Agent) Run(ctx context.Context) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.loop(ctx)
	}()
}

// loop Agent核心循环
func (a *Agent) loop(ctx context.Context) {
	if a.sandboxInstance != nil {
		ctx = sandbox.WithContainer(ctx, a.sandboxInstance)
	}
	tools := a.toolRegistry.ToLLMTools()

	// 尝试从磁盘加载历史记忆（用于会话恢复场景）
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: a.buildSystemPrompt()},
	}
	if loaded := a.loadMemory(); loaded != nil {
		// 将加载的记忆追加到system消息之后，但排除system消息避免重复
		for _, msg := range loaded {
			if msg.Role != openai.ChatMessageRoleSystem {
				messages = append(messages, msg)
			}
		}
		log.Info().Str("sessionID", a.sessionID).
			Int("loadedCount", len(loaded)).
			Int("totalMessages", len(messages)).
			Msg("会话已加载历史记忆")
	}

	j, _ := json.Marshal(messages)
	fmt.Println("debug", string(j))

	for userInput := range a.inputChan {
		content := userInput.Content
		if len(userInput.Metadata) > 0 {
			metaJSON, _ := json.Marshal(userInput.Metadata)
			content = fmt.Sprintf("[消息元数据: %s]\n%s", string(metaJSON), userInput.Content)
		}
		messages = append(messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: content})

		for i := 0; i < a.maxIterations; i++ {
			a.emitEvent(AgentEvent{Type: EventThought, Data: fmt.Sprintf("正在进行第 %d 轮迭代...", i+1)})

			// 创建流式请求
			req := openai.ChatCompletionRequest{
				Model:       a.cfg.Model,
				Temperature: float32(a.cfg.Temperature),
				MaxTokens:   a.cfg.MaxTokens,
				Messages:    messages,
				Tools:       tools,
				Stream:      true,
			}

			// Qwen3 模型支持 chat_template_kwargs 控制思考模式
			req.ChatTemplateKwargs = map[string]any{
				"enable_thinking": a.cfg.EnableThinking,
			}

			stream, err := a.llmClient.CreateChatCompletionStream(ctx, req)
			if err != nil {
				log.Error().Err(err).Msg("LLM流式调用失败")
				a.emitEvent(AgentEvent{Type: EventError, Data: ErrorEventData{Error: fmt.Sprintf("LLM流式调用失败: %v", err)}})
				break
			}

			reader := NewStreamReader(stream)

			// CollectAll 同时收集content文本和tool calls
			// 支持模型在同一次响应中同时输出推理文本和工具调用
			result := reader.CollectAll(func(chunk string) {
				a.emitEvent(AgentEvent{Type: EventChunk, Data: chunk})
			})
			reader.Close()

			// 构建assistant消息（可能同时包含content和tool_calls）
			messages = append(messages, openai.ChatCompletionMessage{
				Role:      openai.ChatMessageRoleAssistant,
				Content:   result.Content,
				ToolCalls: result.ToolCalls,
			})

			if len(result.ToolCalls) > 0 {
				// 有工具调用，执行工具
				a.handleToolCall(ctx, result, &messages, i)
			} else {
				// 无工具调用，视为最终回答
				a.emitEvent(AgentEvent{Type: EventFinal, Data: result.Content})
				break
			}
		}

		// 单轮处理结束后发出round_end事件
		a.emitEvent(AgentEvent{Type: EventRoundEnd, Data: nil})

		// 用户消息处理完成后，保存历史记录到磁盘
		a.saveHistory(messages)

		// 消息数量过多时触发压缩
		if len(messages) > 20 {
			a.compactHistory(messages)
		}
	}
}

// handleToolCall 处理工具调用结果
func (a *Agent) handleToolCall(ctx context.Context, result CollectAllResult, messages *[]openai.ChatCompletionMessage, iterIndex int) {
	for _, toolCall := range result.ToolCalls {
		a.emitEvent(AgentEvent{
			Type: EventToolCall,
			Data: ToolCallEventData{
				Tool: toolCall.Function.Name,
				Args: json.RawMessage(toolCall.Function.Arguments),
			},
		})

		toolContent, err := a.toolRegistry.Execute(
			ctx,
			toolCall.Function.Name,
			json.RawMessage(toolCall.Function.Arguments),
		)

		if err != nil {
			toolContent = fmt.Sprintf("工具执行错误: %v\n\n%v", err, toolContent)
		}

		a.emitEvent(AgentEvent{
			Type: EventToolResult,
			Data: ToolResultEventData{
				Tool:   toolCall.Function.Name,
				Result: toolContent,
			},
		})

		if a.maxIterations == iterIndex+1 {
			toolContent += "\n\n---" + fmt.Sprintf("达到最大迭代次数 (%d)，任务未完成\n---", a.maxIterations)
			a.emitEvent(AgentEvent{Type: EventError, Data: ErrorEventData{Error: fmt.Sprintf("达到最大迭代次数 (%d)，任务未完成", a.maxIterations)}})
		}

		*messages = append(*messages, openai.ChatCompletionMessage{
			Role:       openai.ChatMessageRoleTool,
			Content:    toolContent,
			ToolCallID: toolCall.ID,
		})
	}
}

// handleToolCall 处理工具调用
//func (a *Agent) handleToolCall(ctx context.Context, reader *StreamReader, messages *[]openai.ChatCompletionMessage, iterIndex int) {
//	toolCalls := reader.CollectToolCalls()
//	reader.Close()
//
//	*messages = append(*messages, openai.ChatCompletionMessage{
//		Role:      openai.ChatMessageRoleAssistant,
//		ToolCalls: toolCalls,
//	})
//
//	for _, toolCall := range toolCalls {
//		a.emitEvent(AgentEvent{
//			Type: EventToolCall,
//			Data: ToolCallEventData{
//				Tool: toolCall.Function.Name,
//				Args: json.RawMessage(toolCall.Function.Arguments),
//			},
//		})
//
//		toolContent, err := a.toolRegistry.Execute(
//			ctx,
//			toolCall.Function.Name,
//			json.RawMessage(toolCall.Function.Arguments),
//		)
//
//		if err != nil {
//			toolContent = fmt.Sprintf("工具执行错误: %v\n\n%v", err, toolContent)
//		}
//
//		a.emitEvent(AgentEvent{
//			Type: EventToolResult,
//			Data: ToolResultEventData{
//				Tool:   toolCall.Function.Name,
//				Result: toolContent,
//			},
//		})
//
//		if a.maxIterations == iterIndex+1 {
//			toolContent += "\n\n---" + fmt.Sprintf("达到最大迭代次数 (%d)，任务未完成\n---", a.maxIterations)
//			a.emitEvent(AgentEvent{Type: EventError, Data: ErrorEventData{Error: fmt.Sprintf("达到最大迭代次数 (%d)，任务未完成", a.maxIterations)}})
//		}
//
//		*messages = append(*messages, openai.ChatCompletionMessage{
//			Role:       openai.ChatMessageRoleTool,
//			Content:    toolContent,
//			ToolCallID: toolCall.ID,
//		})
//	}
//}

// handleFinalAnswer 处理最终回答（流式输出）
//func (a *Agent) handleFinalAnswer(reader *StreamReader, messages *[]openai.ChatCompletionMessage) {
//	finalAnswer := reader.CollectContent(func(chunk string) {
//		// 每收到一个chunk就emit，实现打字机效果
//		a.emitEvent(AgentEvent{Type: EventChunk, Data: chunk})
//	})
//	reader.Close()
//
//	*messages = append(*messages, openai.ChatCompletionMessage{
//		Role:    openai.ChatMessageRoleAssistant,
//		Content: finalAnswer,
//	})
//
//	a.emitEvent(AgentEvent{Type: EventFinal, Data: finalAnswer})
//}
//
