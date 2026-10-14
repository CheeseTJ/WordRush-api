package api

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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

/* ==================== 口语对话（自由闲聊 + 情景对话） ==================== */

// chatSystemPrompt 是自由闲聊（遗留模式，前端已不再暴露）的角色设定：
// 简单英文、顺带纠错、引导继续对话。
const chatSystemPrompt = "You are a friendly English conversation partner for a Chinese learner of English. " +
	"Reply in simple, natural everyday English, at most 2 short sentences. " +
	"If the user makes an obvious grammar or word mistake, gently correct it first, " +
	"then keep the conversation going with a short question."

// maxChatTurns 是单次转发给模型的最大消息条数（只取最近若干条，控制上下文与额度）。
const maxChatTurns = 10

// chatScenario 是一个情景对话剧本。人设与目标只存在服务端，
// 前端仅传 scenario_id —— 防止前端伪造角色或篡改目标。
type chatScenario struct {
	Name  string   // 中文名（结算批改 prompt 用）
	Role  string   // AI 扮演的角色
	Scene string   // 场景一句话描述
	Level string   // 学习者水平（CEFR）
	Turns int      // 学习者回合上限
	Goals []string // 任务目标（英文，给模型）
}

// chatScenarioTurns 是所有情景共用的回合上限：有限对话，到轮即结算。
const chatScenarioTurns = 6

// chatScenarios 情景池（与前端 src/data/scenarios.ts 的 id 保持一致）。
var chatScenarios = map[string]chatScenario{
	"coffee": {
		Name: "咖啡店点单", Role: "a friendly barista at Bean & Leaf coffee shop",
		Scene: "the learner walks into a coffee shop to order a drink",
		Level: "A2", Turns: chatScenarioTurns,
		Goals: []string{"order a drink", "ask about the price", "ask for less sugar or no sugar"},
	},
	"airport": {
		Name: "机场值机", Role: "a check-in agent at the airport",
		Scene: "the learner is checking in for a flight and checking one suitcase",
		Level: "A2+", Turns: chatScenarioTurns,
		Goals: []string{"check in for the flight", "check in a suitcase and ask about the weight limit"},
	},
	"hotel": {
		Name: "酒店入住", Role: "a receptionist at a hotel front desk",
		Scene: "the learner arrives at a hotel with a reservation and asks about breakfast and Wi-Fi",
		Level: "A2", Turns: chatScenarioTurns,
		Goals: []string{"check in with a reservation", "ask about breakfast time", "ask for the Wi-Fi password"},
	},
	"restaurant": {
		Name: "餐厅点餐", Role: "a waiter at a cozy noodle restaurant",
		Scene: "the learner orders a main dish and a drink at a restaurant, then asks for the bill",
		Level: "A2", Turns: chatScenarioTurns,
		Goals: []string{"order a main dish and a drink", "ask what the waiter recommends", "ask for the bill"},
	},
	"shopping": {
		Name: "退换货", Role: "a shop assistant at a clothing store",
		Scene: "the learner wants to return a jacket that is the wrong size and asks for a refund or exchange",
		Level: "B1", Turns: chatScenarioTurns,
		Goals: []string{"explain the problem with the item", "ask for a refund or exchange", "ask about the refund policy or time"},
	},
	"doctor": {
		Name: "看医生", Role: "a doctor at a clinic",
		Scene: "the learner has had a sore throat and a cough for two days and describes it to the doctor",
		Level: "B1", Turns: chatScenarioTurns,
		Goals: []string{"describe the symptoms", "mention how long it has lasted", "ask for advice or medicine"},
	},
	"interview": {
		Name: "面试自我介绍", Role: "an HR interviewer at a friendly tech company",
		Scene: "a job interview: the learner introduces themselves and talks about strengths and experience",
		Level: "B1+", Turns: chatScenarioTurns,
		Goals: []string{"give a self-introduction", "talk about one strength with an example", "ask one question about the job"},
	},
	"rent": {
		Name: "租房看房", Role: "a landlord showing an apartment",
		Scene: "the learner views an apartment to rent and asks about rent, bills and the contract",
		Level: "B1+", Turns: chatScenarioTurns,
		Goals: []string{"ask about the rent and bills", "ask about one detail of the apartment (e.g. furniture, pets)", "ask about the contract or move-in date"},
	},
}

