// Package release 负责安卓客户端的版本查询与发版渠道对接。
//
// 设计取舍：
//   - 版本与下载地址来自蒲公英（国内 CDN，直连稳定），不自己托管 APK；
//   - 「是否强制更新」由服务端决定（min_supported_version_code），
//     这样下线老版本不用重新发版；
//   - API Key 只存在服务端环境变量里，不下发到前端包——
//     打进客户端的密钥等同于公开，谁都能刷你的额度。
package release

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PgyerAPIBase 是蒲公英开放 API 版本前缀。
const PgyerAPIBase = "https://www.pgyer.com/apiv2"

// Info 是客户端检查更新拿到的结果。
type Info struct {
	// Version 展示版本号（如 1.0.3）
	Version string `json:"version"`
	// VersionCode 内部版本号，用于比较大小
	VersionCode int `json:"version_code"`
	// HasUpdate 是否比客户端当前版本新
	HasUpdate bool `json:"has_update"`
	// Force 是否强制更新（服务端配置的下限高于客户端版本）
	Force bool `json:"force"`
	// DownloadURL 直装 APK 地址
	DownloadURL string `json:"download_url"`
	// PageURL 蒲公英安装页（浏览器/二维码用）
	PageURL string `json:"page_url"`
	// Description 发版说明
	Description string `json:"description"`
	// FileSize 字节数，0 表示未知
	FileSize int64 `json:"file_size"`
}

// Client 是蒲公英开放 API 的最小实现（只用到 check 一个接口）。
type Client struct {
	http   *http.Client
	apiKey string
	appKey string

	// 缓存：蒲公英免费额度有限，同一分钟内没必要每次都回源
	mu      sync.Mutex
	cached  *Info
	expires time.Time
}

// NewClient 构造客户端；apiKey 或 appKey 为空时 Enabled 为 false。
func NewClient(apiKey, appKey string) *Client {
	return &Client{
		http:   &http.Client{Timeout: 10 * time.Second},
		apiKey: strings.TrimSpace(apiKey),
		appKey: strings.TrimSpace(appKey),
	}
}

// Enabled 报告凭据是否配齐。
func (c *Client) Enabled() bool {
	return c != nil && c.apiKey != "" && c.appKey != ""
}

// cacheTTL 是最新版本的缓存时长。发版不频繁，1 分钟足够把瞬时并发/重复点击
// 挡在蒲公英之外，又不至于让刚上传的新版本等太久。
const cacheTTL = time.Minute

// Check 查询线上最新版本。currentCode 是客户端上报的 versionCode。
func (c *Client) Check(ctx context.Context, currentCode int) (*Info, error) {
	latest, err := c.latest(ctx)
	if err != nil {
		return nil, err
	}
	// 复制一份再改写：缓存里的对象不能带调用方的比较结果
	out := *latest
	out.HasUpdate = latest.VersionCode > currentCode
	out.Force = false
	return &out, nil
}

// latest 取蒲公英最新版本，带一分钟缓存。
func (c *Client) latest(ctx context.Context) (*Info, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("未配置蒲公英 App Key / API Key")
	}

	c.mu.Lock()
	if c.cached != nil && time.Now().Before(c.expires) {
		hit := *c.cached
		c.mu.Unlock()
		return &hit, nil
	}
	c.mu.Unlock()

	info, err := c.fetch(ctx)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.cached = info
	c.expires = time.Now().Add(cacheTTL)
	c.mu.Unlock()

	hit := *info
	return &hit, nil
}

// fetch 调用 /app/check 取最新版本。
//
// buildVersion 传 "0.0.0"：让蒲公英总是返回它那边的最新版本，
// 是否「有新版本」由我们按 versionCode 自己判断——
// 服务端要用这个结果去决定强制更新，交给蒲公英比对就判断不了了。
func (c *Client) fetch(ctx context.Context) (*Info, error) {
	form := url.Values{}
	form.Set("_api_key", c.apiKey)
	form.Set("appKey", c.appKey)
	form.Set("buildVersion", "0.0.0")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		PgyerAPIBase+"/app/check", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	// 蒲公英只解析 form-urlencoded，发 JSON 会报 1001「_api_key could not be empty」
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求蒲公英失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("蒲公英返回 %s: %s", resp.Status, truncate(string(body), 200))
	}

	var raw struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			BuildVersion       string `json:"buildVersion"`
			BuildVersionNo     string `json:"buildVersionNo"`
			BuildUpdateDesc    string `json:"buildUpdateDescription"`
			DownloadURL        string `json:"downloadURL"`
			BuildShortcutURL   string `json:"buildShortcutUrl"`
			BuildFileSize      string `json:"buildFileSize"`
			BuildHaveNewVer    bool   `json:"buildHaveNewVersion"`
			BuildNeedForceUpd  bool   `json:"needForceUpdate"`
			BuildBuildVersionN string `json:"-"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析蒲公英响应失败: %w", err)
	}
	if raw.Code != 0 {
		return nil, fmt.Errorf("蒲公英错误 code=%d %s", raw.Code, raw.Message)
	}

	// buildVersionNo 是内部构建号（蒲公英里是递增整数）。少数情况下为空，
	// 此时退回解析 buildVersion 的末尾数字，避免 versionCode 恒为 0 导致
	// 永远判定「有新版本」。
	code, _ := strconv.Atoi(strings.TrimSpace(raw.Data.BuildVersionNo))
	if code <= 0 {
		code = parseVersionCode(raw.Data.BuildVersion)
	}

	size, _ := strconv.ParseInt(strings.TrimSpace(raw.Data.BuildFileSize), 10, 64)

	return &Info{
		Version:     raw.Data.BuildVersion,
		VersionCode: code,
		DownloadURL: raw.Data.DownloadURL,
		PageURL:     shortcutPage(raw.Data.BuildShortcutURL),
		Description: raw.Data.BuildUpdateDesc,
		FileSize:    size,
	}, nil
}

// shortcutPage 把蒲公英返回的短链补成完整安装页地址。
// API 给的有时是纯 short id，有时已是完整 URL，两种都要兜住。
func shortcutPage(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s
	}
	return "https://www.pgyer.com/" + strings.TrimPrefix(s, "/")
}

// parseVersionCode 从 "1.2.3" 里取可用于比较的整数（1*10000+2*100+3）。
// 仅作蒲公英 buildVersionNo 缺失时的兜底。
//
// 必须固定按三段计算：缺失段补 0。若按实际段数逐段累乘，
// "1.0"（100）会小于 "1.0.0"（10000），同一版本比出两种结果。
func parseVersionCode(v string) int {
	parts := strings.Split(strings.TrimSpace(v), ".")
	code := 0
	for i := 0; i < 3; i++ {
		n := 0
		if i < len(parts) {
			if v, err := strconv.Atoi(strings.TrimSpace(parts[i])); err == nil {
				n = v
			} else {
				// 带字母前缀/后缀（v1.0.3 / 1.0.3-beta）时取其中的连续数字
				n = leadingInt(parts[i])
			}
		}
		code = code*100 + n
	}
	return code
}

// leadingInt 取字符串里第一段连续数字，取不到返回 0。
// 先扫非数字前缀（v1 → 1），让 "v1.0.3" 这类写法也能解析。
func leadingInt(s string) int {
	s = strings.TrimSpace(s)
	start := 0
	for start < len(s) && (s[start] < '0' || s[start] > '9') {
		start++
	}
	end := start
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == start {
		return 0
	}
	n, _ := strconv.Atoi(s[start:end])
	return n
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
