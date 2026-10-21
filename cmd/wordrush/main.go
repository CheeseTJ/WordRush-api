// wordrush API 服务入口。
//
// 学习数据存放在 OCI HeatWave（MySQL），词库打包在前端（离线优先的 PWA），
// 账户与令牌完全复用 Ucenter（Ed25519 JWT + JWKS 本地验签）。
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"wordrush/internal/api"
	"wordrush/internal/auth"
	"wordrush/internal/config"
	"wordrush/internal/release"
	"wordrush/internal/store"
)

func main() {
	configPath := flag.String("config", "./config.yaml", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	st, err := store.Open(cfg.DB)
	if err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}
	defer st.Close()

	fetcher := auth.NewFetcher(cfg.Ucenter.BaseURL, cfg.Ucenter.JwksDuration())
	// 启动时预热 JWKS：失败只告警，首次鉴权时会重试
	if err := fetcher.Warmup(); err != nil {
		log.Printf("警告: 预热 JWKS 失败（%v），将在首次鉴权时重试", err)
	} else {
		log.Printf("Ucenter JWKS 就绪: %s（app=%s）", cfg.Ucenter.BaseURL, cfg.Ucenter.App)
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()
	r.Use(corsMiddleware(cfg.Server.AllowedOrigins))

	// 蒲公英版本查询客户端：凭据缺失时 Enabled=false，接口只回下载页兜底
	rel := release.NewClient(cfg.App.PgyerAPIKey, cfg.App.PgyerAppKey)
	if rel.Enabled() {
		log.Printf("客户端更新已启用（蒲公英 app_key=%s…）", maskKey(cfg.App.PgyerAppKey))
	} else {
		log.Printf("客户端更新未启用：缺少 WORDRUSH_APP_PGYER_API_KEY / WORDRUSH_APP_PGYER_APP_KEY")
	}

	api.Register(r, api.Deps{Cfg: cfg, Store: st, Fetcher: fetcher, Release: rel})

	srv := &http.Server{Addr: cfg.Server.Addr, Handler: r}
	go func() {
		log.Printf("wordrush 启动，监听 %s（ucenter=%s app=%s db=%s）",
			cfg.Server.Addr, cfg.Ucenter.BaseURL, cfg.Ucenter.App, cfg.DB.Name)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("收到退出信号，正在关闭...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("HTTP 服务关闭异常: %v", err)
	}
	log.Println("已退出")
}

// maskKey 只保留密钥首尾各 4 位，用于日志里确认「配置的是哪一把」而不泄露。
func maskKey(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "…" + s[len(s)-4:]
}

// corsMiddleware 允许前端站点跨域直连。
func corsMiddleware(allowed []string) gin.HandlerFunc {
	allowAll := len(allowed) == 0
	whitelist := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		whitelist[strings.TrimRight(o, "/")] = struct{}{}
	}

	return func(c *gin.Context) {
		origin := strings.TrimRight(c.GetHeader("Origin"), "/")
		if origin != "" {
			_, hit := whitelist[origin]
			if allowAll || hit {
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Vary", "Origin")
			}
		}
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
		c.Header("Access-Control-Max-Age", "86400")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
