package types

// SkillMetadata Skill元数据
type SkillMeta struct {
	Name        string `yaml:"name"`        // 技能名称
	Description string `yaml:"description"` // 技能描述
}

// SkillInfo 技能信息
type SkillInfo struct {
	SkillMeta
	Path string // 技能目录路径
}

// Skill 技能定义
type Skill struct {
	Info         SkillInfo
	Instructions string // 详细内容
}
