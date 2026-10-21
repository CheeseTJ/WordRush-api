package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// UnitProgress 是一个关卡的学习进度。
type UnitProgress struct {
	UnitID    string `json:"unit_id"`
	Stars     int    `json:"stars"`
	BestCombo int    `json:"best_combo"`
	Correct   int    `json:"correct"`
	Total     int    `json:"total"`
	UpdatedAt int64  `json:"updated_at"`
}

// Profile 是用户学习档案。
type Profile struct {
	XP           int    `json:"xp"`
	StreakDays   int    `json:"streak_days"`
	LastDay      string `json:"last_day,omitempty"`
	Hearts       int    `json:"hearts"`
	Gems         int    `json:"gems"`
	StreakFreeze int    `json:"streak_freeze"`
	HeartsAt     int64  `json:"-"` // 回心计时锚点，不外露
}

// FinishOutcome 是一次关卡提交的经济结算结果。
type FinishOutcome struct {
	Profile    *Profile
	GemsGained int
	HeartsLost int
	Failed     bool // 红心归零：前端弹失败结算页
}

// 经济系统常量（迭代三）。
const (
	maxHearts        = 5                // 红心上限
	heartRefill      = 30 * time.Minute // 每 30 分钟自然回 1 心
	refillCost       = 25               // 宝石补满红心
	freezeCost       = 200              // 宝石购买连胜冻结
	freezeCap        = 2                // 冻结持有上限
	gemPass          = 10               // 通关奖励
	gemCombo5        = 5                // 连击 5 里程碑
	taskNewTarget    = 20               // 每日任务：新学 20 词
	taskReviewTarget = 10               // 每日任务：复习 10 词
	gemTaskNew       = 20
	gemTaskReview    = 10
	gemTaskStar      = 15
	gemTaskChat      = 10 // 每日任务：完成 1 次情景对话

	// 情景对话奖励（迭代五）：按星级发 XP/宝石，每日最多 chatDailyCap 次有奖励，
	// 防止反复刷同一场对话无限套利。
	chatDailyCap  = 3
	chatXpPerStar = 5
	chatGem3Star  = 2
	chatGem2Star  = 1
)

// DailyStat 是一天的学习统计。
type DailyStat struct {
	Day     string `json:"day"`
	Words   int    `json:"words"`
	Correct int    `json:"correct"`
	Wrong   int    `json:"wrong"`
	XP      int    `json:"xp"`
}

// WordResult 是一道题的正误记录。
type WordResult struct {
	WordID string `json:"id"`
	OK     bool   `json:"ok"`
}

// QuizResult 是一次关卡提交。
type QuizResult struct {
	UnitID    string       `json:"unit_id"`
	Correct   int          `json:"correct"`
	Total     int          `json:"total"`
	BestCombo int          `json:"best_combo"`
	Stars     int          `json:"stars"`
	Words     []WordResult `json:"words"`
}

// chinaDay 把当前时刻换算为东八区日期字符串（YYYY-MM-DD）。
//
// 中国无夏令时，固定 +8 偏移即可，不依赖 tzdata。
func chinaDay(ts time.Time) string {
	return ts.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02")
}

