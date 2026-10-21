package store

import (
	"slices"
	"strings"
	"testing"
)

// TestReviewQueueSQL 校验复习队列 SQL 的拼装规则。
//
// 这段逻辑值得单测：参数顺序一旦错位，NOT IN 会比对到错误的列，
// 或者占位符数与参数数不匹配导致运行时报错，而它在正常路径下看不出问题。
func TestReviewQueueSQL(t *testing.T) {
	t.Run("无 exclude", func(t *testing.T) {
		q, args := reviewQueueSQL("u1", "2026-10-21", 10, nil)

		if strings.Contains(q, "NOT IN") {
			t.Error("没有 exclude 时不应出现 NOT IN")
		}
		if got := strings.Count(q, "?"); got != 3 {
			t.Errorf("占位符应为 3 个（user / today / limit），实际 %d", got)
		}
		if len(args) != 3 {
			t.Fatalf("参数应为 3 个，实际 %d", len(args))
		}
		if args[0] != "u1" || args[1] != "2026-10-21" || args[2] != 10 {
			t.Errorf("参数顺序错误: %#v", args)
		}
	})

	t.Run("有 exclude", func(t *testing.T) {
		ex := []string{"w1", "w2", "w3"}
		q, args := reviewQueueSQL("u1", "2026-10-21", 10, ex)

		if !strings.Contains(q, "word_id NOT IN (?,?,?)") {
			t.Errorf("NOT IN 占位符不正确:\n%s", q)
		}
		// user + today + 3 个 exclude + limit
		if got := strings.Count(q, "?"); got != 6 {
			t.Errorf("占位符应为 6 个，实际 %d", got)
		}
		if len(args) != 6 {
			t.Fatalf("参数应为 6 个，实际 %d", len(args))
		}
		if args[0] != "u1" || args[1] != "2026-10-21" {
			t.Errorf("前缀参数错误: %#v", args[:2])
		}
		if !slices.Equal(args[2:5], []any{"w1", "w2", "w3"}) {
			t.Errorf("exclude 参数应为 w1,w2,w3，实际 %#v", args[2:5])
		}
		if args[5] != 10 {
			t.Errorf("LIMIT 必须是最后一个参数，实际 %v", args[5])
		}
	})

	t.Run("排序与 LIMIT 子句位置", func(t *testing.T) {
		q, _ := reviewQueueSQL("u1", "d", 5, []string{"a"})
		notIn := strings.Index(q, "NOT IN")
		order := strings.Index(q, "ORDER BY")
		if notIn < 0 || order < 0 {
			t.Fatalf("缺少子句:\n%s", q)
		}
		// NOT IN 必须在 ORDER BY 之前，否则语法错误
		if notIn > order {
			t.Error("NOT IN 必须在 ORDER BY 之前")
		}
		if !strings.HasSuffix(strings.TrimSpace(q), "LIMIT ?") {
			t.Errorf("SQL 应以 LIMIT ? 结尾:\n%s", q)
		}
	})

	t.Run("exclude 超长被截断到 500", func(t *testing.T) {
		ex := make([]string, 800)
		for i := range ex {
			ex[i] = "word"
		}
		q, args := reviewQueueSQL("u1", "d", 10, ex)

		if got := strings.Count(q, "?"); got != 1+1+500+1 {
			t.Errorf("截断后占位符应为 503，实际 %d", got)
		}
		if len(args) != 503 {
			t.Errorf("参数应为 503 个，实际 %d", len(args))
		}
	})

	t.Run("SQL 注入尝试被参数化", func(t *testing.T) {
		evil := "w1') OR 1=1; DROP TABLE word_stats; --"
		q, args := reviewQueueSQL("u1", "d", 10, []string{evil})

		if strings.Contains(q, "DROP TABLE") {
			t.Error("恶意输入不应出现在 SQL 文本里（必须参数化）")
		}
		if args[2] != evil {
			t.Error("恶意输入应原样作为参数传递")
		}
	})
}
