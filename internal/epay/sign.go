// Package epay 易支付协议层：对外接口（submit/api/签名），保持向后兼容
package epay

import (
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// ParamsFilter 过滤参数，生成签名时需删除 "sign" 和 "sign_type" 参数
func ParamsFilter(params map[string]string) map[string]string {
	filtered := make(map[string]string)
	for k, v := range params {
		// pay_type 是网关扩展参数（选择支付模式），不参与签名
		if k == "sign" || k == "sign_type" || k == "pay_type" || v == "" {
			continue
		}
		filtered[k] = v
	}
	return filtered
}

// ParamsSort 对参数排序
func ParamsSort(params map[string]string) []string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// CreateUrlString 生成待签名字符串 "a=1&b=2&c=3"
func CreateUrlString(params map[string]string) string {
	keys := ParamsSort(params)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return strings.Join(parts, "&")
}

// MD5String 生成加盐 MD5 字符串
func MD5String(urlString, key string) string {
	digest := md5.Sum([]byte(urlString + key))
	return hex.EncodeToString(digest[:])
}

// GenerateSign 生成签名
func GenerateSign(params map[string]string, key string) string {
	filtered := ParamsFilter(params)
	return MD5String(CreateUrlString(filtered), key)
}

// VerifySign 验证签名（常数时间比较，防时序侧信道逐字节猜签名）
func VerifySign(params map[string]string, key string) bool {
	sign := params["sign"]
	expected := GenerateSign(params, key)
	return subtle.ConstantTimeCompare([]byte(sign), []byte(expected)) == 1
}

// BuildSignedParams 生成带签名的参数
func BuildSignedParams(params map[string]string, key string) map[string]string {
	params["sign"] = GenerateSign(params, key)
	params["sign_type"] = "MD5"
	return params
}

// BuildQueryString 将参数拼接成 URL query string
func BuildQueryString(params map[string]string) string {
	keys := ParamsSort(params)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return strings.Join(parts, "&")
}

// DebugPrint 打印参数用于调试
func DebugPrint(tag string, params map[string]string) {
	fmt.Printf("[%s] %s\n", tag, BuildQueryString(params))
}
