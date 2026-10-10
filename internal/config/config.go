// Package config 加载 wordrush 的 YAML 配置，敏感项用环境变量覆盖。
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"gopkg.in/yaml.v3"
)

// Config 是服务的全部配置。
type Config struct {
	Server    Server    `yaml:"server"`
	Ucenter   Ucenter   `yaml:"ucenter"`
	DB        DB        `yaml:"db"`
	SpeakEval SpeakEval `yaml:"speak_eval"`
}

// SpeakEval 是口语跟读评分服务（OpenPronounce 微服务）配置。
// BaseURL 留空表示功能关闭，前端自动降级浏览器识别。
type SpeakEval struct {
	BaseURL string `yaml:"base_url"`
	// Timeout 单次评测超时；冷启动模型加载可能较慢，默认 30s
	Timeout string `yaml:"timeout"`
	// CacheDir 是发音 TTS 的磁盘缓存目录（默认 data/tts-cache）
	CacheDir string `yaml:"cache_dir"`
}

// CacheDirOrDefault 返回 TTS 缓存目录。
func (s SpeakEval) CacheDirOrDefault() string {
	if s.CacheDir != "" {
		return s.CacheDir
	}
	return "data/tts-cache"
}

// TimeoutDuration 解析评测超时，非法值回退 30 秒。
func (s SpeakEval) TimeoutDuration() time.Duration {
	d, err := time.ParseDuration(s.Timeout)
	if err != nil || d <= 0 {
		return 30 * time.Second
	}
	return d
}

// Server 是 HTTP 服务与 CORS 配置。
type Server struct {
	Addr string `yaml:"addr"`
	// AllowedOrigins 是浏览器直连白名单，留空表示放行所有来源。
	AllowedOrigins []string `yaml:"allowed_origins"`
	// WebDir 是前端构建产物目录；存在时本进程同时托管 SPA（缺省 web/dist）。
	WebDir string `yaml:"web_dir"`
}

// Ucenter 是接入用户中心所需的参数。
type Ucenter struct {
	BaseURL string `yaml:"base_url"`
	// App 必须与 Ucenter Worker 的 APPS 白名单一致，用于校验 JWT 的 aud。
	App     string `yaml:"app"`
	JwksTTL string `yaml:"jwks_ttl"`
}

// DB 是学习记录数据库（OCI HeatWave / MySQL）配置。
type DB struct {
	DSN      string `yaml:"dsn"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Name     string `yaml:"name"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	// TLSMode：HeatWave 内部域名是自签证书，不能配 "true"，用 preferred。
	TLSMode            string `yaml:"tls_mode"`
	MaxOpenConns       int    `yaml:"max_open_conns"`
	MaxIdleConns       int    `yaml:"max_idle_conns"`
	ConnMaxLifetimeSec int    `yaml:"conn_max_lifetime_sec"`
}

// JwksDuration 解析 JWKS 缓存时长，非法值回退 1 小时。
func (u Ucenter) JwksDuration() time.Duration {
	d, err := time.ParseDuration(u.JwksTTL)
	if err != nil || d <= 0 {
		return time.Hour
	}
	return d
}

// ConnDSN 返回实际使用的 MySQL 连接串（驱动 Config 拼装，避免密码特殊字符问题）。
func (d DB) ConnDSN() (string, error) { return d.connDSN(true) }

// NoDBDSN 返回不带库名的连接串，专用于启动时 CREATE DATABASE。
func (d DB) NoDBDSN() (string, error) { return d.connDSN(false) }

func (d DB) connDSN(withDB bool) (string, error) {
	if s := strings.TrimSpace(d.DSN); s != "" {
		return s, nil
	}
	if d.Host == "" || d.User == "" {
		return "", fmt.Errorf("db.host / db.user 均不能为空（或直接提供 db.dsn）")
	}
	if withDB && d.Name == "" {
		return "", fmt.Errorf("db.name 不能为空（或直接提供 db.dsn）")
	}

	port := d.Port
	if port == 0 {
		port = 3306
	}

	c := mysql.NewConfig()
	c.User = d.User
	c.Passwd = d.Password
	c.Net = "tcp"
	c.Addr = net.JoinHostPort(d.Host, strconv.Itoa(port))
	if withDB {
		c.DBName = d.Name
	}
	c.ParseTime = false
	c.Loc = time.UTC
	c.Timeout = 10 * time.Second
	c.ReadTimeout = 30 * time.Second
	c.WriteTimeout = 30 * time.Second

	switch strings.ToLower(strings.TrimSpace(d.TLSMode)) {
	case "", "preferred":
		c.TLSConfig = "preferred"
	case "true", "verify", "verify-full":
		c.TLSConfig = "true"
	case "skip-verify":
		c.TLSConfig = "skip-verify"
	case "false", "disabled", "off":
	default:
		return "", fmt.Errorf("db.tls_mode 取值非法: %q（可选 preferred / true / skip-verify / false）", d.TLSMode)
	}
	return c.FormatDSN(), nil
}

