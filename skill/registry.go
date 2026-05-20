// internal/skill/registry.go
package skill

import (
	"chatAgent/types"
	"fmt"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
)

// Registry Skill注册表
type Registry struct {
	loader       *Loader
	skillInfos   []types.SkillInfo       // 元数据缓存（启动时加载）
	loadedSkills map[string]*types.Skill // 已激活的完整Skill
	mu           sync.RWMutex
}

// NewRegistry 创建注册表
func NewRegistry(skillsDir string) *Registry {
	return &Registry{
		loader:       NewLoader(skillsDir),
		skillInfos:   make([]types.SkillInfo, 0),
		loadedSkills: make(map[string]*types.Skill),
	}
}

// Discover 发现所有Skill（启动时调用）
func (r *Registry) Discover() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	infos, err := r.loader.DiscoverSkills()
	if err != nil {
		return err
	}

	r.skillInfos = infos
	log.Info().Int("count", len(infos)).Msg("发现Skill")

	for _, info := range infos {
		log.Debug().Str("name", info.Name).Str("description", truncate(info.Description, 50)).Msg("  - ")
	}

	return nil
}

// GetSkillInfos 获取所有Skill摘要（用于LLM上下文）
func (r *Registry) GetSkillInfos() []types.SkillInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]types.SkillInfo, len(r.skillInfos))
	copy(result, r.skillInfos)
	return result
}

// BuildSkillsPrompt 构建Skill列表提示词
func (r *Registry) BuildSkillsPrompt() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.skillInfos) == 0 {
		return ""
	}

	var builder strings.Builder
	builder.WriteString("## 可用技能\n\n")
	builder.WriteString("以下是可以激活的专业技能：\n\n")

	for _, info := range r.skillInfos {
		builder.WriteString(fmt.Sprintf("- **%s**: %s\n", info.Name, info.Description))
	}

	builder.WriteString("\n当任务匹配某个技能时，使用 `activate_skill` 工具来激活它。\n")

	return builder.String()
}

// ActivateSkill 激活Skill（完整加载）
func (r *Registry) ActivateSkill(name string) (*types.Skill, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 检查是否已加载
	if skill, ok := r.loadedSkills[name]; ok {
		return skill, nil
	}

	// 查找 Skill 路径
	var skillPath string
	for _, info := range r.skillInfos {
		if info.Name == name {
			skillPath = info.Path
			break
		}
	}

	if skillPath == "" {
		return nil, fmt.Errorf("Skill不存在: %s", name)
	}

	// 完整加载
	skill, err := r.loader.LoadSkill(skillPath)
	if err != nil {
		return nil, fmt.Errorf("加载Skill失败: %w", err)
	}

	r.loadedSkills[name] = skill
	log.Info().Str("name", name).Msg("已激活Skill")

	return skill, nil
}

// truncate 截断字符串
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