// EnsureUser 把 JWT 身份落到影子用户表；带节流，避免每次请求都写。
func (s *Store) EnsureUser(ctx context.Context, id, email, role string) error {
	now := time.Now().Unix()

	s.mu.Lock()
	if t, ok := s.touched[id]; ok && time.Since(t) < touchInterval {
		s.mu.Unlock()
		return nil
	}
	s.touched[id] = time.Now()
	s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (id, email, role, created_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE email = VALUES(email), role = VALUES(role), last_seen_at = VALUES(last_seen_at)
	`, id, email, role, now, now)
	return err
}

// Progress 返回用户全部关卡进度。
func (s *Store) Progress(ctx context.Context, userID string) ([]UnitProgress, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT unit_id, stars, best_combo, correct, total, updated_at
		FROM unit_progress WHERE user_id = ? ORDER BY updated_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []UnitProgress{}
	for rows.Next() {
		var p UnitProgress
		if err := rows.Scan(&p.UnitID, &p.Stars, &p.BestCombo, &p.Correct, &p.Total, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// refillHearts 按当前时刻惰性补心，返回补算后的红心数与计时锚点。
// hearts==maxHearts 时锚点归零（满血不计时）。
func refillHearts(hearts int, heartsAt, now int64) (int, int64) {
	if hearts >= maxHearts {
		return maxHearts, 0
	}
	if heartsAt <= 0 {
		heartsAt = now // 历史数据无锚点，从现在起算
	}
	sec := int64(heartRefill / time.Second)
	gain := (now - heartsAt) / sec
	if gain <= 0 {
		return hearts, heartsAt
	}
	hearts += int(gain)
	if hearts >= maxHearts {
		return maxHearts, 0
	}
	return hearts, heartsAt + gain*sec
}

// FinishQuiz 提交一次关卡结果：进度 + 单词正误 + 每日统计 + 档案一次事务写完。
// XP 由服务端按正确数重算（每题 15 分），不信任前端给值。
// 迭代三经济结算：按答错数扣红心（归零 = 本关失败）、通关/三星/连击发宝石、断签自动消耗连胜冻结。
func (s *Store) FinishQuiz(ctx context.Context, userID string, r QuizResult) (*FinishOutcome, error) {
	if r.Total <= 0 || r.Correct < 0 || r.Correct > r.Total {
		return nil, fmt.Errorf("结果数据非法")
	}
	if r.Stars < 0 {
		r.Stars = 0
	}
	if r.Stars > 3 {
		r.Stars = 3
	}
	xp := r.Correct * 15
	if xp > 200 {
		xp = 200
	}
	now := time.Now()
	nowUnix := now.Unix()
	day := chinaDay(now)
	words := r.Total
	if words <= 0 {
		words = len(r.Words)
	}
	wrong := words - r.Correct
	if wrong < 0 {
		wrong = 0
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	prof, err := profileTx(ctx, tx, userID)
	if err != nil {
		return nil, err
	}

	// ---- 经济结算（迭代三）----
	// 红心：惰性补算后按答错数扣除；满血开始扣才启动回心计时
	hearts, heartsAt := refillHearts(prof.Hearts, prof.HeartsAt, nowUnix)
	heartsLost := wrong
	if heartsLost > hearts {
		heartsLost = hearts
	}
	heartsAfter := hearts - heartsLost
	if hearts == maxHearts && heartsAfter < maxHearts {
		heartsAt = nowUnix
	}
	// 宝石：通关 +10、三星 +15、连击 5 里程碑 +5
	gemsGained := 0
	if r.Stars > 0 {
		gemsGained += gemPass
	}
	if r.Stars == 3 {
		gemsGained += gemPass // 三星合计 +15
	}
	if r.BestCombo >= 5 {
		gemsGained += gemCombo5
	}

	// 1. 关卡进度：取历史最优
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO unit_progress (user_id, unit_id, stars, best_combo, correct, total, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			stars      = GREATEST(stars, VALUES(stars)),
			best_combo = GREATEST(best_combo, VALUES(best_combo)),
			correct    = correct + VALUES(correct),
			total      = total + VALUES(total),
			updated_at = VALUES(updated_at)
	`, userID, r.UnitID, r.Stars, r.BestCombo, r.Correct, r.Total, nowUnix); err != nil {
		return nil, err
	}

	// 2. 单词正误
	for _, w := range r.Words {
		if w.WordID == "" {
			continue
		}
		if err := upsertWordSRS(ctx, tx, userID, w.WordID, w.OK, now); err != nil {
			return nil, err
		}
	}

	// 3. 每日统计
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO daily_stats (user_id, day, words, correct, wrong, xp)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			words   = words + VALUES(words),
			correct = correct + VALUES(correct),
			wrong   = wrong + VALUES(wrong),
			xp      = xp + VALUES(xp)
	`, userID, day, words, r.Correct, wrong, xp); err != nil {
		return nil, err
	}

	// 4. 档案：XP 累加 + 连胜推进（带冻结）+ 经济状态回写
	//    连胜规则：last_day 为昨天则 +1；今天不变；断签且有冻结则保持连胜并消耗 1 个冻结；否则清零重计。
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO profiles (user_id, xp, streak_days, last_day, updated_at, hearts, hearts_at, gems, streak_freeze)
		VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			xp          = xp + VALUES(xp),
			streak_days = IF(
				last_day = DATE_SUB(VALUES(last_day), INTERVAL 1 DAY), streak_days + 1,
				IF(last_day = VALUES(last_day), streak_days,
					IF(streak_freeze > 0, streak_days, 1))),
			streak_freeze = IF(
				last_day >= DATE_SUB(VALUES(last_day), INTERVAL 1 DAY), streak_freeze,
				GREATEST(streak_freeze - 1, 0)),
			last_day    = VALUES(last_day),
			hearts      = VALUES(hearts),
			hearts_at   = VALUES(hearts_at),
			gems        = gems + VALUES(gems),
			updated_at  = VALUES(updated_at)
	`, userID, xp, day, nowUnix, heartsAfter, heartsAt, gemsGained, prof.StreakFreeze); err != nil {
		return nil, err
	}

	final, err := profileTx(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	out := &FinishOutcome{
		Profile:    final,
		GemsGained: gemsGained,
		HeartsLost: heartsLost,
		Failed:     final.Hearts <= 0,
	}
	return out, tx.Commit()
}

