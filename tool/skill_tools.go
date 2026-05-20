package tool

import (
	"chatAgent/skill"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
)

// ActivateSkillTool 激活技能工具
type ActivateSkillTool struct {
	registry *skill.Registry
}

// NewActivateSkillTool 创建激活技能工具
func NewActivateSkillTool(registry *skill.Registry) *ActivateSkillTool {
	return &ActivateSkillTool{registry: registry}
}

func (t *ActivateSkillTool) Name() string {
	return "activate_skill"
}

func (t *ActivateSkillTool) Description() string {
	return "当任务匹配某个技能的描述时使用,激活一个专业技能，将其完整指令加载到上下文中。"
}

func (t *ActivateSkillTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"skill_name": map[string]interface{}{
				"type":        "string",
				"description": "要激活的技能名称",
			},
		},
		"required": []string{"skill_name"},
	}
}

func (t *ActivateSkillTool) Execute(ctx context.Context, params json.RawMessage) (string, error) {
	var input struct {
		SkillName string `json:"skill_name"`
	}

	if err := json.Unmarshal(params, &input); err != nil {
		log.Error().Err(err).Msg("参数解析失败")
		return "", fmt.Errorf("参数解析失败: %w", err)
	}

	skill, err := t.registry.ActivateSkill(input.SkillName)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("技能 '%s', 文件目录: %s, 已激活。\n\n%s", skill.Info.Name, skill.Info.Path, skill.Instructions), nil
}

// ListSkillsTool 列出可用技能工具
type ListSkillsTool struct {
	registry *skill.Registry
}

// NewListSkillsTool 创建列出技能工具
func NewListSkillsTool(registry *skill.Registry) *ListSkillsTool {
	return &ListSkillsTool{registry: registry}
}

func (t *ListSkillsTool) Name() string {
	return "list_skills"
}

func (t *ListSkillsTool) Description() string {
	return "列出所有可用的技能及其描述"
}

func (t *ListSkillsTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}
}

func (t *ListSkillsTool) Execute(ctx context.Context, params json.RawMessage) (string, error) {
	infos := t.registry.GetSkillInfos()

	if len(infos) == 0 {
		return "当前没有可用的技能。", nil
	}

	var builder strings.Builder
	builder.WriteString("可用技能列表：\n\n")

	for _, info := range infos {
		builder.WriteString(fmt.Sprintf("- **%s**\n  %s\n\n", info.Name, info.Description))
	}

	return builder.String(), nil
}
