package api

import (
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"wordrush/internal/ai"
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
	if word == "" || len(word) > 240 {
		httpx.BadRequest(c, "word 不能为空且不超过 240 字符（支持整句跟读）")
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

	// 评测冷启动（whisper 首次推理）可能超过通用 5s，用 speak_eval 专属超时
	ctx, cancel := context.WithTimeout(c.Request.Context(), d.Cfg.SpeakEval.TimeoutDuration())
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

// ttsMu 串行化同一文本的合成（避免并发重复调评测服务）。
var ttsMu sync.Mutex

// getSpeakTTS 发音合成（迭代四补充）：GET /speak/tts?text=word&voice=... → 16kHz WAV。
// 带磁盘缓存：同一「音色+文本」只合成一次，之后直接读缓存文件。
func getSpeakTTS(c *gin.Context, d Deps) {
	if d.Cfg.SpeakEval.BaseURL == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "unavailable", "message": "发音服务未开启"})
		return
	}
	text := strings.TrimSpace(c.Query("text"))
	if text == "" || len(text) > 240 {
		httpx.BadRequest(c, "text 不能为空且不超过 240 字符（支持整句合成）")
		return
	}
	voice := strings.TrimSpace(c.Query("voice"))
	if !speak.ValidVoice(voice) {
		httpx.BadRequest(c, "voice 非法（Piper id 格式，如 en_US-ryan-high）")
		return
	}

	// 缓存键含音色：不同音色互不干扰
	cacheDir := d.Cfg.SpeakEval.CacheDirOrDefault()
	key := fmt.Sprintf("%x.wav", sha1.Sum([]byte(voice + "|" + text)))
	cachePath := filepath.Join(cacheDir, key)
	if wav, err := os.ReadFile(cachePath); err == nil && len(wav) > 44 {
		serveWav(c, wav)
		return
	}

	// 加锁合成（锁内再查一次缓存，防并发重复）
	ttsMu.Lock()
	defer ttsMu.Unlock()
	if wav, err := os.ReadFile(cachePath); err == nil && len(wav) > 44 {
		serveWav(c, wav)
		return
	}

	// 首次使用新音色需从模型库下载（~60MB），给更宽的超时
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Minute)
	defer cancel()
	client := speak.NewClient(d.Cfg.SpeakEval.BaseURL, d.Cfg.SpeakEval.TimeoutDuration())
	wav, err := client.Synthesize(ctx, text, voice)
	if err != nil {
		if errors.Is(err, speak.ErrUnavailable) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"code": "unavailable", "message": err.Error()})
			return
		}
		httpx.BadRequest(c, err.Error())
		return
	}
	// 缓存写入失败不影响本次响应
	if err := os.MkdirAll(cacheDir, 0o755); err == nil {
		_ = os.WriteFile(cachePath, wav, 0o644)
	}
	serveWav(c, wav)
}

// serveWav 输出 WAV 并允许浏览器长期缓存（同一文本内容不变）。
func serveWav(c *gin.Context, wav []byte) {
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	c.Data(http.StatusOK, "audio/wav", wav)
}

/* ==================== 口语对话（语音 → AI → 语音） ==================== */

// chatSystemPrompt 是英语陪练的角色设定：简单英文、顺带纠错、引导继续对话。
const chatSystemPrompt = "You are a friendly English conversation partner for a Chinese learner of English. " +
	"Reply in simple, natural everyday English, at most 2 short sentences. " +
	"If the user makes an obvious grammar or word mistake, gently correct it first, " +
	"then keep the conversation going with a short question."

// maxChatTurns 是单次转发给模型的最大消息条数（只取最近若干条，控制上下文与额度）。
const maxChatTurns = 10

// postChat 转发对话消息给 Workers AI，返回助手回复文本。
func postChat(c *gin.Context, d Deps) {
	if !d.Cfg.AI.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "unavailable", "message": "对话服务未开启"})
		return
	}
	var req struct {
		Messages []ai.Message `json:"messages"`
		// Thinking 按请求覆盖思考模式；缺省（nil）时用服务端配置默认值
		Thinking *bool `json:"thinking"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.BadRequest(c, "请求体非法："+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		httpx.BadRequest(c, "messages 不能为空")
		return
	}
	// 只保留最近 N 条：长历史会放大输入 token，也拖慢响应
	if len(req.Messages) > maxChatTurns {
		req.Messages = req.Messages[len(req.Messages)-maxChatTurns:]
	}
	for _, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			httpx.BadRequest(c, "role 只能是 user / assistant")
			return
		}
		if strings.TrimSpace(m.Content) == "" || len(m.Content) > 2000 {
			httpx.BadRequest(c, "单条消息不能为空且不超过 2000 字符")
			return
		}
	}

	msgs := append([]ai.Message{{Role: "system", Content: chatSystemPrompt}}, req.Messages...)

	// 思考模式：按请求覆盖，缺省用服务端默认。开启时思考链会占输出额度，放宽 max_tokens。
	thinking := d.Cfg.AI.EnableThinking
	if req.Thinking != nil {
		thinking = *req.Thinking
	}
	maxTokens := 400
	if thinking {
		maxTokens = 1200
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), d.Cfg.AI.TimeoutDuration())
	defer cancel()
	client := ai.NewClient(d.Cfg.AI.Endpoint(), d.Cfg.AI.APIToken, d.Cfg.AI.ModelOrDefault(), thinking, d.Cfg.AI.TimeoutDuration())
	reply, err := client.Chat(ctx, msgs, maxTokens)
	if err != nil {
		if errors.Is(err, ai.ErrQuotaExhausted) {
			// 额度耗尽：返回稳定业务码，前端给"明天再来"的友好提示
			c.JSON(http.StatusTooManyRequests, gin.H{"code": "quota_exhausted", "message": "今日对话额度已用完"})
			return
		}
		if errors.Is(err, ai.ErrUnavailable) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"code": "unavailable", "message": err.Error()})
			return
		}
		httpx.Internal(c, err, "对话失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"reply": reply})
}

// postChatSTT 口语对话输入：整段录音 → OpenPronounce /speech2text → 文本。
func postChatSTT(c *gin.Context, d Deps) {
	if d.Cfg.SpeakEval.BaseURL == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "unavailable", "message": "语音服务未开启"})
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

	ctx, cancel := context.WithTimeout(c.Request.Context(), d.Cfg.SpeakEval.TimeoutDuration())
	defer cancel()
	client := speak.NewClient(d.Cfg.SpeakEval.BaseURL, d.Cfg.SpeakEval.TimeoutDuration())
	text, err := client.Transcribe(ctx, audio, fh.Header.Get("Content-Type"))
	if err != nil {
		if errors.Is(err, speak.ErrUnavailable) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"code": "unavailable", "message": err.Error()})
			return
		}
		httpx.BadRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"transcript": text})
}

func contextWithTimeout(c *gin.Context) (ctx context.Context, cancel context.CancelFunc) {
	ctx, cancel = context.WithTimeout(c.Request.Context(), 5*time.Second)
	return
}
