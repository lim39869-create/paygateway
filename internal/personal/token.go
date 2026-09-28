package personal

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 二维码防劫持 token（docs/MIANQIAN.md 5.3）
//
// 支付页面不内嵌真实收款码，只引用网关签发的动态图片接口：
//
//	图片地址 = /qr/real?token={token}
//	浏览器请求 → 网关校验（token 签名 + 订单存在 + WAIT_PAY + 未过期）
//	→ 下发真实收款码 PNG（真实码内容不进支付页 HTML）
//
// token 格式（自包含，无需数据库索引）：
//
//	{随机16字节 base64url}.{orderID:expireUnix}.{HMAC-SHA256(orderID|expireUnix|随机, secret) 前 22 字节 base64url}
//
// 服务端校验：解码 → 重算 HMAC → 常量时间比较 → 检查过期 → 返回 orderID
// 攻击者无法伪造（无 secret），篡改任一字段 HMAC 失配即拒绝。

// QrTokenSecret 套码 token 签名密钥（服务端持有，内存）
var qrTokenSecret []byte

// InitQrToken 初始化套码 token 签名密钥（hex 32 字节）
// 未调用 InitQrToken 时自动生成并持久化由 main 负责；这里校验长度
func InitQrToken(secret string) error {
	key, err := hex.DecodeString(secret)
	if err != nil {
		return fmt.Errorf("套码 token 密钥格式错误: %w", err)
	}
	if len(key) != 32 {
		return fmt.Errorf("套码 token 密钥长度错误: 需要 32 字节，实际 %d", len(key))
	}
	qrTokenSecret = key
	return nil
}

// QrTokenSecretSet 是否已初始化
func QrTokenSecretSet() bool { return len(qrTokenSecret) == 32 }

// GenerateQrToken 生成套码 token
// key: 订单号（out_trade_no）或订单 ID 字符串
func GenerateQrToken(key string, expireAt time.Time) (string, error) {
	if !QrTokenSecretSet() {
		return "", fmt.Errorf("套码 token 密钥未初始化")
	}
	// 随机 16 字节
	rnd := make([]byte, 16)
	if _, err := rand.Read(rnd); err != nil {
		return "", fmt.Errorf("生成 token 随机数失败: %w", err)
	}
	// payload: key|expireUnix
	payload := fmt.Sprintf("%s|%d", key, expireAt.Unix())
	// HMAC over payload|random
	mac := hmac.New(sha256.New, qrTokenSecret)
	mac.Write([]byte(payload))
	mac.Write([]byte("|"))
	mac.Write(rnd)
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:22])

	token := base64.RawURLEncoding.EncodeToString(rnd) + "." +
		base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + sig
	return token, nil
}

// ParseQrToken 校验并解析 token，返回 (key, expireAt)
// 校验失败返回 error（篡改/伪造/过期）
func ParseQrToken(token string) (string, time.Time, error) {
	if !QrTokenSecretSet() {
		return "", time.Time{}, fmt.Errorf("套码 token 密钥未初始化")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", time.Time{}, fmt.Errorf("token 格式错误")
	}
	rnd, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(rnd) != 16 {
		return "", time.Time{}, fmt.Errorf("token 随机数非法")
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token payload 非法")
	}
	payload := string(payloadBytes)
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) == 0 {
		return "", time.Time{}, fmt.Errorf("token 签名非法")
	}

	// 重算 HMAC 并常量时间比较
	mac := hmac.New(sha256.New, qrTokenSecret)
	mac.Write([]byte(payload))
	mac.Write([]byte("|"))
	mac.Write(rnd)
	expect := mac.Sum(nil)[:22]
	if !hmac.Equal(expect, sig) {
		return "", time.Time{}, fmt.Errorf("token 签名不匹配（篡改或伪造）")
	}

	// 解析 payload: key|expireUnix
	kv := strings.Split(payload, "|")
	if len(kv) != 2 {
		return "", time.Time{}, fmt.Errorf("token payload 格式错误")
	}
	key := kv[0]
	if key == "" {
		return "", time.Time{}, fmt.Errorf("token key 非法")
	}
	expireUnix, err := strconv.ParseInt(kv[1], 10, 64)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("token expire 非法")
	}
	expireAt := time.Unix(expireUnix, 0)
	if !expireAt.After(time.Now()) {
		return "", time.Time{}, fmt.Errorf("token 已过期")
	}
	return key, expireAt, nil
}