// ReviewQueue 返回复习到期池：due_date <= 今天（东八区）的词，到期早的优先。
// total 即「今日到期总数」——做完后归零，前端展示「今日复习已完成」。
// ReviewQueue 返回今日到期复习词。
//
// exclude 是客户端当天已经复习过（或已展示过）的词 id。为什么要它：
// 复习一个词会把它的 due_date 推到明天，但 SQL 排序是
// `due_date ASC, (wrong-correct) DESC`，同一批未复习的词仍会排在最前面。
// 客户端分批复习时，如果只靠 LIMIT 翻页，第 2 次请求又会拿到刚复习过的
// 那批词，导致要么死循环、要么永远看不到后面真正的新词。
func (s *Store) ReviewQueue(ctx context.Context, userID string, limit int, exclude []string) ([]string, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	today := chinaDay(time.Now())

	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM word_stats WHERE user_id = ? AND due_date IS NOT NULL AND due_date <= ?`,
		userID, today,
	).Scan(&total); err != nil {
		return nil, 0, err
	}

	// 动态拼 NOT IN，参数化避免注入。
	query, args := reviewQueueSQL(userID, today, limit, exclude)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, 0, err
		}
		out = append(out, id)
	}
	return out, total, rows.Err()
}

// reviewQueueSQL 拼装复习队列查询。
//
// 抽成独立函数是为了能离线测试 SQL 与参数顺序——这段逻辑出错的代价很高
// （参数错位会导致 NOT IN 到错误的列，或占位符数量与参数不匹配直接报错），
// 而它本身是纯函数，没必要为了测试引入数据库依赖。
//
// exclude 上限 500：占位符过多会让 SQL 文本膨胀，且当天复习上限远低于此。
func reviewQueueSQL(userID, today string, limit int, exclude []string) (string, []any) {
	query := `
		SELECT word_id FROM word_stats
		WHERE user_id = ? AND due_date IS NOT NULL AND due_date <= ?`
	args := []any{userID, today}

	if len(exclude) > 0 {
		if len(exclude) > 500 {
			exclude = exclude[:500]
		}
		placeholders := make([]string, len(exclude))
		for i, id := range exclude {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += ` AND word_id NOT IN (` + strings.Join(placeholders, ",") + `)`
	}

	query += ` ORDER BY due_date ASC, (wrong - correct) DESC LIMIT ?`
	args = append(args, limit)
	return query, args
}

// upsertWordSRS 写入单词正误并推进 SRS（简化 SM-2）。
// 首见词 ease=250；答对间隔沿 1->2->4->...->120 倍增封顶；答错间隔回 1、ease -20（下限 130）。
func upsertWordSRS(ctx context.Context, tx *sql.Tx, userID, wordID string, ok bool, now time.Time) error {
	var ease, interval int
	err := tx.QueryRowContext(ctx,
		`SELECT ease, interval_days FROM word_stats WHERE user_id = ? AND word_id = ?`,
		userID, wordID,
	).Scan(&ease, &interval)
	switch {
	case err == sql.ErrNoRows:
		ease, interval = 250, 0 // 新词首见
	case err != nil:
		return err
	}

	newEase, newInterval := ease, interval
	if ok {
		if interval > 0 {
			newInterval = interval * 2
			if newInterval > 120 {
				newInterval = 120
			}
		} else {
			newInterval = 1
		}
	} else {
		newEase = ease - 20
		if newEase < 130 {
			newEase = 130
		}
		newInterval = 1 // 明天再见
	}
	due := chinaDay(now.AddDate(0, 0, newInterval))

	okInt := 0
	if ok {
		okInt = 1
	}
	nowUnix := now.Unix()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO word_stats (user_id, word_id, correct, wrong, last_seen, ease, interval_days, due_date)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			correct       = correct + VALUES(correct),
			wrong         = wrong + VALUES(wrong),
			last_seen     = VALUES(last_seen),
			ease          = VALUES(ease),
			interval_days = VALUES(interval_days),
			due_date      = VALUES(due_date)
	`, userID, wordID, okInt, 1-okInt, nowUnix, newEase, newInterval, due)
	return err
}

