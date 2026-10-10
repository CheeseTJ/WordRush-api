package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"wordrush/internal/auth"
	"wordrush/internal/httpx"
	"wordrush/internal/speak"
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

// getProfile 返回学习档案（XP、连续天数、红心、宝石）。
func getProfile(c *gin.Context, d Deps) {
	p, err := d.Store.Profile(c.Request.Context(), auth.UserID(c))
	if err != nil {
		httpx.Internal(c, err, "读取档案失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"xp":            p.XP,
		"streak_days":   p.StreakDays,
		"last_day":      p.LastDay,
		"level":         p.XP/500 + 1,
		"hearts":        p.Hearts,
		"gems":          p.Gems,
		"streak_freeze": p.StreakFreeze,
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

// postQuizFinish 提交关卡结果，返回更新后的档案与经济结算。
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
	out, err := d.Store.FinishQuiz(c.Request.Context(), auth.UserID(c), req)
	if err != nil {
		httpx.BadRequest(c, err.Error())
		return
	}
	p := out.Profile
	c.JSON(http.StatusOK, gin.H{
		"xp":            p.XP,
		"streak_days":   p.StreakDays,
		"level":         p.XP/500 + 1,
		"xp_gained":     req.Correct * 15,
		"hearts":        p.Hearts,
		"gems":          p.Gems,
		"streak_freeze": p.StreakFreeze,
		"gems_gained":   out.GemsGained,
		"hearts_lost":   out.HeartsLost,
		"failed":        out.Failed,
	})
}

// getTasks 返回今日三条每日任务。
func getTasks(c *gin.Context, d Deps) {
	tasks, err := d.Store.Tasks(c.Request.Context(), auth.UserID(c))
	if err != nil {
		httpx.Internal(c, err, "读取每日任务失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks})
}

// postTaskClaim 领取每日任务奖励。
func postTaskClaim(c *gin.Context, d Deps) {
	var req struct {
		ID string `json:"id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.ID == "" {
		httpx.BadRequest(c, "id 不能为空")
		return
	}
	gems, err := d.Store.ClaimTask(c.Request.Context(), auth.UserID(c), req.ID)
	if err != nil {
		httpx.BadRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"gems": gems})
}

// postHeartsRefill 花 25 宝石补满红心。
func postHeartsRefill(c *gin.Context, d Deps) {
	p, err := d.Store.RefillHearts(c.Request.Context(), auth.UserID(c))
	if err != nil {
		httpx.BadRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"hearts": p.Hearts,
		"gems":   p.Gems,
	})
}

// postBuyFreeze 花 200 宝石购买 1 个连胜冻结。
func postBuyFreeze(c *gin.Context, d Deps) {
	p, err := d.Store.BuyFreeze(c.Request.Context(), auth.UserID(c))
	if err != nil {
		httpx.BadRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"streak_freeze": p.StreakFreeze,
		"gems":          p.Gems,
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

// postSpeakAssess 口语跟读评分（迭代四）：前端上传音频，转发 OpenPronounce 打分。
func postSpeakAssess(c *gin.Context, d Deps) {
	if d.Cfg.SpeakEval.BaseURL == "" {
		httpx.BadRequest(c, "评测服务未开启")
		return
	}
	word := strings.TrimSpace(c.PostForm("word"))
	if word == "" || len(word) > 64 {
		httpx.BadRequest(c, "word 不能为空且不超过 64 字符")
		return
	}
	fh, err := c.FormFile("audio")
	if err != nil {
		httpx.BadRequest(c, "audio 文件缺失")
		return
	}
	if fh.Size > 2<<20 {
		httpx.BadRequest(c, "音频超过 2MB 上限")
		return
	}
	f, err := fh.Open()
	if err != nil {
		httpx.BadRequest(c, "音频读取失败")
		return
	}
	defer f.Close()
	audio, err := io.ReadAll(io.LimitReader(f, 2<<20+1))
	if err != nil {
		httpx.Internal(c, err, "音频读取失败")
		return
	}

	ctx, cancel := contextWithTimeout(c)
	defer cancel()
	client := speak.NewClient(d.Cfg.SpeakEval.BaseURL, d.Cfg.SpeakEval.TimeoutDuration())
	res, err := client.Assess(ctx, word, audio, fh.Header.Get("Content-Type"))
	if err != nil {
		if errors.Is(err, speak.ErrUnavailable) {
			// 降级约定：503 + unavailable，前端走浏览器识别
			c.JSON(http.StatusServiceUnavailable, gin.H{"code": "unavailable", "message": err.Error()})
			return
		}
		httpx.BadRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, res)
}

func contextWithTimeout(c *gin.Context) (ctx context.Context, cancel context.CancelFunc) {
	ctx, cancel = context.WithTimeout(c.Request.Context(), 5*time.Second)
	return
}
