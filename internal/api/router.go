// Package api 组装 HTTP 路由。
package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"

	"wordrush/internal/auth"
	"wordrush/internal/config"
	"wordrush/internal/httpx"
	"wordrush/internal/store"
)

// Deps 是路由所需的依赖。
type Deps struct {
	Cfg     *config.Config
	Store   *store.Store
	Fetcher *auth.Fetcher
}

// Register 挂载全部路由。
//
// 分层约定：
//   - /healthz        存活探针，不查任何外部依赖
//   - /api/health     详细诊断，公开但只暴露「是否配置」，不回显任何密钥
//   - /api/v1/*       全部需要 Ucenter JWT
func Register(r *gin.Engine, d Deps) {
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	r.GET("/api/health", func(c *gin.Context) { health(c, d) })

	v1 := r.Group("/api/v1")
	v1.Use(auth.Middleware(d.Fetcher, d.Cfg.Ucenter.App))
	v1.Use(ensureUser(d.Store))
	{
		v1.GET("/profile", func(c *gin.Context) { getProfile(c, d) })
		v1.GET("/progress", func(c *gin.Context) { getProgress(c, d) })
		v1.POST("/quiz/finish", func(c *gin.Context) { postQuizFinish(c, d) })
		v1.GET("/review", func(c *gin.Context) { getReview(c, d) })
		v1.POST("/review/result", func(c *gin.Context) { postReviewResult(c, d) })
		v1.GET("/stats", func(c *gin.Context) { getStats(c, d) })
		v1.GET("/tasks", func(c *gin.Context) { getTasks(c, d) })
		v1.POST("/tasks/claim", func(c *gin.Context) { postTaskClaim(c, d) })
		v1.POST("/hearts/refill", func(c *gin.Context) { postHeartsRefill(c, d) })
		v1.POST("/freeze/buy", func(c *gin.Context) { postBuyFreeze(c, d) })
		v1.POST("/speak/assess", func(c *gin.Context) { postSpeakAssess(c, d) })
		v1.GET("/speak/tts", func(c *gin.Context) { getSpeakTTS(c, d) })
	}

	// 非 /api 路径：web_dir（前端构建产物）存在时由本进程托管 SPA，
	// 一个端口同时出 UI 与 API。
	webRoot := d.Cfg.Server.WebDir
	if st, err := os.Stat(webRoot); err == nil && st.IsDir() {
		r.NoRoute(func(c *gin.Context) {
			if strings.HasPrefix(c.Request.URL.Path, "/api/") {
				httpx.NotFound(c, "接口不存在")
				return
			}
			p := filepath.Join(webRoot, filepath.Clean("/"+c.Request.URL.Path))
			if info, err := os.Stat(p); err == nil && !info.IsDir() {
				c.File(p)
				return
			}
			// index.html 禁缓存：发版后浏览器不能拿旧壳加载旧 hash 资源
			c.Writer.Header().Set("Cache-Control", "no-cache")
			c.File(filepath.Join(webRoot, "index.html"))
		})
	} else {
		r.NoRoute(func(c *gin.Context) {
			if strings.HasPrefix(c.Request.URL.Path, "/api/") {
				httpx.NotFound(c, "接口不存在")
				return
			}
			c.String(http.StatusNotFound, "not found")
		})
	}
}

// ensureUser 把 JWT 中的身份落到影子用户表（带节流）。
func ensureUser(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := st.EnsureUser(
			c.Request.Context(),
			auth.UserID(c),
			c.GetString(auth.CtxEmail),
			c.GetString(auth.CtxRole),
		); err != nil {
			httpx.Internal(c, err, "用户信息写入失败")
			return
		}
		c.Next()
	}
}
