package store

import (
	"context"
	"database/sql"
	"fmt"
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
	XP         int    `json:"xp"`
	StreakDays int    `json:"streak_days"`
	LastDay    string `json:"last_day,omitempty"`
}

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

// FinishQuiz 提交一次关卡结果：进度 + 单词正误 + 每日统计 + 档案一次事务写完。
// XP 由服务端按正确数重算（每题 15 分），不信任前端给值。
func (s *Store) FinishQuiz(ctx context.Context, userID string, r QuizResult) (*Profile, error) {
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

	// 4. 档案：XP 累加 + 连续天数推进（last_day 为昨天则 +1，今天不变，否则重置 1）
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO profiles (user_id, xp, streak_days, last_day, updated_at)
		VALUES (?, ?, 1, ?, ?)
		ON DUPLICATE KEY UPDATE
			xp          = xp + VALUES(xp),
			streak_days = IF(
				last_day = DATE_SUB(VALUES(last_day), INTERVAL 1 DAY), streak_days + 1,
				IF(last_day = VALUES(last_day), streak_days, 1)),
			last_day    = VALUES(last_day),
			updated_at  = VALUES(updated_at)
	`, userID, xp, day, nowUnix); err != nil {
		return nil, err
	}

	prof, err := s.profileTx(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	return prof, tx.Commit()
}

// ReviewQueue 返回复习到期池：due_date <= 今天（东八区）的词，到期早的优先。
// total 即「今日到期总数」——做完后归零，前端展示「今日复习已完成」。
func (s *Store) ReviewQueue(ctx context.Context, userID string, limit int) ([]string, int, error) {
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

	rows, err := s.db.QueryContext(ctx, `
		SELECT word_id FROM word_stats
		WHERE user_id = ? AND due_date IS NOT NULL AND due_date <= ?
		ORDER BY due_date ASC, (wrong - correct) DESC
		LIMIT ?
	`, userID, today, limit)
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

// ReviewResult 记录复习正误（只推进 word_stats 的 SRS，不计 XP 与关卡）。
func (s *Store) ReviewResult(ctx context.Context, userID string, results []WordResult) error {
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, w := range results {
		if w.WordID == "" {
			continue
		}
		if err := upsertWordSRS(ctx, tx, userID, w.WordID, w.OK, now); err != nil {
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

// Profile 读取用户档案；无记录时返回零值档案。
func (s *Store) Profile(ctx context.Context, userID string) (*Profile, error) {
	return s.profileTx(ctx, s.db, userID)
}

func (s *Store) profileTx(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, userID string) (*Profile, error) {
	var p Profile
	var lastDay sql.NullString
	err := q.QueryRowContext(ctx,
		`SELECT xp, streak_days, last_day FROM profiles WHERE user_id = ?`, userID,
	).Scan(&p.XP, &p.StreakDays, &lastDay)
	if err == sql.ErrNoRows {
		return &Profile{}, nil
	}
	if err != nil {
		return nil, err
	}
	p.LastDay = lastDay.String
	return &p, nil
}
