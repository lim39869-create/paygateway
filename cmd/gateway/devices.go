// paygateway 支付网关 devices（从 main.go 拆分，设备/渠道映射）
package main

import (
	"fmt"
	"paygateway/internal/personal"
)

func deviceSerialFromDevice(d *personal.Device) string {
	if d == nil {
		return ""
	}
	if d.AdbHost != "" {
		if d.AdbPort > 0 {
			return fmt.Sprintf("%s:%d", d.AdbHost, d.AdbPort)
		}
		return d.AdbHost
	}
	if d.AdbWifiHost != "" && d.AdbWifiPort > 0 {
		return fmt.Sprintf("%s:%d", d.AdbWifiHost, d.AdbWifiPort)
	}
	return ""
}

// ledgerProviderOf 账户通道 → 账本 provider（仅个人免签通道有账本页可监听）
func ledgerProviderOf(channel string) (string, bool) {
	switch channel {
	case "personal.alipay":
		return "alipay", true
	case "personal.wechat":
		return "wechat", true
	}
	return "", false
}

// ledgerUserSpaceOf 账户所属用户空间：收款码内容匹配该设备 (平台, 用户空间) 最新码。
// 主空间(user 0)与双开(user 999)的码内容不同，用 QRContent 精确匹配归属；
// 匹配不到（无码/未上报）默认主空间 0。