// ReviewResult 记录复习正误（推进 SRS，不计 XP 与关卡；复习词数入每日统计供任务结算）。
func (s *Store) ReviewResult(ctx context.Context, userID string, results []WordResult) error {
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	n := 0
	for _, w := range results {
		if w.WordID == "" {
			continue
		}
		n++
		if err := upsertWordSRS(ctx, tx, userID, w.WordID, w.OK, now); err != nil {
			return err
		}
	}
	// 复习词数入当日统计（每日任务「复习 10 词」的进度来源）
	if n > 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO daily_stats (user_id, day, review_words) VALUES (?, ?, ?)
			ON DUPLICATE KEY UPDATE review_words = review_words + VALUES(review_words)
		`, userID, chinaDay(now), n); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Stats 返回档案 + 最近 days 天的每日统计。
func (s *Store) Stats(ctx context.Context, userID string, days int) (*Profile, []DailyStat, int, error) {
	if days <= 0 || days > 90 {
		days = 14
	}
	prof, err := s.Profile(ctx, userID)
	if err != nil {
		return nil, nil, 0, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT day, words, correct, wrong, xp FROM daily_stats
		WHERE user_id = ? ORDER BY day DESC LIMIT ?
	`, userID, days)
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()

	out := []DailyStat{}
	for rows.Next() {
		var st DailyStat
		var day sql.NullString
		if err := rows.Scan(&day, &st.Words, &st.Correct, &st.Wrong, &st.XP); err != nil {
			return nil, nil, 0, err
		}
		st.Day = day.String
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, err
	}

	var learned int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM word_stats WHERE user_id = ?`, userID,
	).Scan(&learned); err != nil {
		return nil, nil, 0, err
	}
	return prof, out, learned, nil
}

// Profile 读取用户档案；红心按当前时刻惰性补算后落库。
func (s *Store) Profile(ctx context.Context, userID string) (*Profile, error) {
	p, err := profileTx(ctx, s.db, userID)
	if err != nil {
		return nil, err
	}
	// 惰性回心：读取时把自然恢复落库，避免「多次读取数值不变」的观感
	if h, at := refillHearts(p.Hearts, p.HeartsAt, time.Now().Unix()); h != p.Hearts {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE profiles SET hearts = ?, hearts_at = ? WHERE user_id = ?`, h, at, userID); err != nil {
			return nil, err
		}
		p.Hearts, p.HeartsAt = h, at
	}
	return p, nil
}

func profileTx(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, userID string) (*Profile, error) {
	var p Profile
	var lastDay sql.NullString
	err := q.QueryRowContext(ctx,
		`SELECT xp, streak_days, last_day, hearts, hearts_at, gems, streak_freeze FROM profiles WHERE user_id = ?`, userID,
	).Scan(&p.XP, &p.StreakDays, &lastDay, &p.Hearts, &p.HeartsAt, &p.Gems, &p.StreakFreeze)
	if err == sql.ErrNoRows {
		return &Profile{Hearts: maxHearts}, nil
	}
	if err != nil {
		return nil, err
	}
	p.LastDay = lastDay.String
	return &p, nil
}

/* ==================== 迭代三：每日任务 / 宝石消费 ==================== */

// Task 是一条每日任务（进度从当日数据推导）。
type Task struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Target  int    `json:"target"`
	Current int    `json:"current"`
	Reward  int    `json:"reward"`
	Done    bool   `json:"done"`
	Claimed bool   `json:"claimed"`
}

// task_claimed 位图
const (
	taskBitNew    = 1 << iota // 新学 20 词
	taskBitReview             // 复习 10 词
	taskBitStar               // 三星一关
	taskBitChat               // 完成 1 次情景对话
)

