// Package store 封装学习记录数据库：连接、自动建库建表与查询。
//
// 数据存放在 OCI HeatWave（MySQL 兼容）中；词库内容打包在前端，
// 这里只存用户产生的数据（进度 / 正误 / 每日统计 / 档案）。
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql" // 注册 "mysql" 驱动

	"wordrush/internal/config"
)

//go:embed schema.sql
var schemaSQL string

// touchInterval 是影子用户表 last_seen_at 的写入节流间隔。
const touchInterval = 10 * time.Minute

// pingTimeout 是启动时探测数据库的等待上限。
const pingTimeout = 10 * time.Second

// Store 持有数据库连接与少量内存状态。
type Store struct {
	db *sql.DB

	mu      sync.Mutex
	touched map[string]time.Time
}

// Open 连接数据库；库不存在时自动创建，然后确保表结构存在。
func Open(cfg config.DB) (*Store, error) {
	if err := ensureDatabase(cfg); err != nil {
		return nil, err
	}

	dsn, err := cfg.ConnDSN()
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConnsOrDefault())
	db.SetMaxIdleConns(cfg.MaxIdleConnsOrDefault())
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime())

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接 HeatWave 失败（检查 db.host / db.name / db.user 与出网白名单）: %w", err)
	}
	if err := initSchema(ctx, db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{db: db, touched: make(map[string]time.Time)}, nil
}

// ensureDatabase 用不带库名的连接串执行 CREATE DATABASE IF NOT EXISTS。
//
// HeatWave 的账号通常具备建库权限（Imgbed 同款用法）；若权限不足会在这里
// 得到明确报错，而不是留下「连不上库」的模糊线索。
func ensureDatabase(cfg config.DB) error {
	dsn, err := cfg.NoDBDSN()
	if err != nil {
		return err
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("打开数据库连接失败: %w", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("连接 HeatWave 失败: %w", err)
	}
	name := strings.ReplaceAll(cfg.Name, "`", "")
	if _, err := db.ExecContext(ctx,
		"CREATE DATABASE IF NOT EXISTS `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		return fmt.Errorf("创建数据库 %s 失败（账号可能没有建库权限）: %w", cfg.Name, err)
	}
	return nil
}

// initSchema 逐条执行内嵌的 DDL，并对存量表做幂等列迁移。
//
// MySQL 驱动默认不允许一次发送多条语句，因此这里手工切分；
// 建表语句全部是 CREATE TABLE IF NOT EXISTS，重复执行是安全的。
func initSchema(ctx context.Context, db *sql.DB) error {
	for _, stmt := range splitStatements(schemaSQL) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("初始化表结构失败: %w\n语句: %.120s", err, stmt)
		}
	}
	return ensureColumns(ctx, db)
}

// ensureColumns 幂等列迁移：MySQL 不支持 ADD COLUMN IF NOT EXISTS，
// 通过 information_schema 检查后再 ALTER，保证老库平滑升级、可重复启动。
func ensureColumns(ctx context.Context, db *sql.DB) error {
	migrations := []struct {
		table string
		col   string
		ddl   string
	}{
		// 迭代二：SRS 间隔复习字段
		{"word_stats", "ease", "ALTER TABLE word_stats ADD COLUMN ease SMALLINT NOT NULL DEFAULT 250"},
		{"word_stats", "interval_days", "ALTER TABLE word_stats ADD COLUMN interval_days INT NOT NULL DEFAULT 0"},
		{"word_stats", "due_date", "ALTER TABLE word_stats ADD COLUMN due_date DATE NULL"},
		// 迭代三：经济系统（红心 / 宝石 / 连胜冻结 / 每日任务）
		{"profiles", "hearts", "ALTER TABLE profiles ADD COLUMN hearts TINYINT NOT NULL DEFAULT 5"},
		{"profiles", "hearts_at", "ALTER TABLE profiles ADD COLUMN hearts_at BIGINT NOT NULL DEFAULT 0"},
		{"profiles", "gems", "ALTER TABLE profiles ADD COLUMN gems INT NOT NULL DEFAULT 0"},
		{"profiles", "streak_freeze", "ALTER TABLE profiles ADD COLUMN streak_freeze TINYINT NOT NULL DEFAULT 0"},
		{"daily_stats", "review_words", "ALTER TABLE daily_stats ADD COLUMN review_words INT NOT NULL DEFAULT 0"},
		{"daily_stats", "chat_done", "ALTER TABLE daily_stats ADD COLUMN chat_done INT NOT NULL DEFAULT 0"},
		{"daily_stats", "task_claimed", "ALTER TABLE daily_stats ADD COLUMN task_claimed TINYINT NOT NULL DEFAULT 0"},
	}
	for _, m := range migrations {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.COLUMNS
			 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
			m.table, m.col).Scan(&n); err != nil {
			return fmt.Errorf("检查列 %s.%s 失败: %w", m.table, m.col, err)
		}
		if n > 0 {
			continue
		}
		if _, err := db.ExecContext(ctx, m.ddl); err != nil {
			return fmt.Errorf("迁移列 %s.%s 失败: %w", m.table, m.col, err)
		}
	}
	// 存量行回填：旧错题池整体转为「明天到期」
	if _, err := db.ExecContext(ctx,
		`UPDATE word_stats SET due_date = DATE(FROM_UNIXTIME(last_seen)) + INTERVAL 1 DAY
		 WHERE due_date IS NULL`); err != nil {
		return fmt.Errorf("回填 word_stats.due_date 失败: %w", err)
	}
	// SRS 查询索引
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.STATISTICS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'word_stats' AND INDEX_NAME = 'idx_due'`).Scan(&n); err != nil {
		return fmt.Errorf("检查索引 idx_due 失败: %w", err)
	}
	if n == 0 {
		if _, err := db.ExecContext(ctx,
			`ALTER TABLE word_stats ADD INDEX idx_due (user_id, due_date)`); err != nil {
			return fmt.Errorf("创建索引 idx_due 失败: %w", err)
		}
	}
	return nil
}

// splitStatements 去掉注释行后，按行尾分号把脚本切分成独立语句。
func splitStatements(script string) []string {
	var (
		out []string
		cur strings.Builder
	)
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
		if strings.HasSuffix(trimmed, ";") {
			stmt := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(cur.String()), ";"))
			if stmt != "" {
				out = append(out, stmt)
			}
			cur.Reset()
		}
	}
	if stmt := strings.TrimSpace(cur.String()); stmt != "" {
		out = append(out, stmt)
	}
	return out
}

// DB 暴露底层连接。
func (s *Store) DB() *sql.DB { return s.db }

// Close 关闭数据库连接池。
func (s *Store) Close() error { return s.db.Close() }
