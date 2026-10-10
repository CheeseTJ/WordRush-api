package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"wordrush/internal/auth"
	"wordrush/internal/httpx"
	"wordrush/internal/store"
)

// health 报告依赖状态：只暴露「是否配置/是否可用」，不回显任何密钥。
func health(c *gin.Context, d Deps) {
	ctx, cancel := contextWithTimeout(c)
	defer cancel()

	dbOK := true
	if err := d.Store.DB().PingContext(ctx); err != nil {
		dbOK = false
	}
	keys, _, jwksErr := d.Fetcher.Status()

	body := gin.H{
		"status":      "ok",
		"ucenter_app": d.Cfg.Ucenter.App,
		"db":          dbOK,
		"jwks_keys":   keys,
	}
	if jwksErr != nil {
		body["jwks_last_error"] = jwksErr.Error()
	}
	if !dbOK {
		body["status"] = "degraded"
	}
	c.JSON(http.StatusOK, body)
}

// getProfile 返回学习档案（XP、连续天数）。
func getProfile(c *gin.Context, d Deps) {
	p, err := d.Store.Profile(c.Request.Context(), auth.UserID(c))
	if err != nil {
		httpx.Internal(c, err, "读取档案失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"xp":          p.XP,
		"streak_days": p.StreakDays,
		"last_day":    p.LastDay,
		"level":       p.XP/500 + 1,
	})
}

// getProgress 返回全部关卡进度。
func getProgress(c *gin.Context, d Deps) {
	units, err := d.Store.Progress(c.Request.Context(), auth.UserID(c))
	if err != nil {
		httpx.Internal(c, err, "读取进度失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"units": units})
}

// postQuizFinish 提交关卡结果，返回更新后的档案。
func postQuizFinish(c *gin.Context, d Deps) {
	var req store.QuizResult
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.BadRequest(c, "请求体非法："+err.Error())
		return
	}
	if req.UnitID == "" {
		httpx.BadRequest(c, "unit_id 不能为空")
		return
	}
	prof, err := d.Store.FinishQuiz(c.Request.Context(), auth.UserID(c), req)
	if err != nil {
		httpx.BadRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"xp":          prof.XP,
		"streak_days": prof.StreakDays,
		"level":       prof.XP/500 + 1,
		"xp_gained":   req.Correct * 15,
	})
}

// getReview 返回复习队列（错题单词 ID 列表）。
func getReview(c *gin.Context, d Deps) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	ids, total, err := d.Store.ReviewQueue(c.Request.Context(), auth.UserID(c), limit)
	if err != nil {
		httpx.Internal(c, err, "读取复习队列失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"word_ids": ids, "total": total})
}

// postReviewResult 记录复习正误。
func postReviewResult(c *gin.Context, d Deps) {
	var req struct {
		Results []store.WordResult `json:"results"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.BadRequest(c, "请求体非法："+err.Error())
		return
	}
	if len(req.Results) == 0 {
		httpx.BadRequest(c, "results 不能为空")
		return
	}
	if len(req.Results) > 100 {
		httpx.BadRequest(c, "results 单次最多 100 条")
		return
	}
	if err := d.Store.ReviewResult(c.Request.Context(), auth.UserID(c), req.Results); err != nil {
		httpx.Internal(c, err, "写入复习结果失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// getStats 返回档案 + 近期每日统计 + 已学词数。
func getStats(c *gin.Context, d Deps) {
	days, _ := strconv.Atoi(c.DefaultQuery("days", "14"))
	prof, daily, learned, err := d.Store.Stats(c.Request.Context(), auth.UserID(c), days)
	if err != nil {
		httpx.Internal(c, err, "读取统计失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"xp":           prof.XP,
		"streak_days":  prof.StreakDays,
		"level":        prof.XP/500 + 1,
		"learned":      learned,
		"days":         daily,
	})
}

func contextWithTimeout(c *gin.Context) (ctx context.Context, cancel context.CancelFunc) {
	ctx, cancel = context.WithTimeout(c.Request.Context(), 5*time.Second)
	return
}