// scenarioSystemPrompt 拼装情景对话的 system prompt：
// 人设 + 目标 + 回合约束 + 输出格式（[GOALS:...] / [DONE:...] 标记行）。
func scenarioSystemPrompt(s chatScenario, turn int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are role-playing %s in an English practice scenario: %s. ", s.Role, s.Scene)
	fmt.Fprintf(&b, "The learner is a Chinese learner of English (level about %s).\n", s.Level)
	b.WriteString("Rules:\n")
	b.WriteString("- Stay in character. Reply in simple, natural spoken English, at most 2 short sentences.\n")
	b.WriteString("- Gently guide the learner toward these goals: ")
	for i, g := range s.Goals {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%d) %s", i+1, g)
	}
	b.WriteString("\n")
	b.WriteString("- If the learner makes an obvious grammar or word mistake, model the correct sentence naturally in your reply without lecturing.\n")
	if turn >= s.Turns {
		fmt.Fprintf(&b, "- The conversation lasts at most %d learner turns, and this is the FINAL turn: help the learner wrap the scene up and say goodbye.\n", s.Turns)
	} else {
		fmt.Fprintf(&b, "- The conversation lasts at most %d learner turns; this is turn %d. Keep the scene going naturally.\n", s.Turns, turn)
	}
	b.WriteString("Output format (strict): first your in-character reply as plain text (no emoji, no markdown); ")
	b.WriteString("then one line exactly like [GOALS:1,0,0] marking which goals are completed after this turn ")
	b.WriteString("(1 = done, 0 = not yet, one digit per goal in the same order); ")
	b.WriteString("then [DONE:no] — only write [DONE:yes] when ALL goals are complete. Never mention or explain these markers.")
	return b.String()
}

// chatGoalRe / chatDoneRe 匹配模型回复尾部的结构化标记行。
var (
	chatGoalRe = regexp.MustCompile(`\[GOALS:\s*([01,\s]+?)\]`)
	chatDoneRe = regexp.MustCompile(`\[DONE:\s*(yes|no)\s*\]`)
)

// parseChatMarkers 剥掉标记行，解析目标完成情况。
// 模型偶尔会漏掉标记：ok=false 时前端保持原状即可，不影响对话流。
func parseChatMarkers(reply string) (clean string, goals []bool, done bool, ok bool) {
	done = false
	if m := chatDoneRe.FindAllStringSubmatch(reply, -1); len(m) > 0 {
		done = m[len(m)-1][1] == "yes"
	}
	matches := chatGoalRe.FindAllStringSubmatch(reply, -1)
	clean = strings.TrimSpace(chatGoalRe.ReplaceAllString(chatDoneRe.ReplaceAllString(reply, ""), ""))
	if len(matches) == 0 {
		return clean, nil, done, false
	}
	fields := strings.FieldsFunc(matches[len(matches)-1][1], func(r rune) bool { return r == ',' || r == ' ' })
	goals = make([]bool, 0, len(fields))
	for _, f := range fields {
		goals = append(goals, f == "1")
	}
	return clean, goals, done, true
}

