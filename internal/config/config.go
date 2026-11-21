package config

import (
	"fmt"
	"os"
	"strings"
)

// Config 全部通过环境变量配置（HL_ 前缀）。
type Config struct {
	Addr          string // HTTP 监听地址
	DBURL         string // 空=SQLite(默认)；postgres://...=PostgreSQL
	RedisURL      string // 空=不启用 Redis 会话缓存
	SessionSecret string

	BaseURL string // OAuth 回调基准地址，如 http://localhost:8080
	DataDir string // SQLite 托管实例的数据卷目录

	// OAuth
	Provider     string // github | generic | (空)
	ClientID     string
	ClientSecret string
	AuthorizeURL string // generic 必填
	TokenURL     string // generic 必填
	UserInfoURL  string // generic 必填
	Scopes       string

	DemoLogin   bool   // 未配置 OAuth 时的本地演示登录
	AdminEmails string // 逗号分隔的管理员邮箱白名单
}

func Load() *Config {
	c := &Config{
		Addr:          getEnv("HL_ADDR", ":8080"),
		DBURL:         getEnv("HL_DB", ""),
		RedisURL:      getEnv("HL_REDIS_URL", ""),
		SessionSecret: getEnv("HL_SESSION_SECRET", ""),
		BaseURL:       strings.TrimRight(getEnv("HL_BASE_URL", "http://localhost:8080"), "/"),
		DataDir:       getEnv("HL_DATA_DIR", "./data/volumes"),
		Provider:      strings.ToLower(getEnv("HL_OAUTH_PROVIDER", "")),
		ClientID:      getEnv("HL_CLIENT_ID", ""),
		ClientSecret:  getEnv("HL_CLIENT_SECRET", ""),
		AuthorizeURL:  getEnv("HL_AUTH_URL", ""),
		TokenURL:      getEnv("HL_TOKEN_URL", ""),
		UserInfoURL:   getEnv("HL_USERINFO_URL", ""),
		Scopes:        getEnv("HL_SCOPES", ""),
		DemoLogin:     getEnv("HL_DEMO_LOGIN", "false") == "true",
		AdminEmails:   getEnv("HL_ADMIN_EMAILS", ""),
	}
	if c.SessionSecret == "" {
		c.SessionSecret = "harborlight-dev-secret-change-me"
	}
	return c
}

func (c *Config) IsAdminEmail(email string) bool {
	want := strings.ToLower(strings.TrimSpace(email))
	for _, candidate := range strings.Split(c.AdminEmails, ",") {
		if strings.ToLower(strings.TrimSpace(candidate)) == want && want != "" {
			return true
		}
	}
	return false
}

// OAuthEnabled 是否配置了 OAuth。
func (c *Config) OAuthEnabled() bool {
	if c.Provider == "github" {
		return c.ClientID != "" && c.ClientSecret != ""
	}
	if c.Provider == "generic" {
		return c.ClientID != "" && c.ClientSecret != "" &&
			c.AuthorizeURL != "" && c.TokenURL != "" && c.UserInfoURL != ""
	}
	return false
}

// DBSummary 存储后端摘要（用于界面展示）。
func (c *Config) DBSummary() string {
	if c.DBURL == "" {
		return "SQLite"
	}
	return "PostgreSQL"
}

func (c *Config) OAuthRedirectURL() string {
	return fmt.Sprintf("%s/auth/callback", c.BaseURL)
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
