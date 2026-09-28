// 临时 E2E 工具：解密上游密钥 + 构造签名下单请求（验证后删除）
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"paygateway/internal/account"
	"paygateway/internal/epay"
	"paygateway/internal/store"
)

func main() {
	keyHex := os.Getenv("ACCOUNT_MASTER_KEY")
	if err := account.InitCrypto(keyHex); err != nil {
		panic("InitCrypto: " + err.Error())
	}
	db, err := store.NewUpstreamDB("data/upstreams.db")
	if err != nil {
		panic("NewUpstreamDB: " + err.Error())
	}
	defer db.Close()
	up, err := db.GetUpstreamByPid(context.Background(), "2001")
	if err != nil || up == nil {
		panic(fmt.Sprintf("GetByPid: %v up=%v", err, up))
	}
	key := up.KeyEnc
	dec, err := account.Decrypt(key)
	if err != nil {
		panic("Decrypt: " + err.Error())
	}
	fmt.Println("UPSTREAM_KEY_DECRYPTED len:", len(dec))

	// 构造 epay 下单参数（type=alipay 会优先路由 personal.alipay）
	outNo := fmt.Sprintf("E2E%d", time.Now().Unix())
	params := map[string]string{
		"pid":          "2001",
		"type":         "alipay",
		"out_trade_no": outNo,
		"name":         "e2e测试",
		"money":        "1.00",
		"notify_url":   "http://127.0.0.1:8080/notify",
		"return_url":   "http://127.0.0.1:8080/return",
		"pay_type":     "alipay",
		"device":       "pc",
	}
	signed := epay.BuildSignedParams(params, string(dec))
	form := url.Values{}
	for k, v := range signed {
		form.Set(k, v)
	}
	resp, err := http.Post("http://127.0.0.1:8080/submit.php",
		"application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		panic("POST: " + err.Error())
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	fmt.Println("RESP:", resp.StatusCode)
	fmt.Println(string(buf[:n]))
}
