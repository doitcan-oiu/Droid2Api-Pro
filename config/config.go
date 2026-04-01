package config

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

// ModelConfig defines a model
type ModelConfig struct {
	Name      string `yaml:"name"`
	ID        string `yaml:"id"`
	Type      string `yaml:"type"`      // anthropic, openai, google, common
	Reasoning string `yaml:"reasoning"` // auto, low, medium, high, xhigh, off
	Provider  string `yaml:"provider"`
	Fast      bool   `yaml:"fast"`
}

// EndpointConfig defines an upstream endpoint
type EndpointConfig struct {
	Name    string `yaml:"name"`
	BaseURL string `yaml:"base_url"`
}

// ProxyConfig defines a proxy
type ProxyConfig struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

// AppConfig is the root configuration
type AppConfig struct {
	Port                 int               `yaml:"port"`
	DevMode              bool              `yaml:"dev_mode"`
	UserAgent            string            `yaml:"user_agent"`
	SystemPrompt         string            `yaml:"system_prompt"`
	RefreshKeys          []string          `yaml:"refresh_keys"`
	RefreshIntervalHours int               `yaml:"refresh_interval_hours"`
	ModelRedirects       map[string]string `yaml:"model_redirects"`
	Endpoints            []EndpointConfig  `yaml:"endpoints"`
	Proxies              []ProxyConfig     `yaml:"proxies"`
	Models               []ModelConfig     `yaml:"models"`
}

var (
	cfg     atomic.Pointer[AppConfig]
	cfgPath string
	baseDir string
	once    sync.Once
)

// BaseDir returns the directory containing the config file.
// Used by auth/handler to place data/ and logs/ next to config.
func BaseDir() string {
	return baseDir
}

// Load reads config.yaml (with env override for refresh_keys) and starts file watcher.
// If the config file does not exist, it is created with default values.
func Load(path string) error {
	cfgPath = path
	baseDir = filepath.Dir(path)
	if baseDir == "" || baseDir == "." {
		baseDir, _ = os.Getwd()
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		log.Printf("[INFO] 配置文件 %s 不存在，正在生成默认配置...", path)
		if err := os.WriteFile(path, []byte(defaultConfigYAML), 0o644); err != nil {
			return fmt.Errorf("生成默认配置失败: %w", err)
		}
		log.Printf("[INFO] 默认配置已生成: %s", path)
	}
	if err := reload(); err != nil {
		return err
	}
	go watchFile(path)
	return nil
}

const defaultConfigYAML = `port: 3000
dev_mode: false
user_agent: "factory-cli/0.85.0"
system_prompt: "You are Droid, an AI software engineering agent built by Factory.\n\n"

# 多令牌轮询 (round-robin)
# 支持多个 DROID_REFRESH_KEY 做负载均衡
# 也可通过环境变量设置: DROID_REFRESH_KEY=key1,key2,key3
refresh_keys: []

# 令牌刷新间隔 (小时)
refresh_interval_hours: 6

# 模型重定向: 将旧模型 ID 映射到新的
model_redirects:
  claude-3-5-haiku-20241022: "claude-haiku-4-5-20251001"
  claude-sonnet-4-5: "claude-sonnet-4-5-20250929"

# 上游端点
endpoints:
  - name: openai
    base_url: "https://api.factory.ai/api/llm/o/v1/responses"
  - name: anthropic
    base_url: "https://api.factory.ai/api/llm/a/v1/messages"
  - name: google
    base_url: "https://api.factory.ai/api/llm/g/v1/generate"
  - name: common
    base_url: "https://api.factory.ai/api/llm/o/v1/chat/completions"

# 代理列表 (可选, 轮询)
proxies: []

# 模型定义
models:
  - name: "Opus 4.5"
    id: "claude-opus-4-5-20251101"
    type: "anthropic"
    reasoning: "auto"
    provider: "anthropic"

  - name: "Opus 4.6"
    id: "claude-opus-4-6"
    type: "anthropic"
    reasoning: "auto"
    provider: "anthropic"

  - name: "Haiku 4.5"
    id: "claude-haiku-4-5-20251001"
    type: "anthropic"
    reasoning: "auto"
    provider: "anthropic"

  - name: "Sonnet 4.5"
    id: "claude-sonnet-4-5-20250929"
    type: "anthropic"
    reasoning: "auto"
    provider: "anthropic"

  - name: "Sonnet 4.6"
    id: "claude-sonnet-4-6"
    type: "anthropic"
    reasoning: "auto"
    provider: "anthropic"

  - name: "GPT-5.4"
    id: "gpt-5.4"
    type: "openai"
    reasoning: "auto"
    provider: "openai"

  - name: "Gemini 3 flash"
    id: "gemini-3-flash-preview"
    type: "google"
    reasoning: "auto"
    provider: "google"

  - name: "Gemini 3.1 Pro"
    id: "gemini-3.1-pro-preview"
    type: "google"
    reasoning: "auto"
    provider: "google"
`

