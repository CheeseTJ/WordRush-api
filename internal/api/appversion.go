package api

import (
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// getAppVersion 返回安卓客户端的最新版本信息，供「我的」页「检查更新」使用。
//
// 公开接口（不走 Ucenter JWT）：未登录用户也应该能升级，否则老版本用户
// 可能卡在登录失败的循环里出不来。
//
// 参数：?version_code=12&platform=android
//   - version_code 客户端当前 versionCode，缺省 0（即「总是当作有新版本」）
//   - platform 目前只支持 android，留作 iOS 扩展位
//
// 降级策略：蒲公英不可用时不返回 5xx（否则前端只能弹错误），
// 而是 returned 200 + available=false + 兜底下载页，把选择权交给用户。
func getAppVersion(c *gin.Context, d Deps) {
	currentCode := 0
	if v := strings.TrimSpace(c.Query("version_code")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			currentCode = n
		}
	}

	platform := strings.ToLower(strings.TrimSpace(c.DefaultQuery("platform", "android")))

	body := gin.H{
		"available":     false,
		"platform":      platform,
		"download_page": d.Cfg.App.DownloadPage,
	}

	// 未配置蒲公英凭据：只给下载页入口，不算错误
	if !d.Release.Enabled() {
		c.JSON(http.StatusOK, body)
		return
	}

	info, err := d.Release.Check(c.Request.Context(), currentCode)
	if err != nil {
		// 上游故障不该让客户端「检查更新」变成报错弹窗
		log.Printf("查询蒲公英版本失败: %v", err)
		c.JSON(http.StatusOK, body)
		return
	}

	// 服务端配置的最低可用版本高于客户端 → 强制更新。
	// 放在这里而不是让前端判断，是为了让「下线老版本」只需改配置 + 重启，
	// 不必等所有人在线发版。
	force := false
	if min := d.Cfg.App.MinSupportedVersionCode; min > 0 && currentCode < min {
		force = true
		// 强制更新时必须有可下载的地址，否则用户会被卡死在不可取消的弹窗上
		if info.DownloadURL == "" {
			log.Printf("强制更新但蒲公英未返回下载地址（客户端 versionCode=%d, 下限=%d）", currentCode, min)
			force = false
		}
	}

	body["available"] = true
	body["version"] = info.Version
	body["version_code"] = info.VersionCode
	body["has_update"] = info.HasUpdate
	body["force"] = force
	body["download_url"] = info.DownloadURL
	body["page_url"] = info.PageURL
	body["description"] = info.Description
	body["file_size"] = info.FileSize
	c.JSON(http.StatusOK, body)
}
