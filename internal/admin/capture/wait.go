// 收款码捕获公共等待辅助：轮询相册直到出现新文件或超时。
//
// 背景：adb 走 WiFi/反代通道有网络延迟，手机冷启动/低端机有性能延迟，
// 点「保存图片」后文件落盘耗时波动大（实测 2s~15s 不等），固定 sleep 不可靠——
// 过早检查误判"未保存"，过久等待拖慢整个捕获流程。轮询在"尽快返回"和"不误判"间取得平衡。
package capture

import (
	"context"
	"time"
)

// pollForNewFile 轮询目录列表（listFn 过滤），直到数量超过 base 数量或超时。
// 每 2.5s 轮询一次，总超时 timeout；ctx 取消立即返回 false。
func pollForNewFile(ctx context.Context, base []string, timeout time.Duration, listFn func() []string) bool {
	deadline := time.Now().Add(timeout)
	for {
		if len(listFn()) > len(base) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2500 * time.Millisecond):
		}
	}
}
