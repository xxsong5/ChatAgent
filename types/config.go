package types

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var defaultConfig *Config

// GetDefaultConfig 获取默认配置的只读指针
func GetDefaultConfig() *Config {
	return defaultConfig
}

// Config LLM配置
type Config struct {
	APIKey          string        `json:"api_key" yaml:"api_key"`
	BaseURL         string        `json:"base_url" yaml:"base_url"`
	Model           string        `json:"model" yaml:"model"`
	Temperature     float64       `json:"temperature" yaml:"temperature"`
	MaxTokens       int           `json:"max_tokens" yaml:"max_tokens"`
	Timeout         int           `json:"timeout" yaml:"timeout"`
	MaxIterations   int           `json:"max_iterations" yaml:"max_iterations"`
	EnableThinking  bool          `json:"enable_thinking" yaml:"enable_thinking"` // Qwen3模型思考开关，默认nil表示使用模型默认
	MCPServerConfig []*MCPConfig  `json:"mcp_server_config" yaml:"mcp_server_config"`
	Server          ServerConfig  `json:"server" yaml:"server"`
	Sandbox         SandboxConfig `json:"sandbox" yaml:"sandbox"`
	SkillsDir       string        `json:"skills_dir" yaml:"skills_dir"`
	SessionsDir     string        `json:"sessions_dir" yaml:"sessions_dir"`
	StashDir        string        `json:"stash_dir" yaml:"stash_dir"`
	SessionTTL      int           `json:"sessionTTL" yaml:"sesionTTL"`
}

// GetTimeoutDuration 获取超时时间
func (c *Config) GetTimeoutDuration() time.Duration {
	return time.Duration(c.Timeout) * time.Second
}

// GetSessionTTL 获取session空闲存活时间
func (c *Config) GetSessionTTL() time.Duration {
	return time.Duration(c.SessionTTL) * time.Second
}

// MCPConfig MCP 服务器配置
type MCPConfig struct {
	Name      string `json:"name"`      // 服务器名称
	Transport string `json:"transport"` // "stdio" | "http"
	Enabled   bool   `json:"enabled"`   // 是否启用

	// stdio 传输配置
	Command string            `json:"command,omitempty"` // 启动命令
	Args    []string          `json:"args,omitempty"`    // 命令参数
	Env     map[string]string `json:"env,omitempty"`     // 环境变量

	// HTTP 传输配置
	URL     string            `json:"url,omitempty"`     // MCP 端点 URL
	Headers map[string]string `json:"headers,omitempty"` // 请求头
	Timeout int               `json:"timeout,omitempty"` // 超时（秒）
}

// ServerType 服务器类型
type ServerType string

const (
	// ServerTypeOpenAI OpenAI兼容API服务器
	ServerTypeOpenAI ServerType = "openai"
	// ServerTypeMCP MCP HTTP服务器
	ServerTypeMCP ServerType = "mcp"
	// ServerTypeCli Command Line交互执行
	ServerTypeCli ServerType = "cli"
	// ServerTypeWeb httpUI交互执行
	ServerTypeWeb ServerType = "web"
	// ServerTypeWeixin 主动对接微信，作为消息输入输出
	ServerTypeWeixin ServerType = "weixin"
)

// AuthConfig 认证配置
type AuthConfig struct {
	Type string `json:"type" yaml:"type"` // 认证类型: "none" | "api_key" | "bearer"
}

// ServerConfig 服务器配置
type ServerConfig struct {
	Type ServerType `json:"type" yaml:"type"`     // 服务器类型: "openai" | "mcp" | "cli"
	Host string     `json:"host" yaml:"host"`     // 监听地址
	Port int        `json:"port" yaml:"port"`     // 监听端口
	Auth string     `json:"apikey" yaml:"apikey"` // 认证密钥
}

// SandboxConfig 沙箱运行容器配置
type SandboxConfig struct {
	Enabled        bool   `json:"enabled" yaml:"enabled"`
	Image          string `json:"image" yaml:"image"`
	MountReadWrite bool   `json:"mountReadWrite" yaml:"mountReadWrite"`
	MountPath      string `json:"mountPath" yaml:"mountPath"` // 挂载路径，默认当前目录
	ShareReplica   bool   `json:"shareReplica" yaml:"shareReplica"`
	MaxReplicas    int64  `json:"maxReplicas" yaml:"maxReplicas"`
	WarmupReplicas int64  `json:"warmupReplicas" yaml:"warmupReplicas"`
	// 资源限制
	MemoryMB    int64  `json:"memoryMB" yaml:"memoryMB"`       // 内存限制(MB)，默认256
	NanoCPUs    int64  `json:"nanoCPUs" yaml:"nanoCPUs"`       // CPU限制，默认500000000 (0.5核)
	NetworkMode string `json:"networkMode" yaml:"networkMode"` // 网络模式，默认bridge
	// 超时配置
	ContainerTTL int `json:"containerTTL" yaml:"containerTTL"` // 容器存活时间(秒)，默认1800
	StartTimeout int `json:"startTimeout" yaml:"startTimeout"` // 启动超时(秒)，默认30
	ExecTimeout  int `json:"execTimeout" yaml:"execTimeout"`   // 命令执行超时(秒)，默认10
}

func init() {
	// 尝试从当前目录加载配置文件
	rootPaths := []string{"./", "./config/"}
	configPaths := []string{"config.yaml", "config.yml", "config.json"}

	for _, root := range rootPaths {
		for _, configPath := range configPaths {
			fpath := filepath.Join(root, configPath)
			if _, err := os.Stat(fpath); err == nil {
				if cfg, err := loadConfig(fpath); err == nil {
					defaultConfig = cfg
				} else {
					fmt.Println(err)
				}
				return
			}
		}
	}

	panic("请在当前工作跟路径下设置config配置文件")
}

func loadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开配置文件失败: %w", err)
	}
	defer file.Close()

	var config Config
	if err := json.NewDecoder(file).Decode(&config); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	apiKey := os.Getenv(config.APIKey)
	if apiKey == "" {
		return nil, fmt.Errorf("环境变量 %s 未设置", config.APIKey)
	}
	config.APIKey = apiKey

	if len(config.Sandbox.MountPath) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("获取当前工作目录失败: %w", err)
		}
		config.Sandbox.MountPath = cwd
	}

	if config.MaxIterations <= 0 {
		config.MaxIterations = 10
	}

	if config.SessionTTL <= 0 {
		config.SessionTTL = 1200
	}

	return &config, nil
}
