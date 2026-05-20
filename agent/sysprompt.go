package agent

import (
	"fmt"
	"os"
	"runtime"
)

// buildSystemPrompt 构建系统提示词
func (a *Agent) buildSystemPrompt() string {
	prompt := a.systemPrompt
	if prompt == "" {
		prompt = `你是一个专业的AI Agent助手。你可以使用提供的工具来完成用户的任务。`
	}

	rw := ""
	if a.cfg.Sandbox.MountReadWrite == false {
		rw = "[目录属性：只读], /tmp[可读，可写]"
	}

	// 注入环境元数据
	wd, _ := os.Getwd()
	meta := fmt.Sprintf(`
## 系统环境信息
- 当前工作目录: %s%s
- 操作系统: %s
- 架构: %s`,
		wd, rw,
		runtime.GOOS, runtime.GOARCH,
	)

	prompt = meta + "\n\n" + prompt

	skillPromt := a.skillRegistry.BuildSkillsPrompt()
	if skillPromt != "" {
		prompt += "\n\n" + skillPromt
	}
	return prompt
}
