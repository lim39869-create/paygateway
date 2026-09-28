package personal

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// 监听端通信签名（docs/MIANQIAN.md 8.1）
//
//	HTTPS 强制（生产）
//	sign = HMAC-SHA256(secret, method|path|timestamp|field1|field2|...)
//	timestamp 防重放：±5 分钟窗口
//	device_id：客户端唯一标识（首次启动生成 UUID）
//
// 字段化签名设计（关键）：
//   - 签名基于"字段值按固定顺序拼接"，而非对整个 JSON body 字节签名
//   - 原因：JSON 序列化字段顺序不可控（Go map 随机序），若对 body 字节签名，
//     客户端和服务端 JSON 字段顺序不一致会导致验签失败（且无法把 sign 放进 body 同时签名）
//   - 客户端：构造业务字段 → 计算签名 → 把 sign 加入 JSON 发送
//   - 服务端：解析 JSON 取出字段 → 用相同顺序计算期望签名 → 常量时间比较

// SignWindow 签名时间戳窗口（±5 分钟）
const SignWindow = 5 * time.Minute

// SignMsg 计算监听端请求签名
// fields 按固定顺序传入（客户端/服务端必须一致）：
//   - 心跳:   device_id
//   - 推送:   device_id, provider, amount, note, raw
func SignMsg(secret, method, path string, timestamp int64, fields ...string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s|%s|%d|", method, path, timestamp)
	mac.Write([]byte(strings.Join(fields, "|")))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySign 校验监听端请求签名
// 校验：签名匹配 + 时间戳在 ±SignWindow 窗口内（防重放）
func VerifySign(secret, method, path string, timestamp int64, sign string, fields ...string) error {
	if sign == "" {
		return fmt.Errorf("缺少签名")
	}
	now := time.Now().Unix()
	diff := now - timestamp
	if diff < 0 {
		diff = -diff
	}
	if diff > int64(SignWindow.Seconds()) {
		return fmt.Errorf("时间戳超出窗口（防重放）: diff=%ds", diff)
	}
	expect := SignMsg(secret, method, path, timestamp, fields...)
	if !hmac.Equal([]byte(expect), []byte(sign)) {
		return fmt.Errorf("签名不匹配")
	}
	return nil
}

// NewDeviceSecret 生成设备通信密钥（32 字节 hex）
func NewDeviceSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成设备密钥失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// NewDeviceID 生成设备唯一标识（UUID v4 简化版，32 位 hex）
func NewDeviceID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// 兜底：rand 失败时用时间戳（几乎不可能）
		return fmt.Sprintf("dev-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
