package admin

import (
	"strings"
	"testing"

	qrcode "github.com/skip2/go-qrcode"

	"paygateway/internal/admin/capture"
)

// TestDecodeQRContent 生成二维码 → 解析回原文（端到端验证解码链路）
func TestDecodeQRContent(t *testing.T) {
	contents := []string{
		"https://qr.alipay.com/fkx12345abc",
		"weixin://wxpay/bizpayurl?pr=AbCdEf123",
	}
	for _, want := range contents {
		png, err := qrcode.Encode(want, qrcode.Medium, 256)
		if err != nil {
			t.Fatalf("生成二维码失败: %v", err)
		}
		got, err := capture.DecodeQRContent(png)
		if err != nil {
			t.Fatalf("解析二维码失败: %v", err)
		}
		if got != want {
			t.Fatalf("解析内容不一致: want=%q got=%q", want, got)
		}
	}
}

// TestDecodeQRContentInvalid 非二维码图片 → 报错
func TestDecodeQRContentInvalid(t *testing.T) {
	// 纯色 PNG（无二维码）
	if _, err := capture.DecodeQRContent([]byte("not an image")); err == nil {
		t.Fatal("非法输入应报错")
	}
	// 有效图片但无二维码
	png, _ := qrcode.Encode("x", qrcode.Medium, 64)
	if _, err := capture.DecodeQRContent(png[:32]); err == nil {
		t.Fatal("截断图片应报错")
	}
}

// TestDecodeQRContentEmpty 空输入 → 报错
func TestDecodeQRContentEmpty(t *testing.T) {
	if _, err := capture.DecodeQRContent(nil); err == nil {
		t.Fatal("空输入应报错")
	}
	if _, err := capture.DecodeQRContent([]byte{}); err == nil {
		t.Fatal("空输入应报错")
	}
}

// TestDecodeQRContentTooLarge 超 2MB → 报错
func TestDecodeQRContentTooLarge(t *testing.T) {
	big := make([]byte, capture.MaxQRUploadSize+1)
	if _, err := capture.DecodeQRContent(big); err == nil {
		t.Fatal("超大图片应报错")
	}
	if !strings.Contains(decodeErr(capture.DecodeQRContent(big)), "2MB") {
		t.Fatal("错误信息应包含大小限制说明")
	}
}

func decodeErr(_ string, err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
