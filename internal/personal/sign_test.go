package personal

import (
	"testing"
	"time"
)

func TestVerifySignValid(t *testing.T) {
	secret := "test-secret-key"
	method := "POST"
	path := "/personal/push"
	ts := time.Now().Unix()

	sign := SignMsg(secret, method, path, ts, "d1", "alipay", "10.00", "ab3k", "支付宝到账10.00元", "0")
	if err := VerifySign(secret, method, path, ts, sign, "d1", "alipay", "10.00", "ab3k", "支付宝到账10.00元", "0"); err != nil {
		t.Fatalf("valid sign should pass: %v", err)
	}
}

func TestVerifySignWrongSecret(t *testing.T) {
	ts := time.Now().Unix()
	sign := SignMsg("secret-a", "POST", "/personal/push", ts, "d1")
	if err := VerifySign("secret-b", "POST", "/personal/push", ts, sign, "d1"); err == nil {
		t.Fatalf("wrong secret should fail")
	}
}

func TestVerifySignTamperedField(t *testing.T) {
	secret := "s"
	ts := time.Now().Unix()
	sign := SignMsg(secret, "POST", "/personal/push", ts, "d1", "10.00")
	// 篡改金额字段（签名不变）→ 验签失败
	if err := VerifySign(secret, "POST", "/personal/push", ts, sign, "d1", "100.00"); err == nil {
		t.Fatalf("tampered field should fail")
	}
}

func TestVerifySignReplayWindow(t *testing.T) {
	secret := "s"
	// 时间戳超出窗口（10 分钟前）
	oldTS := time.Now().Add(-10 * time.Minute).Unix()
	sign := SignMsg(secret, "POST", "/personal/push", oldTS, "d1")
	if err := VerifySign(secret, "POST", "/personal/push", oldTS, sign, "d1"); err == nil {
		t.Fatalf("out-of-window timestamp should fail (replay protection)")
	}
	// 窗口内（1 分钟前）应通过
	recentTS := time.Now().Add(-1 * time.Minute).Unix()
	sign2 := SignMsg(secret, "POST", "/personal/push", recentTS, "d1")
	if err := VerifySign(secret, "POST", "/personal/push", recentTS, sign2, "d1"); err != nil {
		t.Fatalf("in-window timestamp should pass: %v", err)
	}
}

func TestVerifySignEmpty(t *testing.T) {
	if err := VerifySign("s", "POST", "/personal/push", time.Now().Unix(), "", "d1"); err == nil {
		t.Fatalf("empty sign should fail")
	}
}

// TestVerifySignRiskText 封号监控链路（docs/MIANQIAN.md §10）：
// 监听端检测到风控文案 → 上报 risk_text（amount/note 为空串），
// risk_text 必须参与签名（客户端 SignUtil.signPush 字段序含 risk_text），
// 且空串也参与拼接 —— 模拟 APP 真实上报的字段序，验证服务端验签通过
func TestVerifySignRiskText(t *testing.T) {
	secret := "test-secret-key"
	method := "POST"
	path := "/personal/push"
	ts := time.Now().Unix()
	deviceID := "dev-1234"
	provider := "alipay"

	// 模拟 APP 风控上报：amount="" note=""，raw 与 risk_text 均带原文
	// 注意：Kotlin SignUtil.signPush 字段序 = [device_id, provider, amount, note, raw, risk_text, space]
	raw := "您的账户存在风控提示，请及时处理"
	amount, note := "", ""
	riskText := "您的账户存在风控提示，请及时处理"
	sign := SignMsg(secret, method, path, ts, deviceID, provider, amount, note, raw, riskText, "0")
	if err := VerifySign(secret, method, path, ts, sign, deviceID, provider, amount, note, raw, riskText, "0"); err != nil {
		t.Fatalf("risk-text sign should pass (fields order must match client): %v", err)
	}
	// 篡改 risk_text → 验签失败
	if err := VerifySign(secret, method, path, ts, sign, deviceID, provider, amount, note, raw, "伪造的风控文案", "0"); err == nil {
		t.Fatalf("tampered risk_text should fail")
	}
}

// TestVerifySignRiskTextEmpty 普通收款上报（risk_text 为空串也必须参与拼接）
func TestVerifySignRiskTextEmpty(t *testing.T) {
	secret := "s"
	ts := time.Now().Unix()
	// 客户端普通推送：risk_text 默认为 ""，但 SignUtil 仍拼入字段序
	sign := SignMsg(secret, "POST", "/personal/push", ts, "d1", "alipay", "10.00", "ab3k", "支付宝到账10.00元", "", "0")
	if err := VerifySign(secret, "POST", "/personal/push", ts, sign, "d1", "alipay", "10.00", "ab3k", "支付宝到账10.00元", "", "0"); err != nil {
		t.Fatalf("empty risk_text must still be signed (client sends it): %v", err)
	}
}

func TestNewDeviceSecret(t *testing.T) {
	s1, err := NewDeviceSecret()
	if err != nil {
		t.Fatalf("NewDeviceSecret: %v", err)
	}
	s2, err := NewDeviceSecret()
	if err != nil {
		t.Fatalf("NewDeviceSecret: %v", err)
	}
	if len(s1) != 64 { // 32 字节 hex
		t.Fatalf("secret length: got %d want 64", len(s1))
	}
	if s1 == s2 {
		t.Fatalf("secrets should differ")
	}
}
