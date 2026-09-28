// Package notify 回调 URL 安全防护：
// SSRF 面在商户可控的 notify_url —— 下单时校验格式，发送时用安全拨号器
// 在 TCP 连接层面复核解析出的 IP（防 DNS rebinding 绕过域名黑名单）。
package notify

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ValidateNotifyURL 校验回调地址。
// scheme 必须为 http/https；allowPrivate=false 时拒绝解析到私网/环回/链路本地/未指定地址的目标。
func ValidateNotifyURL(raw string, allowPrivate bool) error {
	if raw == "" {
		return nil // 空地址合法（worker 跳过发送）
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("notify_url 无法解析: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("notify_url 协议必须是 http/https: %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("notify_url 缺少主机名")
	}
	if allowPrivate {
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("notify_url 域名解析失败: %w", err)
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return fmt.Errorf("notify_url 指向内网地址 %s（已拒绝）", ip)
		}
	}
	return nil
}

// IsPrivateURL 判断 URL 主机指向是否为内网/环回/链路本地等非公网地址
// （return_url 兜底重写用：return_url 是浏览器跳转，不涉及 SSRF 拨号）。
// DNS 解析失败按"非私有"处理：语法级检查已可靠拦截 localhost/环回/私网 IP 字面量，
// 纯域名解析失败（网关断网/无 resolver）不应把正常公网域名饿死出候选。
func IsPrivateURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return true
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return true
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		return isPrivateIP(ip)
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return true
	}
	ips, err := net.LookupIP(u.Hostname())
	if err != nil {
		return false // 解析不可达：语法检查已过，放行（见函数注释）
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return true
		}
	}
	return false
}

func isPrivateIP(ip net.IP) bool {
	privateRanges := []struct {
		network *net.IPNet
	}{
		{cidr("127.0.0.0/8")},    // IPv4 环回
		{cidr("10.0.0.0/8")},     // 私网
		{cidr("172.16.0.0/12")},  // 私网
		{cidr("192.168.0.0/16")}, // 私网
		{cidr("169.254.0.0/16")}, // 链路本地（云元数据 169.254.169.254 在此段）
		{cidr("0.0.0.0/8")},      // 未指定
		{cidr("100.64.0.0/10")},  // 运营商级 NAT（Tailscale 等）
		{cidr("::1/128")},        // IPv6 环回
		{cidr("fc00::/7")},       // IPv6 ULA
		{cidr("fe80::/10")},      // IPv6 链路本地
	}
	for _, r := range privateRanges {
		if r.network.Contains(ip) {
			return true
		}
	}
	return false
}

func cidr(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(fmt.Sprintf("内置 CIDR 非法 %s: %v", s, err))
	}
	return n
}

// NewNotifyHTTPClient 构造带 SSRF 复核的 HTTP 客户端：
// Control 复核发生在实际 connect 的地址上，域名解析结果与连接地址不可替换
// （DNS rebinding 无效）。allowPrivate=true 时行为与默认客户端一致。
func NewNotifyHTTPClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{}
	if !allowPrivate {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err == nil {
				var ips []net.IP
				if ip := net.ParseIP(host); ip != nil {
					ips = []net.IP{ip}
				} else if ips, err = net.LookupIP(host); err != nil {
					return nil, fmt.Errorf("notify 目标解析失败: %w", err)
				}
				for _, ip := range ips {
					if isPrivateIP(ip) {
						return nil, fmt.Errorf("notify 目标指向内网地址 %s（已拒绝）", ip)
					}
				}
			}
			return dialer.DialContext(ctx, network, addr)
		}
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}
