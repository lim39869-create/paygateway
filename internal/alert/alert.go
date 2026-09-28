// Package alert 告警系统（RESILIENCE.md P1-3）
//
// 支持 Bark（iOS 推送）与通用 Webhook（POST JSON），异步发送 + 防抖：
//   - 发送失败静默重试 2 次（不阻塞业务主链路）
//   - 同类型告警 1 分钟窗口内去重（防止刷屏）
//   - 配置：ALERT_BARK_URL / ALERT_WEBHOOK_URL（env），留空则该通道禁用
package alert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"paygateway/internal/logger"
)

// Client 告警客户端
type Client struct {
	barkURL    string // Bark 推送 URL（形如 https://api.day.app/<key>）
	webhookURL string // 通用 Webhook URL（POST JSON {title, body, level, fields}）
	client     *http.Client

	mu   sync.Mutex
	last map[string]time.Time // 防抖：title → 上次发送时间
}

// Options 告警配置
type Options struct {
	BarkURL    string
	WebhookURL string
}

// New 创建告警客户端（两者均空时返回 nil，调用方跳过）
func New(opts Options) *Client {
	if opts.BarkURL == "" && opts.WebhookURL == "" {
		return nil
	}
	return &Client{
		barkURL:    opts.BarkURL,
		webhookURL: opts.WebhookURL,
		client:     &http.Client{Timeout: 8 * time.Second},
		last:       make(map[string]time.Time),
	}
}

// Enabled 是否有任何告警通道
func (a *Client) Enabled() bool { return a != nil }

// dedup 防抖：同 title 窗口内去重
func (a *Client) dedup(title string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if t, ok := a.last[title]; ok && now.Sub(t) < time.Minute {
		return false
	}
	a.last[title] = now
	return true
}

// Send 发送告警（异步，不阻塞）
// level: info / warn / error；title: 告警类型（防抖 key）；body: 详情
func (a *Client) Send(level, title, body string) {
	if a == nil {
		return
	}
	if !a.dedup(title) {
		return // 1 分钟窗口内已发过同类告警
	}
	title = fmt.Sprintf("[%s] %s", level, title)
	logger.Warn("alert", "发送告警", "level", level, "title", title, "body", body)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("alert", "告警发送协程 panic（已恢复）", "title", title,
					"panic", r, "stack", string(debug.Stack()))
			}
		}()
		// 重试 3 次（1s/3s/9s 退避），全部失败仅记日志
		var lastErr error
		for i := 0; i < 3; i++ {
			if err := a.sendOnce(level, title, body); err == nil {
				return
			} else {
				lastErr = err
			}
			time.Sleep(time.Duration(1<<i) * time.Second)
		}
		logger.Error("alert", "告警发送失败", "err", lastErr, "title", title)
	}()
}

// sendOnce 单次发送到全部已配置通道
func (a *Client) sendOnce(level, title, body string) error {
	var firstErr error
	sent := false
	if a.barkURL != "" {
		if err := a.sendBark(title, body); err != nil {
			firstErr = err
		} else {
			sent = true
		}
	}
	if a.webhookURL != "" {
		if err := a.sendWebhook(level, title, body); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			sent = true
		}
	}
	if !sent {
		return fmt.Errorf("全部告警通道失败: %w", firstErr)
	}
	return nil
}

// sendBark Bark 推送（POST JSON）
func (a *Client) sendBark(title, body string) error {
	payload, _ := json.Marshal(map[string]string{
		"title": title,
		"body":  body,
	})
	req, err := http.NewRequest(http.MethodPost, a.barkURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("bark HTTP %d", resp.StatusCode)
	}
	return nil
}

// sendWebhook 通用 Webhook（POST JSON）
func (a *Client) sendWebhook(level, title, body string) error {
	payload, _ := json.Marshal(map[string]string{
		"level": level,
		"title": title,
		"body":  body,
		"time":  time.Now().Format(time.RFC3339),
	})
	req, err := http.NewRequest(http.MethodPost, a.webhookURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook HTTP %d", resp.StatusCode)
	}
	return nil
}

// Close 关闭空闲连接
func (a *Client) Close() {
	if a == nil {
		return
	}
	a.client.CloseIdleConnections()
}

var _ = context.Background
