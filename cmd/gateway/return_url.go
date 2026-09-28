// paygateway return_url 兜底重写：上游商户传来的回跳地址指向内网时，付款人支付成功后
// 跳回浏览器打不开的 localhost/内网页面。网关侧无法修改上游配置，但手里有可靠来源
// （Referer = 真实发起支付的前端地址、上游表的公网服务地址），按可信度逐层兜底。
package main

import (
	"net/url"
	"paygateway/internal/logger"
	"paygateway/internal/notify"
	"strings"

	"github.com/gin-gonic/gin"
)

// sanitizeReturnURL 返回修正后的 return_url：
//  1. 上游传的是公网地址（或空） → 原样返回
//  2. 内网/localhost → 用 Referer origin 重写（保留 path/query）
//  3. Referer 不可用 → 用上游后台配置的公网服务地址（BaseURL）重写
//  4. 无法判定 → 原样返回（附警示日志）
func (a *App) sanitizeReturnURL(c *gin.Context, orderNo, pid, returnURL string) string {
	if returnURL == "" || !notify.IsPrivateURL(returnURL) {
		return returnURL
	}
	fixed, source := a.publicOriginCandidate(c, orderNo, pid)
	if fixed == "" {
		return returnURL
	}
	out := rehost(returnURL, fixed)
	if out == "" {
		return returnURL
	}
	logger.Info("epay", "return_url 指向内网，已重写为付款人可达地址",
		"out_trade_no", orderNo, "source", source, "origin", fixed, "to", out)
	return out
}

// publicOriginCandidate 返回可用的公网 origin + 来源标注（"" 表示无可用候选）
func (a *App) publicOriginCandidate(c *gin.Context, orderNo, pid string) (string, string) {
	if ref := c.GetHeader("Referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil &&
			(u.Scheme == "https" || u.Scheme == "http") && u.Host != "" &&
			!notify.IsPrivateURL(u.Scheme+"://"+u.Host) {
			return u.Scheme + "://" + u.Host, "referer"
		}
	}
	if a.upstreamPool != nil {
		// pid 用 handleSubmit 已解析的参数（POST 表单场景 c.Query 取不到）
		if up := a.upstreamPool.ByPid(pid); up != nil && up.BaseURL != "" &&
			!notify.IsPrivateURL(up.BaseURL) {
			return strings.TrimRight(up.BaseURL, "/"), "upstream_base_url"
		}
	}
	logger.Warn("epay", "return_url 指向内网且无公网候选，将原样跳回（付款人可能打不开）",
		"out_trade_no", orderNo)
	return "", ""
}

// rehost 以 publicOrigin 的 host/origin 重写原始 URL，path/query/fragment 保留
func rehost(raw, origin string) string {
	src, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	dst, err := url.Parse(origin)
	if err != nil || dst.Host == "" {
		return ""
	}
	out := *src
	out.Scheme = dst.Scheme
	out.Host = dst.Host
	return out.String()
}