// Tasks 返回今日三条任务：新学 20 词 / 复习 10 词 / 三星一关。
func (s *Store) Tasks(ctx context.Context, userID string) ([]Task, error) {
	today := chinaDay(time.Now())
	start, err := time.ParseInLocation("2006-01-02", today, time.FixedZone("CST", 8*3600))
	if err != nil {
		return nil, err
	}

	var words, reviewWords, chatDone, claimed int
	row := s.db.QueryRowContext(ctx,
		`SELECT words, review_words, chat_done, task_claimed FROM daily_stats WHERE user_id = ? AND day = ?`, userID, today)
	if err := row.Scan(&words, &reviewWords, &chatDone, &claimed); err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	var starCount int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM unit_progress WHERE user_id = ? AND stars = 3 AND updated_at >= ?`,
		userID, start.Unix(),
	).Scan(&starCount); err != nil {
		return nil, err
	}

	tasks := []Task{
		{ID: "new", Label: fmt.Sprintf("新学 %d 个单词", taskNewTarget), Target: taskNewTarget, Current: words, Reward: gemTaskNew},
		{ID: "review", Label: fmt.Sprintf("复习 %d 个单词", taskReviewTarget), Target: taskReviewTarget, Current: reviewWords, Reward: gemTaskReview},
		{ID: "star", Label: "拿到一次三星通关", Target: 1, Current: starCount, Reward: gemTaskStar},
		{ID: "chat", Label: "完成 1 次情景对话", Target: 1, Current: chatDone, Reward: gemTaskChat},
	}
	for i := range tasks {
		t := &tasks[i]
		if t.Current > t.Target {
			t.Current = t.Target
		}
		t.Done = t.Current >= t.Target
		switch t.ID {
		case "new":
			t.Claimed = claimed&taskBitNew != 0
		case "review":
			t.Claimed = claimed&taskBitReview != 0
		case "star":
			t.Claimed = claimed&taskBitStar != 0
		case "chat":
			t.Claimed = claimed&taskBitChat != 0
		}
	}
	return tasks, nil
}

// ClaimTask 领取一条已完成任务的宝石奖励（task_claimed 位图防重复）。
func (s *Store) ClaimTask(ctx context.Context, userID, id string) (int, error) {
	var bit, reward, target int
	switch id {
	case "new":
		bit, reward, target = taskBitNew, gemTaskNew, taskNewTarget
	case "review":
		bit, reward, target = taskBitReview, gemTaskReview, taskReviewTarget
	case "star":
		bit, reward, target = taskBitStar, gemTaskStar, 1
	case "chat":
		bit, reward, target = taskBitChat, gemTaskChat, 1
	default:
		return 0, fmt.Errorf("任务不存在")
	}

	today := chinaDay(time.Now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// 保证当日行存在
	if _, err := tx.ExecContext(ctx,
		`INSERT IGNORE INTO daily_stats (user_id, day) VALUES (?, ?)`, userID, today); err != nil {
		return 0, err
	}

	// 完成度校验（以库内数据为准，不信任前端）
	var cur int
	if id == "star" {
		start, e := time.ParseInLocation("2006-01-02", today, time.FixedZone("CST", 8*3600))
		if e != nil {
			return 0, e
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM unit_progress WHERE user_id = ? AND stars = 3 AND updated_at >= ?`,
			userID, start.Unix(),
		).Scan(&cur); err != nil {
			return 0, err
		}
	} else {
		// new/review/chat 各读自己的进度列（review 之前误读 words，已修正）
		col := map[string]string{"new": "words", "review": "review_words", "chat": "chat_done"}[id]
		if err := tx.QueryRowContext(ctx,
			`SELECT `+col+` FROM daily_stats WHERE user_id = ? AND day = ?`, userID, today,
		).Scan(&cur); err != nil {
			return 0, err
		}
	}
	if cur < target {
		return 0, fmt.Errorf("任务还未完成")
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE daily_stats SET task_claimed = task_claimed | ?
		WHERE user_id = ? AND day = ? AND task_claimed & ? = 0
	`, bit, userID, today, bit)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, fmt.Errorf("奖励已领取过")
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE profiles SET gems = gems + ? WHERE user_id = ?`, reward, userID); err != nil {
		return 0, err
	}
	var gems int
	if err := tx.QueryRowContext(ctx,
		`SELECT gems FROM profiles WHERE user_id = ?`, userID).Scan(&gems); err != nil {
		return 0, err
	}
	return gems, tx.Commit()
}

