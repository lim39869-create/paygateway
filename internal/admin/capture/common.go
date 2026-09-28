// Package capture 设备端收款码自动捕获（服务端 adb 驱动，纯自动化逻辑）。
//
// 与 admin 包解耦：本包只依赖 adbmgr（adb 操作），不依赖 store / 账户池 / HTTP，
// 便于独立测试与复用。HTTP 层（admin 包）负责设备查找、去重、入库、账户名解析。
//
// 背景：微信收付款页带 FLAG_SECURE（对无障碍隐藏节点树 + 屏蔽系统手势注入），
// 监听端 APP 内无法自动化微信（支付宝无此限制）。但服务器 adb shell 的
// input tap / screencap 不受 SECURE 限制（adb 是系统级通道）→ 微信捕获改由服务器驱动。
package capture

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"regexp"
	"strings"
	"time"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"

	"paygateway/internal/adbmgr"
)

// MaxQRUploadSize 二维码图片大小上限（2MB）
const MaxQRUploadSize = 2 << 20

// QrcodeKeepOnDevice 每设备相册保留的微信收款码图片张数（每账户只留最新 1 张，防存储打满）
const QrcodeKeepOnDevice = 1

// foregroundWait 坐标导航启动 app 后等待其到前台的上限。
// 30s 给慢速跨 user 冷启动留足余量（守卫仍会快速失败，只是不误杀偏慢的冷启）。
const foregroundWait = 30 * time.Second

// wakeHome 唤醒屏幕 + 回桌面，清掉前一个账户驻留的前台。
// 供各 Capture*Ledger/Qrcode 冷启动前调用：避免跨 user app 冷启被驻留前台挡住。
func wakeHome(ctx context.Context, adb *adbmgr.Manager, serial string) error {
	if err := adb.WakeUp(ctx, serial); err != nil {
		return err
	}
	return adb.GoHome(ctx, serial)
}

// DegenerateDump 判断 dump XML 是否为退化空树（无障碍未填充/页面未渲染）。
// 判据：整棵 hierarchy 无任何携带非空 text/content-desc 的节点。正常渲染的账本页
// （哪怕 0 交易行）总有标题/tab/页脚等静态文本节点；退化空树只有根 FrameLayout。
// 用于账本读取：退化空 dump 是'读取失败'而非'账户真实为空'，此时不得建立空基线，
// 否则后续真实历史行会被误判为 fresh 入账（2026-09-13 资金 P0 根因）。
var (
	degenerateTextRe = regexp.MustCompile(`text="([^"]*)"`)
	degenerateDescRe = regexp.MustCompile(`content-desc="([^"]*)"`)
)

func DegenerateDump(xmlText string) bool {
	hasContent := false
	for _, m := range degenerateTextRe.FindAllStringSubmatch(xmlText, -1) {
		if m[1] != "" {
			hasContent = true
			break
		}
	}
	if !hasContent {
		for _, m := range degenerateDescRe.FindAllStringSubmatch(xmlText, -1) {
			if m[1] != "" {
				hasContent = true
				break
			}
		}
	}
	return !hasContent
}

// DecodeQRContent 解析二维码图片内容（纯 Go，支持 PNG/JPEG/GIF）
// 返回解码后的字符串（如 https://qr.alipay.com/xxx 或 weixin://wxpay/bizpayurl?xxx）
func DecodeQRContent(data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("图片为空")
	}
	if len(data) > MaxQRUploadSize {
		return "", fmt.Errorf("图片超过 2MB 限制")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("图片解码失败（请上传清晰的收款码截图）: %w", err)
	}
	bmp, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return "", fmt.Errorf("二维码图像预处理失败: %w", err)
	}
	qrReader := qrcode.NewQRCodeReader()
	result, err := qrReader.Decode(bmp, nil)
	if err != nil {
		return "", fmt.Errorf("未识别到二维码（请确认图片包含清晰的收款码）: %w", err)
	}
	content := result.String()
	if content == "" {
		return "", fmt.Errorf("二维码内容为空")
	}
	return content, nil
}

// TruncateStr 截断字符串（日志安全：不打印完整码内容）
func TruncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// awaitForeground 带重试等待指定包名到前台。
// 坐标导航的前置守卫：目标 app 未到前台时绝不能开始盲点导航坐标——
// 重启失败/弹窗拦截/切前台慢时坐标会落进wrong app（实测点到微信/桌面/其他界面），
// 这正是捕获导航"乱来"的根源。双开空间前台串形如 "u999 com.xxx/..."，Contains 匹配。
func awaitForeground(ctx context.Context, adb *adbmgr.Manager, serial, pkg string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		fg, err := adb.ForegroundApp(ctx, serial)
		if err == nil {
			last = fg
			if strings.Contains(fg, pkg) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待 %s 前台超时 %v（当前前台 %q）", pkg, timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