// MaxOpenConnsOrDefault 返回最大连接数。
func (d DB) MaxOpenConnsOrDefault() int {
	if d.MaxOpenConns > 0 {
		return d.MaxOpenConns
	}
	return 16
}

// MaxIdleConnsOrDefault 返回最大空闲连接数。
func (d DB) MaxIdleConnsOrDefault() int {
	if d.MaxIdleConns > 0 {
		return d.MaxIdleConns
	}
	return 8
}

// ConnMaxLifetime 连接最大存活时长（托管 MySQL 空闲连接会被中间层掐断，设 3 分钟）。
func (d DB) ConnMaxLifetime() time.Duration {
	if d.ConnMaxLifetimeSec > 0 {
		return time.Duration(d.ConnMaxLifetimeSec) * time.Second
	}
	return 3 * time.Minute
}

// Load 读取并校验配置文件，随后用环境变量覆盖敏感项。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}
	cfg := defaultConfig()
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	applyEnv(cfg)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defaultConfig() *Config {
	return &Config{
		Server: Server{Addr: ":8090", WebDir: "web/dist"},
		Ucenter: Ucenter{
			BaseURL: "https://ucenter.june-t.top",
			App:     "wordrush",
			JwksTTL: "1h",
		},
		DB: DB{Port: 3306, TLSMode: "preferred"},
	}
}

func applyEnv(c *Config) {
	setStr(&c.Server.Addr, "WORDRUSH_SERVER_ADDR")
	setStr(&c.Server.WebDir, "WORDRUSH_WEB_DIR")
	if v := os.Getenv("WORDRUSH_ALLOWED_ORIGINS"); v != "" {
		c.Server.AllowedOrigins = splitList(v)
	}
	setStr(&c.Ucenter.BaseURL, "WORDRUSH_UCENTER_BASE_URL")
	setStr(&c.Ucenter.App, "WORDRUSH_UCENTER_APP")
	setStr(&c.DB.DSN, "WORDRUSH_DB_DSN")
	setStr(&c.DB.Host, "WORDRUSH_DB_HOST")
	setStr(&c.DB.Name, "WORDRUSH_DB_NAME")
	setStr(&c.DB.User, "WORDRUSH_DB_USER")
	setStr(&c.DB.Password, "WORDRUSH_DB_PASSWORD")
	setInt(&c.DB.Port, "WORDRUSH_DB_PORT")
	setInt(&c.DB.MaxOpenConns, "WORDRUSH_DB_MAX_OPEN_CONNS")
	setStr(&c.DB.TLSMode, "WORDRUSH_DB_TLS_MODE")
	setStr(&c.SpeakEval.BaseURL, "WORDRUSH_SPEAK_EVAL_URL")
	setStr(&c.SpeakEval.Timeout, "WORDRUSH_SPEAK_EVAL_TIMEOUT")
	setStr(&c.SpeakEval.CacheDir, "WORDRUSH_SPEAK_CACHE_DIR")
}

func (c *Config) validate() error {
	if strings.TrimSpace(c.Server.Addr) == "" {
		return fmt.Errorf("server.addr 不能为空")
	}
	if strings.TrimSpace(c.Ucenter.BaseURL) == "" {
		return fmt.Errorf("ucenter.base_url 不能为空")
	}
	if strings.TrimSpace(c.Ucenter.App) == "" {
		return fmt.Errorf("ucenter.app 不能为空（需与 Ucenter 的 APPS 白名单一致）")
	}
	if _, err := c.DB.ConnDSN(); err != nil {
		return err
	}
	return nil
}

func setStr(dst *string, key string) {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		*dst = v
	}
}

func setInt(dst *int, key string) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		*dst = n
	}
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
