/**
 * 配置加载模块
 * 负责解析 YAML 配置文件，定义 Codex 代理服务所需的全部配置结构
 */
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

/**
 * Config 是 Codex 代理服务的顶层配置结构
 * @field Listen - 监听地址，格式为 host:port
 * @field AuthDir - 账号文件目录路径
 * @field ProxyURL - 全局 HTTP/SOCKS 代理地址
 * @field BaseURL - Codex API 基础 URL
 * @field LogLevel - 日志级别 (debug/info/warn/error)
 * @field RefreshInterval - Token 自动刷新间隔（秒）
 * @field Accounts - 账号文件列表（可选，不指定则自动扫描 AuthDir）
 * @field APIKeys - 可选的 API 访问密钥，用于保护代理服务
 */
type Config struct {
	Listen                 string   `yaml:"listen"`
	AuthDir                string   `yaml:"auth-dir"`
	ProxyURL               string   `yaml:"proxy-url"`
	BackendDomain          string   `yaml:"backend-domain"`
	BackendResolveAddress  string   `yaml:"backend-resolve-address"`
	BaseURL                string   `yaml:"base-url"`
	LogLevel               string   `yaml:"log-level"`
	RefreshInterval        int      `yaml:"refresh-interval"`
	MaxRetry               int      `yaml:"max-retry"`
	HealthCheckInterval    int      `yaml:"health-check-interval"`
	HealthCheckMaxFailures int      `yaml:"health-check-max-failures"`
	HealthCheckConcurrency int      `yaml:"health-check-concurrency"`
	HealthCheckStartDelay  int      `yaml:"health-check-start-delay"`
	HealthCheckBatchSize   int      `yaml:"health-check-batch-size"`
	HealthCheckReqTimeout  int      `yaml:"health-check-request-timeout"`
	RefreshConcurrency     int      `yaml:"refresh-concurrency"`
	MaxConnsPerHost        int      `yaml:"max-conns-per-host"`
	MaxIdleConns           int      `yaml:"max-idle-conns"`
	MaxIdleConnsPerHost    int      `yaml:"max-idle-conns-per-host"`
	EnableHTTP2            bool     `yaml:"enable-http2"`
	StartupAsyncLoad       bool     `yaml:"startup-async-load"`
	Accounts               []string `yaml:"accounts"`
	APIKeys                []string `yaml:"api-keys"`
}

/**
 * LoadConfig 从指定路径加载 YAML 配置文件
 * @param path - 配置文件路径
 * @returns *Config - 解析后的配置对象
 * @returns error - 加载或解析失败时返回错误
 */
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	cfg := &Config{
		Listen:                 ":8080",
		AuthDir:                "./auths",
		BackendDomain:          "",
		BaseURL:                "",
		LogLevel:               "info",
		RefreshInterval:        3000,
		MaxRetry:               2,
		HealthCheckInterval:    300,
		HealthCheckMaxFailures: 3,
		HealthCheckConcurrency: 5,
		HealthCheckStartDelay:  45,
		HealthCheckBatchSize:   20,
		HealthCheckReqTimeout:  8,
		RefreshConcurrency:     50,
		MaxConnsPerHost:        512,
		MaxIdleConns:           1024,
		MaxIdleConnsPerHost:    512,
		EnableHTTP2:            true,
		StartupAsyncLoad:       true,
	}

	if err = yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	cfg.Sanitize()
	return cfg, nil
}

/**
 * Sanitize 清理和规范化配置值
 * 去除多余空白、设置默认值等
 */
func (c *Config) Sanitize() {
	c.Listen = strings.TrimSpace(c.Listen)
	c.AuthDir = strings.TrimSpace(c.AuthDir)
	c.ProxyURL = strings.TrimSpace(c.ProxyURL)
	c.BackendDomain = strings.TrimSpace(c.BackendDomain)
	c.BackendResolveAddress = strings.TrimSpace(c.BackendResolveAddress)
	c.BaseURL = strings.TrimSpace(c.BaseURL)
	c.LogLevel = strings.TrimSpace(strings.ToLower(c.LogLevel))

	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.AuthDir == "" {
		c.AuthDir = "./auths"
	}
	/* 优先级：base-url（若配置） > backend-domain（自动拼接） */
	if c.BaseURL != "" {
		if !strings.HasPrefix(strings.ToLower(c.BaseURL), "http://") && !strings.HasPrefix(strings.ToLower(c.BaseURL), "https://") {
			c.BaseURL = "https://" + c.BaseURL
		}
		if u, err := url.Parse(c.BaseURL); err == nil && u.Hostname() != "" {
			/* 与请求主机保持一致，避免解析地址映射目标与 URL 主机不一致 */
			c.BackendDomain = u.Hostname()
		}
	} else {
		if c.BackendDomain == "" {
			c.BackendDomain = "chatgpt.com"
		}
		c.BaseURL = "https://" + c.BackendDomain + "/backend-api/codex"
	}
	if c.RefreshInterval <= 0 {
		c.RefreshInterval = 3000
	}
	if c.MaxRetry < 0 {
		c.MaxRetry = 0
	}
	if c.HealthCheckInterval < 0 {
		c.HealthCheckInterval = 0
	}
	if c.HealthCheckMaxFailures <= 0 {
		c.HealthCheckMaxFailures = 3
	}
	if c.HealthCheckConcurrency <= 0 {
		c.HealthCheckConcurrency = 5
	}
	if c.HealthCheckConcurrency > 128 {
		c.HealthCheckConcurrency = 128
	}
	if c.HealthCheckStartDelay < 0 {
		c.HealthCheckStartDelay = 0
	}
	if c.HealthCheckBatchSize < 0 {
		c.HealthCheckBatchSize = 0
	}
	if c.HealthCheckBatchSize > 0 && c.HealthCheckConcurrency > c.HealthCheckBatchSize {
		c.HealthCheckConcurrency = c.HealthCheckBatchSize
	}
	if c.HealthCheckReqTimeout <= 0 {
		c.HealthCheckReqTimeout = 8
	}
	if c.RefreshConcurrency <= 0 {
		c.RefreshConcurrency = 50
	}
	if c.MaxConnsPerHost < 0 {
		c.MaxConnsPerHost = 0
	}
	if c.MaxIdleConns < 0 {
		c.MaxIdleConns = 0
	}
	if c.MaxIdleConnsPerHost < 0 {
		c.MaxIdleConnsPerHost = 0
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		c.LogLevel = "info"
	}

	level, err := log.ParseLevel(c.LogLevel)
	if err == nil {
		log.SetLevel(level)
	}
}
