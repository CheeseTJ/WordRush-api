-- wordrush 学习记录表（OCI HeatWave / MySQL）。
--
-- 词库本身打包在前端（离线优先的 PWA），库里只存用户产生的数据：
-- 影子用户、关卡进度、单词正误统计、每日统计与总档案。

CREATE TABLE IF NOT EXISTS users (
    id          VARCHAR(64)  NOT NULL PRIMARY KEY,
    email       VARCHAR(255) NOT NULL,
    role        VARCHAR(32)  NOT NULL DEFAULT 'user',
    created_at  BIGINT       NOT NULL,
    last_seen_at BIGINT      NOT NULL
);

CREATE TABLE IF NOT EXISTS profiles (
    user_id       VARCHAR(64) NOT NULL PRIMARY KEY,
    xp            INT         NOT NULL DEFAULT 0,
    streak_days   INT         NOT NULL DEFAULT 0,
    last_day      DATE        NULL,
    hearts        TINYINT     NOT NULL DEFAULT 5,
    hearts_at     BIGINT      NOT NULL DEFAULT 0,
    gems          INT         NOT NULL DEFAULT 0,
    streak_freeze TINYINT     NOT NULL DEFAULT 0,
    updated_at    BIGINT      NOT NULL
);

CREATE TABLE IF NOT EXISTS unit_progress (
    user_id    VARCHAR(64) NOT NULL,
    unit_id    VARCHAR(64) NOT NULL,
    stars      TINYINT     NOT NULL DEFAULT 0,
    best_combo INT         NOT NULL DEFAULT 0,
    correct    INT         NOT NULL DEFAULT 0,
    total      INT         NOT NULL DEFAULT 0,
    updated_at BIGINT      NOT NULL,
    PRIMARY KEY (user_id, unit_id)
);

CREATE TABLE IF NOT EXISTS word_stats (
    user_id   VARCHAR(64) NOT NULL,
    word_id   VARCHAR(64) NOT NULL,
    correct   INT         NOT NULL DEFAULT 0,
    wrong     INT         NOT NULL DEFAULT 0,
    last_seen BIGINT      NOT NULL,
    PRIMARY KEY (user_id, word_id)
);

CREATE TABLE IF NOT EXISTS daily_stats (
    user_id      VARCHAR(64) NOT NULL,
    day          DATE        NOT NULL,
    words        INT         NOT NULL DEFAULT 0,
    correct      INT         NOT NULL DEFAULT 0,
    wrong        INT         NOT NULL DEFAULT 0,
    xp           INT         NOT NULL DEFAULT 0,
    review_words INT         NOT NULL DEFAULT 0,
    task_claimed TINYINT     NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, day)
);