// postChat 转发对话消息给 Workers AI。
// 带 scenario_id 时为情景对话模式：后端拼装人设与目标、校验回合数，
// 并解析模型回复中的 [GOALS]/[DONE] 标记；否则为遗留的自由闲聊模式。
func postChat(c *gin.Context, d Deps) {
	if !d.Cfg.AI.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "unavailable", "message": "对话服务未开启"})
		return
	}
	var req struct {
		Messages []ai.Message `json:"messages"`
		// Thinking 按请求覆盖思考模式；缺省（nil）时用服务端配置默认值
		Thinking *bool `json:"thinking"`
		// 情景对话模式：scenario_id 定位剧本，turn 是本次请求的回合序号（1 起）
		ScenarioID string `json:"scenario_id"`
		Turn       int    `json:"turn"`
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

	// 情景模式：剧本必须在服务端注册表里，回合数由后端闸住
	systemPrompt := chatSystemPrompt
	var scn *chatScenario
	if req.ScenarioID != "" {
		s, ok := chatScenarios[req.ScenarioID]
		if !ok {
			httpx.BadRequest(c, "未知情景")
			return
		}
		turn := req.Turn
		if turn < 1 {
			turn = 1
		}
		if turn > s.Turns {
			c.JSON(http.StatusBadRequest, gin.H{"code": "turns_exhausted", "message": "本轮对话已用完，请进入结算"})
			return
		}
		scn = &s
		systemPrompt = scenarioSystemPrompt(s, turn)
	}

	msgs := append([]ai.Message{{Role: "system", Content: systemPrompt}}, req.Messages...)

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

	// 情景模式：剥掉标记行，回传目标进度（标记缺失时 goals 为 null，前端保持原状）
	if scn != nil {
		clean, goals, done, _ := parseChatMarkers(reply)
		c.JSON(http.StatusOK, gin.H{"reply": clean, "goals": goals, "done": done})
		return
	}
	c.JSON(http.StatusOK, gin.H{"reply": reply})
}

// chatSummaryPrompt 拼装结算批改的 system prompt：只要一个 JSON。
func chatSummaryPrompt(s chatScenario, hit, total, turns int) string {
	var b strings.Builder
	b.WriteString("You are an English tutor reviewing a finished role-play dialogue with a Chinese learner of English.\n")
	fmt.Fprintf(&b, "Scenario: %s — %s. The learner had %d turns; goals completed: %d/%d. The goals were: ", s.Name, s.Scene, turns, hit, total)
	for i, g := range s.Goals {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%d) %s", i+1, g)
	}
	b.WriteString("\nWrite a short review. Reply with ONLY minified JSON (no markdown fences, no extra text):\n")
	b.WriteString(`{"stars":N,"comment":"...","phrases":[{"en":"...","zh":"..."}],"fixes":[{"bad":"...","good":"...","why":"..."}]}` + "\n")
	b.WriteString("- stars: 3 if all goals completed, 2 if at least half, otherwise 1.\n")
	b.WriteString("- comment: ONE short encouraging sentence in simplified Chinese (no more than 40 characters).\n")
	b.WriteString("- phrases: 2 or 3 useful expressions from (or fitting) this dialogue; en = the English sentence, zh = 简体中文释义.\n")
	b.WriteString("- fixes: 0 to 2 real mistakes the learner actually made; bad = learner's original sentence, good = natural correction, why = 简短中文解释 (no more than 40 characters). Use an empty array if there were none.")
	return b.String()
}

// chatSummaryBody 是结算批改的结构化结果。
type chatSummaryBody struct {
	Comment string `json:"comment"`
	Phrases []struct {
		En string `json:"en"`
		Zh string `json:"zh"`
	} `json:"phrases"`
	Fixes []struct {
		Bad  string `json:"bad"`
		Good string `json:"good"`
		Why  string `json:"why"`
	} `json:"fixes"`
}

// parseChatSummary 从模型输出里抠出 JSON；失败时返回兜底结构，结算流程不中断。
func parseChatSummary(raw string) chatSummaryBody {
	out := chatSummaryBody{Comment: "本次练习完成，继续保持！"}
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return out
	}
	var body chatSummaryBody
	if err := json.Unmarshal([]byte(raw[start:end+1]), &body); err != nil {
		return out
	}
	if strings.TrimSpace(body.Comment) != "" {
		out.Comment = strings.TrimSpace(body.Comment)
	}
	out.Phrases, out.Fixes = body.Phrases, body.Fixes
	return out
}