func reload() error {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var c AppConfig
	if err := yaml.Unmarshal(data, &c); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	// defaults
	if c.Port == 0 {
		c.Port = 3000
	}
	if c.RefreshIntervalHours == 0 {
		c.RefreshIntervalHours = 6
	}
	if c.UserAgent == "" {
		c.UserAgent = "factory-cli/0.85.0"
	}

	// Env override: DROID_REFRESH_KEY can be comma-separated list
	if envKeys := os.Getenv("DROID_REFRESH_KEY"); envKeys != "" {
		keys := strings.Split(envKeys, ",")
		var trimmed []string
		for _, k := range keys {
			k = strings.TrimSpace(k)
			if k != "" {
				trimmed = append(trimmed, k)
			}
		}
		if len(trimmed) > 0 {
			c.RefreshKeys = trimmed
		}
	}

	cfg.Store(&c)
	log.Printf("[INFO] Configuration loaded (%d models, %d refresh keys)", len(c.Models), len(c.RefreshKeys))
	return nil
}

func watchFile(path string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[ERROR] Failed to create file watcher: %v", err)
		return
	}
	defer watcher.Close()

	if err := watcher.Add(path); err != nil {
		log.Printf("[ERROR] Failed to watch config file: %v", err)
		return
	}
	log.Printf("[INFO] Watching config file for changes: %s", path)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
				log.Printf("[INFO] Config file changed, reloading...")
				if err := reload(); err != nil {
					log.Printf("[ERROR] Failed to reload config: %v", err)
				} else {
					log.Printf("[INFO] Config reloaded successfully")
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[ERROR] File watcher error: %v", err)
		}
	}
}

// Get returns the current config snapshot (never nil after Load).
func Get() *AppConfig {
	return cfg.Load()
}

// GetModelByID finds a model by its ID.
func GetModelByID(modelID string) *ModelConfig {
	c := Get()
	for i := range c.Models {
		if c.Models[i].ID == modelID {
			return &c.Models[i]
		}
	}
	return nil
}

// GetEndpointByType finds an endpoint by name.
func GetEndpointByType(name string) *EndpointConfig {
	c := Get()
	for i := range c.Endpoints {
		if c.Endpoints[i].Name == name {
			return &c.Endpoints[i]
		}
	}
	return nil
}

// RedirectModel applies model_redirects mapping.
func RedirectModel(modelID string) string {
	c := Get()
	if redirected, ok := c.ModelRedirects[modelID]; ok {
		log.Printf("[REDIRECT] Model redirected: %s -> %s", modelID, redirected)
		return redirected
	}
	return modelID
}

// GetModelReasoning returns the reasoning level for a model.
func GetModelReasoning(modelID string) string {
	m := GetModelByID(modelID)
	if m == nil || m.Reasoning == "" {
		return ""
	}
	level := strings.ToLower(m.Reasoning)
	switch level {
	case "low", "medium", "high", "xhigh", "auto":
		return level
	}
	return ""
}

// IsModelFast returns whether the model is marked as fast.
func IsModelFast(modelID string) bool {
	m := GetModelByID(modelID)
	return m != nil && m.Fast
}

// GetModelProvider returns the provider for a model.
func GetModelProvider(modelID string) string {
	m := GetModelByID(modelID)
	if m == nil {
		return ""
	}
	return m.Provider
}
