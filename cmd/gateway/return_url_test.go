// return_url 兜底重写测试：上游传内网/localhost 时的三层替换与保留语义。
//
// Given 公网 return_url（或空）：原样保留，不触发重写
// Given 内网 return_url + Referer：重写为 Referer origin，路径与查询参数无损
// Given 内网 return_url（无有效候选）：返回原值
package main

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func ctxWithReferer(t *testing.T, referer string) *gin.Context {
	t.Helper()
	req := httptest.NewRequest("GET", "/submit.php", nil)
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = req
	return ctx
}

func TestRehost(t *testing.T) {
	got := rehost("http://localhost:3000/usage-logs/common?q=2", "https://api.example.com")
	want := "https://api.example.com/usage-logs/common?q=2"
	if got != want {
		t.Fatalf("rehost = %q want %q", got, want)
	}
	if rehost("http://localhost:3000/x", "") != "" {
		t.Fatal("空 origin 应返回空")
	}
}

func TestSanitizeReturnURL_PublicKept(t *testing.T) {
	app := &App{}
	c := ctxWithReferer(t, "https://other.tld/checkout")
	got := app.sanitizeReturnURL(c, "O1", "2001", "https://shop.example.com/ok")
	if got != "https://shop.example.com/ok" {
		t.Fatalf("公网 return_url 不应被改写，实际 %q", got)
	}
	if app.sanitizeReturnURL(c, "O1", "2001", "") != "" {
		t.Fatal("空 return_url 应原样保留")
	}
}

// Referer 兜底：localhost return_url 重写为 Referer origin（真实发起支付的前端）。
func TestSanitizeReturnURL_RefererWins(t *testing.T) {
	app := &App{}
	c := ctxWithReferer(t, "https://xapi-public.example.com/pay/checkout")
	got := app.sanitizeReturnURL(c, "O2", "2001", "http://localhost:3000/usage-logs/common")
	want := "https://xapi-public.example.com/usage-logs/common"
	if got != want {
		t.Fatalf("应重写为 Referer origin，实际 %q want %q", got, want)
	}
}
