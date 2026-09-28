package channel

import (
	"fmt"
	"sync"
)

// registry 通道注册表（线程安全）
var (
	mu       sync.RWMutex
	registry = make(map[string]PaymentChannel)
	enabled  = make(map[string]bool) // 通道启用状态，默认启用
)

// Register 注册通道（每个通道在 init 或组装阶段调用）
func Register(c PaymentChannel) {
	mu.Lock()
	defer mu.Unlock()
	registry[c.Name()] = c
	enabled[c.Name()] = true // 默认启用
}

// Get 按名称获取通道（仅返回已启用的通道）
func Get(name string) (PaymentChannel, bool) {
	mu.RLock()
	defer mu.RUnlock()
	if !enabled[name] {
		return nil, false
	}
	c, ok := registry[name]
	return c, ok
}

// Names 返回已启用通道名列表
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(registry))
	for name := range registry {
		if enabled[name] {
			out = append(out, name)
		}
	}
	return out
}

// AllNames 返回所有已注册通道名列表（含禁用，管理后台用）
func AllNames() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	return out
}

// IsEnabled 检查通道是否启用
func IsEnabled(name string) bool {
	mu.RLock()
	defer mu.RUnlock()
	return enabled[name]
}

// SetEnabled 设置通道启用/禁用状态
func SetEnabled(name string, on bool) {
	mu.Lock()
	defer mu.Unlock()
	enabled[name] = on
}

// Lookup 按名称查找通道（不检查启用状态，管理后台用）
func Lookup(name string) (PaymentChannel, bool) {
	mu.RLock()
	defer mu.RUnlock()
	c, ok := registry[name]
	return c, ok
}

// EpayTypeToChannel 易支付协议类型 → 通道注册名映射
// epay 协议 type 参数使用 "alipay"/"wxpay"，需要转为通道注册名
var EpayTypeToChannel = map[string]string{
	"alipay": "merchant.alipay",
	"wxpay":  "merchant.wechat",
	"qqpay":  "merchant.qq",
	"usdt":   "merchant.usdt",
	// 个人免签通道（显式选择，商户传 type=personal_alipay / personal_wxpay）
	"personal_alipay": "personal.alipay",
	"personal_wxpay":  "personal.wechat",
}

// EpayTypeToPersonalChannel 易支付类型 → 个人免签通道名（优先路由到个人通道）
// 当对应个人通道已注册且配置了收款账户时，用 ResolveEpayType 优先选择个人通道
var EpayTypeToPersonalChannel = map[string]string{
	"alipay": "personal.alipay",
	"wxpay":  "personal.wechat",
	"qqpay":  "personal.qq",
}

// ResolveEpayType 解析易支付 type 参数为实际通道名
// 路由规则（个人免签优先）：
//  1. 若对应个人通道已注册且已启用 → 优先使用个人通道（个人免签 0 费率）
//  2. 否则回退到商家通道（merchant.*）
//  3. 都不存在 → 返回原映射（调用方报"不支持的支付类型"）
func ResolveEpayType(epayType string) string {
	// 个人通道优先（仅当注册且启用）
	if pName, ok := EpayTypeToPersonalChannel[epayType]; ok {
		if c, ok := Get(pName); ok && c != nil {
			return pName
		}
	}
	// 回退商家通道
	if mName, ok := EpayTypeToChannel[epayType]; ok {
		return mName
	}
	return ""
}

// ProviderFromName 从通道名提取 Provider（如 "merchant.alipay" → "alipay"）
func ProviderFromName(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			return name[i+1:]
		}
	}
	return name
}

// ErrUnsupported 不支持的支付类型
var ErrUnsupported = fmt.Errorf("不支持的支付类型")

// MustGet 获取通道，不存在则 panic（组装阶段校验配置）
func MustGet(name string) PaymentChannel {
	c, ok := Get(name)
	if !ok {
		panic(fmt.Sprintf("通道未注册: %s", name))
	}
	return c
}
