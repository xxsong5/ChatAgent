package skill

import (
	"chatAgent/types"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
	"gopkg.in/yaml.v3"
)

// Loader Skill加载器
type Loader struct {
	skillsDir string
}

// NewLoader 创建加载器
func NewLoader(skillsDir string) *Loader {
	absDir, _ := filepath.Abs(skillsDir)
	return &Loader{
		skillsDir: absDir,
	}
}

// DiscoverSkills 发现所有Skill（仅加载元数据）
func (l *Loader) DiscoverSkills() ([]types.SkillInfo, error) {
	var skills []types.SkillInfo

	log.Info().Str("dir", l.skillsDir).Msg("开始发现技能")
	entries, err := os.ReadDir(l.skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return skills, nil // 目录不存在返回空列表
		}
		return nil, fmt.Errorf("读取skills目录失败: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillPath := filepath.Join(l.skillsDir, entry.Name())
		skillMdPath := filepath.Join(skillPath, "SKILL.md")

		// 检查 SKILL.md 是否存在
		if _, err := os.Stat(skillMdPath); os.IsNotExist(err) {
			continue
		}

		// 仅解析元数据
		meta, _, err := l.parseSkillFile(skillMdPath, false)
		if err != nil {
			log.Warn().Str("file", skillMdPath).Err(err).Msg("解析元数据失败")
			continue
		}

		// 验证 name 与目录名匹配
		if meta.Name != entry.Name() {
			log.Warn().Str("expected", meta.Name).Str("actual", entry.Name()).Msg("Skill名称与目录名不匹配")
			continue
		}

		skills = append(skills, types.SkillInfo{
			SkillMeta: *meta,
			Path:      skillPath,
		})
	}

	return skills, nil
}

// LoadSkill 完整加载一个Skill
func (l *Loader) LoadSkill(skillPath string) (*types.Skill, error) {
	skillMdPath := filepath.Join(skillPath, "SKILL.md")
	// 解析 frontmatter 和正文
	meta, body, err := l.parseSkillFile(skillMdPath, true)
	if err != nil {
		return nil, fmt.Errorf("解析SKILL.md失败: %w", err)
	}

	skill := &types.Skill{
		Info: types.SkillInfo{
			SkillMeta: *meta,
			Path:      skillPath,
		},
		Instructions: body,
	}

	return skill, nil
}

// parseSkillFile 解析 SKILL.md 文件，返回元数据和正文
// includeBody 为 true 时验证 name 格式并返回 body，否则仅返回元数据
func (l *Loader) parseSkillFile(path string, includeBody bool) (*types.SkillMeta, string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}

	frontmatter, body, err := l.extractFrontmatter(string(content))
	if err != nil {
		return nil, "", err
	}

	meta, err := l.parseAndValidateMetadata(frontmatter)
	if err != nil {
		return nil, "", err
	}

	if includeBody {
		return meta, body, nil
	}
	return meta, "", nil
}

// extractFrontmatter 提取 frontmatter 和正文内容
func (l *Loader) extractFrontmatter(content string) (frontmatter string, body string, err error) {
	if !strings.HasPrefix(content, "---") {
		return "", "", fmt.Errorf("SKILL.md 必须以 --- 开头")
	}

	rest := content[3:]
	endIndex := strings.Index(rest, "\n---")
	if endIndex == -1 {
		return "", "", fmt.Errorf("SKILL.md frontmatter 未闭合")
	}

	return rest[:endIndex], strings.TrimSpace(rest[endIndex+4:]), nil
}

// parseAndValidateMetadata 解析并验证元数据
func (l *Loader) parseAndValidateMetadata(frontmatter string) (*types.SkillMeta, error) {
	var meta types.SkillMeta
	if err := yaml.Unmarshal([]byte(frontmatter), &meta); err != nil {
		return nil, fmt.Errorf("解析YAML失败: %w", err)
	}

	if meta.Name == "" {
		return nil, fmt.Errorf("缺少必填字段: name")
	}
	if meta.Description == "" {
		return nil, fmt.Errorf("缺少必填字段: description")
	}

	if err := validateSkillName(meta.Name); err != nil {
		return nil, err
	}

	return &meta, nil
}

// validateSkillName 验证 Skill 名称格式
func validateSkillName(name string) error {
	if len(name) > 64 {
		return fmt.Errorf("name 超过64字符限制")
	}

	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("name 不能以连字符开头或结尾")
	}

	if strings.Contains(name, "--") {
		return fmt.Errorf("name 不能包含连续连字符")
	}

	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return fmt.Errorf("name 只能包含小写字母、数字和连字符")
		}
	}

	return nil
}
