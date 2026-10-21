package release

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestParseVersionCode(t *testing.T) {
	cases := map[string]int{
		"1.2.3":      10203,
		"1.0":        10000, // 缺失段补 0，必须与 "1.0.0" 等价
		"2.0.0":      20000,
		"1.2.3-beta": 10203,
		"v1.2.3":     10203, // 字母前缀取第一段数字
		"":           0,
		"abc":        0,
		"10.20.30":   102030,
	}
	for in, want := range cases {
		if got := parseVersionCode(in); got != want {
			t.Errorf("parseVersionCode(%q) = %d, want %d", in, got, want)
		}
	}

	// 关键不变量：同一版本的不同写法必须解析成同一值，
	// 否则客户端会误判「有新版本」
	if parseVersionCode("1.0") != parseVersionCode("1.0.0") {
		t.Error(`"1.0" 与 "1.0.0" 必须等价`)
	}
	if parseVersionCode("1.2") != parseVersionCode("1.2.0") {
		t.Error(`"1.2" 与 "1.2.0" 必须等价`)
	}
}

func TestShortcutPage(t *testing.T) {
	cases := map[string]string{
		"":                                    "",
		"wordrush-demo":                       "https://www.pgyer.com/wordrush-demo",
		"/wordrush-demo":                      "https://www.pgyer.com/wordrush-demo",
		"https://www.pgyer.com/wordrush-demo": "https://www.pgyer.com/wordrush-demo",
		"http://www.pgyer.com/x":              "http://www.pgyer.com/x",
	}
	for in, want := range cases {
		if got := shortcutPage(in); got != want {
			t.Errorf("shortcutPage(%q) = %q, want %q", in, got, want)
		}
	}
}

// newTestClient 把请求指向假服务器，绕开真实蒲公英。
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewClient("test-key", "test-app")
	// 直接替换 transport，把任何请求重定向到测试服务器
	c.http.Transport = rewriteTransport{srv.URL}
	return c
}

type rewriteTransport struct{ base string }

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u := rt.base + req.URL.Path
	parsed, err := http.NewRequestWithContext(req.Context(), req.Method, u, req.Body)
	if err != nil {
		return nil, err
	}
	parsed.Header = req.Header.Clone()
	return http.DefaultTransport.RoundTrip(parsed)
}

func TestCheckComparesVersionCode(t *testing.T) {
	body := func(version, versionNo, downloadURL string) string {
		m := map[string]any{
			"code":    0,
			"message": "",
			"data": map[string]any{
				"buildVersion":           version,
				"buildVersionNo":         versionNo,
				"buildUpdateDescription": "修复若干问题\n优化跟读打分",
				"downloadURL":            downloadURL,
				"buildShortcutUrl":       "wordrush-demo",
				"buildFileSize":          "5242880",
			},
		}
		b, _ := json.Marshal(m)
		return string(b)
	}

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// 蒲公英只认 form-urlencoded，JSON 会读不到字段
		if err := r.ParseForm(); err != nil {
			t.Errorf("解析表单失败: %v", err)
		}
		if r.Form.Get("_api_key") == "" {
			t.Errorf("缺少 _api_key")
		}
		if r.Form.Get("appKey") != "test-app" {
			t.Errorf("appKey = %q", r.Form.Get("appKey"))
		}
		// 固定传 0.0.0，让蒲公英总返回最新版
		if r.Form.Get("buildVersion") != "0.0.0" {
			t.Errorf("buildVersion = %q, want 0.0.0", r.Form.Get("buildVersion"))
		}
		_, _ = w.Write([]byte(body("1.2.0", "42", "https://example.com/a.apk")))
	})

	// 旧版本 → 有新版本
	info, err := c.Check(context.Background(), 41)
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	if !info.HasUpdate {
		t.Error("41 < 42 应判定有新版本")
	}
	if info.VersionCode != 42 || info.Version != "1.2.0" {
		t.Errorf("版本解析错误: %+v", info)
	}
	if info.FileSize != 5242880 {
		t.Errorf("FileSize = %d", info.FileSize)
	}
	if info.PageURL != "https://www.pgyer.com/wordrush-demo" {
		t.Errorf("PageURL = %q", info.PageURL)
	}
	if strings.Contains(info.Description, "\n") == false {
		t.Error("多行发版说明应原样保留")
	}
	if info.Force {
		t.Error("Force 由 API 层按配置决定，release 包内应恒为 false")
	}

	// 已是最新 → 无更新
	info2, err := c.Check(context.Background(), 42)
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	if info2.HasUpdate {
		t.Error("版本相同不应判定有更新")
	}

	// 本地比线上还新（回滚/内部测试包）→ 无更新
	info3, _ := c.Check(context.Background(), 99)
	if info3.HasUpdate {
		t.Error("本地版本更高时不应提示更新")
	}
}

func TestCheckFallsBackWhenVersionNoMissing(t *testing.T) {
	// 蒲公英极少数情况下 buildVersionNo 为空，此时应退回解析 versionName，
	// 否则 versionCode 恒为 0，客户端永远「有新版本」。
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"buildVersion":"1.5.2","buildVersionNo":""}}`))
	})
	info, err := c.Check(context.Background(), 100)
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	if info.VersionCode != 10502 {
		t.Errorf("VersionCode = %d, want 10502（由 1.5.2 推导）", info.VersionCode)
	}
	if !info.HasUpdate {
		t.Error("10502 > 100 的本地版本，应提示更新")
	}
}

func TestCheckSurfacesPgyerError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":1001,"message":"_api_key could not be empty"}`))
	})
	_, err := c.Check(context.Background(), 1)
	if err == nil {
		t.Fatal("应返回错误")
	}
	if !strings.Contains(err.Error(), "1001") {
		t.Errorf("错误信息应含错误码: %v", err)
	}
}

func TestCacheAvoidsRepeatedUpstreamCalls(t *testing.T) {
	var calls int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"code":0,"data":{"buildVersion":"1.0.0","buildVersionNo":"` +
			strconv.Itoa(calls) + `"}}`))
	})

	for i := 0; i < 5; i++ {
		if _, err := c.Check(context.Background(), 0); err != nil {
			t.Fatalf("Check 失败: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("一分钟内应只回源一次，实际 %d 次", calls)
	}
}

func TestEnabled(t *testing.T) {
	if NewClient("", "app").Enabled() {
		t.Error("缺 apiKey 应为未启用")
	}
	if NewClient("key", "").Enabled() {
		t.Error("缺 appKey 应为未启用")
	}
	if !NewClient("key", "app").Enabled() {
		t.Error("两者齐全应为已启用")
	}
	var nilClient *Client
	if nilClient.Enabled() {
		t.Error("nil 客户端不应 panic 且应为未启用")
	}
}