// postChatSummary 情景对话结算：批改整场对话，并按目标达成率发 XP/宝石。
// 星级由服务端按达成率推算（不依赖模型自由发挥）；模型只负责评语/表达/纠错，
// 批改失败时降级为默认评语，保证结算与发奖永远成功。
func postChatSummary(c *gin.Context, d Deps) {
	if !d.Cfg.AI.Enabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": "unavailable", "message": "对话服务未开启"})
		return
	}
	var req struct {
		ScenarioID string       `json:"scenario_id"`
		Messages   []ai.Message `json:"messages"`
		GoalsHit   int          `json:"goals_hit"`
		GoalsTotal int          `json:"goals_total"`
		TurnsUsed  int          `json:"turns_used"`
		Thinking   *bool        `json:"thinking"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.BadRequest(c, "请求体非法："+err.Error())
		return
	}
	scn, ok := chatScenarios[req.ScenarioID]
	if !ok {
		httpx.BadRequest(c, "未知情景")
		return
	}
	if len(req.Messages) == 0 || len(req.Messages) > 24 {
		httpx.BadRequest(c, "messages 需在 1~24 条之间")
		return
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
	if req.GoalsTotal <= 0 || req.GoalsTotal != len(scn.Goals) {
		req.GoalsTotal = len(scn.Goals)
	}
	if req.GoalsHit < 0 {
		req.GoalsHit = 0
	}
	if req.GoalsHit > req.GoalsTotal {
		req.GoalsHit = req.GoalsTotal
	}
	turns := req.TurnsUsed
	if turns < 1 {
		turns = 1
	}
	if turns > scn.Turns {
		turns = scn.Turns
	}

	// 星级：按目标达成率推算（3 星 = 全部达成，2 星 = 过半，其余 1 星）
	stars := 1
	if req.GoalsTotal > 0 {
		if req.GoalsHit == req.GoalsTotal {
			stars = 3
		} else if req.GoalsHit*2 >= req.GoalsTotal {
			stars = 2
		}
	}

	// 批改（失败降级为默认评语，不让结算挂掉）
	summary := chatSummaryBody{Comment: "本次练习完成，继续保持！"}
	thinking := false
	if req.Thinking != nil {
		thinking = *req.Thinking
	}
	maxTokens := 600
	if thinking {
		maxTokens = 1400
	}
	var tBuf strings.Builder
	for _, m := range req.Messages {
		if m.Role == "user" {
			tBuf.WriteString("Learner: ")
		} else {
			tBuf.WriteString("Partner: ")
		}
		tBuf.WriteString(strings.ReplaceAll(m.Content, "\n", " "))
		tBuf.WriteByte('\n')
	}
	msgs := []ai.Message{
		{Role: "system", Content: chatSummaryPrompt(scn, req.GoalsHit, req.GoalsTotal, turns)},
		{Role: "user", Content: "Dialogue transcript:\n" + tBuf.String()},
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), d.Cfg.AI.TimeoutDuration())
	defer cancel()
	client := ai.NewClient(d.Cfg.AI.Endpoint(), d.Cfg.AI.APIToken, d.Cfg.AI.ModelOrDefault(), thinking, d.Cfg.AI.TimeoutDuration())
	if raw, err := client.Chat(ctx, msgs, maxTokens); err == nil {
		summary = parseChatSummary(raw)
	}

	// 发奖：次数与上限都在服务端
	xp, gems, err := d.Store.CompleteScenario(c.Request.Context(), auth.UserID(c), stars)
	if err != nil {
		httpx.Internal(c, err, "情景对话结算失败")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"stars":       stars,
		"xp_gained":   xp,
		"gems_gained": gems,
		"comment":     summary.Comment,
		"phrases":     summary.Phrases,
		"fixes":       summary.Fixes,
	})
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
