package personal

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// base64URL 辅助：普通 base64 编码
func base64URL(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func TestGenerateQrTokenRoundTrip(t *testing.T) {
	if err := InitQrToken(strings.Repeat("ab", 32)); err != nil {
		t.Fatalf("InitQrToken: %v", err)
	}
	token, err := GenerateQrToken("ORDER-123", time.Now().Add(30*time.Minute))
	if err != nil {
		t.Fatalf("GenerateQrToken: %v", err)
	}
	key, expire, err := ParseQrToken(token)
	if err != nil {
		t.Fatalf("ParseQrToken: %v", err)
	}
	if key != "ORDER-123" {
		t.Fatalf("key mismatch: got %q want ORDER-123", key)
	}
	if !expire.After(time.Now()) {
		t.Fatalf("expire should be in future")
	}
}

func TestGenerateQrTokenTampered(t *testing.T) {
	if err := InitQrToken(strings.Repeat("ab", 32)); err != nil {
		t.Fatalf("InitQrToken: %v", err)
	}
	token, err := GenerateQrToken("ORDER-123", time.Now().Add(30*time.Minute))
	if err != nil {
		t.Fatalf("GenerateQrToken: %v", err)
	}
	// 篡改 payload（改成另一个订单号）
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token format wrong")
	}
	// 篡改 payload 部分为另一个订单号（保持时间戳格式，签名必然失配）
	evilPayload := base64URL("ORDER-EVIL|9999999999")
	evil := parts[0] + "." + evilPayload + "." + parts[2]
	if _, _, err := ParseQrToken(evil); err == nil {
		t.Fatalf("tampered token should fail")
	}
}

func TestParseQrTokenExpired(t *testing.T) {
	if err := InitQrToken(strings.Repeat("ab", 32)); err != nil {
		t.Fatalf("InitQrToken: %v", err)
	}
	token, err := GenerateQrToken("ORDER-1", time.Now().Add(-time.Minute)) // 已过期
	if err != nil {
		t.Fatalf("GenerateQrToken: %v", err)
	}
	if _, _, err := ParseQrToken(token); err == nil {
		t.Fatalf("expired token should fail")
	}
}

func TestParseQrTokenGarbage(t *testing.T) {
	if err := InitQrToken(strings.Repeat("ab", 32)); err != nil {
		t.Fatalf("InitQrToken: %v", err)
	}
	if _, _, err := ParseQrToken("garbage"); err == nil {
		t.Fatalf("garbage token should fail")
	}
	if _, _, err := ParseQrToken(""); err == nil {
		t.Fatalf("empty token should fail")
	}
}

func TestGenerateQrTokenUnique(t *testing.T) {
	if err := InitQrToken(strings.Repeat("ab", 32)); err != nil {
		t.Fatalf("InitQrToken: %v", err)
	}
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		token, err := GenerateQrToken("ORDER-1", time.Now().Add(time.Minute))
		if err != nil {
			t.Fatalf("GenerateQrToken: %v", err)
		}
		if seen[token] {
			t.Fatalf("duplicate token")
		}
		seen[token] = true
	}
}