// RefillHearts 花 25 宝石补满红心。
func (s *Store) RefillHearts(ctx context.Context, userID string) (*Profile, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	prof, err := profileTx(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	hearts, _ := refillHearts(prof.Hearts, prof.HeartsAt, time.Now().Unix())
	if hearts >= maxHearts {
		return nil, fmt.Errorf("红心已满")
	}
	if prof.Gems < refillCost {
		return nil, fmt.Errorf("宝石不足（补满需要 %d）", refillCost)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE profiles SET hearts = ?, hearts_at = 0, gems = gems - ? WHERE user_id = ?`,
		maxHearts, refillCost, userID); err != nil {
		return nil, err
	}
	prof, err = profileTx(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	return prof, tx.Commit()
}

// BuyFreeze 花 200 宝石购买 1 个连胜冻结（最多持有 2 个）。
func (s *Store) BuyFreeze(ctx context.Context, userID string) (*Profile, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	prof, err := profileTx(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	if prof.StreakFreeze >= freezeCap {
		return nil, fmt.Errorf("冻结最多持有 %d 个", freezeCap)
	}
	if prof.Gems < freezeCost {
		return nil, fmt.Errorf("宝石不足（冻结需要 %d）", freezeCost)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE profiles SET streak_freeze = streak_freeze + 1, gems = gems - ? WHERE user_id = ?`,
		freezeCost, userID); err != nil {
		return nil, err
	}
	prof, err = profileTx(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	return prof, tx.Commit()
}

/* ==================== 迭代五：情景对话结算 ==================== */

// CompleteScenario 记录一次情景对话完成并结算奖励。
//
// 星级由服务端按目标达成率推算（见 api 层），这里只负责发奖与计数：
// 每日前 chatDailyCap 次有 XP/宝石（按星级），之后只累计次数不发奖，
// 防止反复刷同一场对话套利。XP 同时推进连胜（与通关同一套 last_day 规则）。
func (s *Store) CompleteScenario(ctx context.Context, userID string, stars int) (xpGained, gemsGained int, err error) {
	if stars < 0 {
		stars = 0
	}
	if stars > 3 {
		stars = 3
	}
	now := time.Now()
	day := chinaDay(now)
	nowUnix := now.Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	// 保证当日行存在
	if _, err := tx.ExecContext(ctx,
		`INSERT IGNORE INTO daily_stats (user_id, day) VALUES (?, ?)`, userID, day); err != nil {
		return 0, 0, err
	}

	// 每日奖励上限：超限只计数不发奖
	var chatDone int
	if err := tx.QueryRowContext(ctx,
		`SELECT chat_done FROM daily_stats WHERE user_id = ? AND day = ?`, userID, day,
	).Scan(&chatDone); err != nil {
		return 0, 0, err
	}
	xpGained, gemsGained = 0, 0
	if chatDone < chatDailyCap {
		xpGained = stars * chatXpPerStar
		switch stars {
		case 3:
			gemsGained = chatGem3Star
		case 2:
			gemsGained = chatGem2Star
		}
	}

	// 每日统计：次数 +1，XP 累加
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO daily_stats (user_id, day, chat_done, xp)
		VALUES (?, ?, 1, ?)
		ON DUPLICATE KEY UPDATE chat_done = chat_done + 1, xp = xp + VALUES(xp)
	`, userID, day, xpGained); err != nil {
		return 0, 0, err
	}

	// 档案：XP/宝石累加 + 连胜推进（与 FinishQuiz 同一套规则）
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO profiles (user_id, xp, streak_days, last_day, updated_at, gems)
		VALUES (?, ?, 1, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			xp          = xp + VALUES(xp),
			streak_days = IF(
				last_day = DATE_SUB(VALUES(last_day), INTERVAL 1 DAY), streak_days + 1,
				IF(last_day = VALUES(last_day), streak_days,
					IF(streak_freeze > 0, streak_days, 1))),
			streak_freeze = IF(
				last_day >= DATE_SUB(VALUES(last_day), INTERVAL 1 DAY), streak_freeze,
				GREATEST(streak_freeze - 1, 0)),
			last_day    = VALUES(last_day),
			gems        = gems + VALUES(gems),
			updated_at  = VALUES(updated_at)
	`, userID, xpGained, day, nowUnix, gemsGained); err != nil {
		return 0, 0, err
	}

	return xpGained, gemsGained, tx.Commit()
}
