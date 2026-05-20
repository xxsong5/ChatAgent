package agent

import (
	"errors"
	"io"
	"strings"

	"github.com/sashabaranov/go-openai"
)

// StreamReader 流式响应读取器
// 封装了"先读第一个chunk判断类型，再累积剩余数据"的逻辑
type StreamReader struct {
	stream     *openai.ChatCompletionStream
	firstChunk *openai.ChatCompletionStreamResponse
	peeked     bool
}

// NewStreamReader 创建流式读取器
func NewStreamReader(stream *openai.ChatCompletionStream) *StreamReader {
	return &StreamReader{stream: stream}
}

// Peek 读取第一个chunk并缓存，判断响应类型
// 返回 true -> tool call；false -> content（final answer）
func (r *StreamReader) Peek() (isToolCall bool, err error) {
	if r.peeked {
		panic("StreamReader: Peek() 只能调用一次")
	}

	resp, err := r.stream.Recv()
	if err != nil {
		return false, err
	}

	r.firstChunk = &resp
	r.peeked = true

	delta := resp.Choices[0].Delta
	return len(delta.ToolCalls) > 0, nil
}

// CollectToolCalls 累积完整的ToolCalls（包含第一个chunk的数据）
func (r *StreamReader) CollectToolCalls() []openai.ToolCall {
	if !r.peeked || r.firstChunk == nil {
		panic("StreamReader: 必须先调用 Peek()")
	}

	firstDelta := r.firstChunk.Choices[0].Delta
	toolCalls := make([]openai.ToolCall, len(firstDelta.ToolCalls))
	argBuilders := make([]strings.Builder, len(firstDelta.ToolCalls))

	// 初始化：从第一个chunk拿到函数名和ID
	for i, tc := range firstDelta.ToolCalls {
		idx := i
		if tc.Index != nil {
			idx = *tc.Index
		}
		for len(toolCalls) <= idx {
			toolCalls = append(toolCalls, openai.ToolCall{})
			argBuilders = append(argBuilders, strings.Builder{})
		}
		toolCalls[idx] = openai.ToolCall{
			ID:   tc.ID,
			Type: tc.Type,
			Function: openai.FunctionCall{
				Name: tc.Function.Name,
			},
		}
		argBuilders[idx].WriteString(tc.Function.Arguments)
	}

	// 读取剩余chunk，累积arguments
	for {
		resp, err := r.stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			break
		}
		for i, tc := range resp.Choices[0].Delta.ToolCalls {
			idx := i
			if tc.Index != nil {
				idx = *tc.Index
			}
			if idx < len(argBuilders) {
				argBuilders[idx].WriteString(tc.Function.Arguments)
			}
		}
	}

	for i := range toolCalls {
		toolCalls[i].Function.Arguments = argBuilders[i].String()
	}

	return toolCalls
}

// CollectContent 累积文本内容（包含第一个chunk的数据）
// emitFn: 每收到一个chunk时回调，用于前端实时展示（打字机效果）
func (r *StreamReader) CollectContent(emitFn func(chunk string)) string {
	if !r.peeked || r.firstChunk == nil {
		panic("StreamReader: 必须先调用 Peek()")
	}

	var builder strings.Builder

	// 处理第一个chunk的内容
	firstContent := r.firstChunk.Choices[0].Delta.Content
	if firstContent != "" {
		builder.WriteString(firstContent)
		if emitFn != nil {
			emitFn(firstContent)
		}
	}

	// 读取剩余chunk
	for {
		resp, err := r.stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			break
		}

		content := resp.Choices[0].Delta.Content
		if content != "" {
			builder.WriteString(content)
			if emitFn != nil {
				emitFn(content)
			}
		}
	}

	return builder.String()
}

// CollectAllResult 同时收集content和tool calls的结果
type CollectAllResult struct {
	Content   string
	ToolCalls []openai.ToolCall
}

// CollectAll 读取所有chunk，同时收集content文本和tool calls
// emitFn: 每收到一个内容chunk时回调
// 支持模型在同一次响应中同时输出推理文本和工具调用
func (r *StreamReader) CollectAll(emitFn func(chunk string)) CollectAllResult {
	var contentBuilder strings.Builder
	var toolCalls []openai.ToolCall
	var argBuilders []strings.Builder

	// 处理单个chunk
	processChunk := func(resp openai.ChatCompletionStreamResponse) {
		if len(resp.Choices) == 0 {
			return
		}
		delta := resp.Choices[0].Delta

		// 收集content文本
		if delta.Content != "" {
			contentBuilder.WriteString(delta.Content)
			if emitFn != nil {
				emitFn(delta.Content)
			}
		}

		// 收集tool calls
		// 流式场景下每个 chunk 的 delta.ToolCalls 可能只有一项，
		// 必须用 tc.Index 定位（而非 slice 下标 i），否则多工具调用时参数会互相覆盖
		for i, tc := range delta.ToolCalls {
			idx := i
			if tc.Index != nil {
				idx = *tc.Index
			}
			for len(toolCalls) <= idx {
				toolCalls = append(toolCalls, openai.ToolCall{})
				argBuilders = append(argBuilders, strings.Builder{})
			}
			if tc.ID != "" {
				toolCalls[idx].ID = tc.ID
				toolCalls[idx].Type = tc.Type
			}
			if tc.Function.Name != "" {
				toolCalls[idx].Function.Name = tc.Function.Name
			}
			argBuilders[idx].WriteString(tc.Function.Arguments)
		}
	}

	// 处理第一个chunk（如果已Peek）
	if r.peeked && r.firstChunk != nil {
		processChunk(*r.firstChunk)
	} else if !r.peeked {
		// 未Peek则读取第一个chunk
		resp, err := r.stream.Recv()
		if err == nil {
			processChunk(resp)
		}
	}

	// 读取剩余chunk
	for {
		resp, err := r.stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			break
		}
		processChunk(resp)
	}

	// 完成tool call arguments的拼装
	for i := range toolCalls {
		toolCalls[i].Function.Arguments = argBuilders[i].String()
	}

	return CollectAllResult{
		Content:   contentBuilder.String(),
		ToolCalls: toolCalls,
	}
}

// Close 关闭底层流
func (r *StreamReader) Close() {
	r.stream.Close()
}
