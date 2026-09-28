// Package notifywatcher 通知栏快速通道监听（独立于 ledgermon 的账本慢通道）。
//
// 背景（2026-08-30 真机实测 + 用户需求确认）：
//   - 微信/支付宝收款到账会弹系统通知（dumpsys notification --noredact 可读全文）；
//   - 通知栏比账本页快 ~15s，是收款感知的快速通道；
//   - 通知栏数据不清理会一直存在 → 匹配窗口必须以通知 when（收款发生时刻）为锚，
//     不能用 time.Now()（否则旧通知会错误匹配新订单）；
//   - 账户区分靠 uid：微信/支付宝主空间 uid 无 999 前缀，双开(XSpace) uid 有 999 前缀
//     （实测：com.tencent.mm uid=10277 主号 / uid=99910277 双开；支付宝同理 10250/99910250）。
//
// 本包只负责：设备枚举 → 按设备轮询 dumpsys notification --noredact → 解析 →
// 指纹去重 → 回调 main.go 注入的匹配入账函数（复用 matchPersonalOrder 链路）。
// 不直接触碰 UI/账本页，与 ledgermon 的 DeviceActor 串行队列零竞争。
package notifywatcher

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// 收款通知金额提取正则（微信/支付宝实测格式）：
//   - 微信: "微信支付收款0.10元(朋友到店)"  → 文本含 "收款0.10元"
//   - 支付宝: "你已成功收款0.10元（老顾客消费）" → 标题含 "收款0.10元"
//
// 兼容 "收款 1 元"、"到账 12.5元" 等变体（贪心取收款/到账后第一个金额）。
var amountRe = regexp.MustCompile(`(?:收款|到账)\s*([0-9]+(?:\.[0-9]{1,2})?)元`)

// 目标包名：微信 / 支付宝（个人免签通道对应的收款 APP）。
const (
	pkgWechat = "com.tencent.mm"
	pkgAlipay = "com.eg.android.AlipayGphone"
)

// Event 单条收款通知的解析结果（设备级：属于哪台设备、哪个空间）。
type Event struct {
	DeviceID    string // 设备ID（personal.devices.device_id，枚举时携带）
	Provider    string // "wechat" / "alipay"
	Space       int    // 0=主空间，999=双开(XSpace)
	AmountMinor int64  // 金额（分）
	When        int64  // 通知创建时间（when= 毫秒时间戳，收款发生时刻）
	Key         string // 系统通知 key（0|pkg|id|tag|uid，同通知轮询去重基准之一）
	Title       string // 通知标题（审计）
	Text        string // 通知正文（审计）
	Raw         string // 指纹原文（审计；含设备ID保证跨设备隔离）
}

// Fingerprint 通知指纹：同设备+同通知(key)+同金额+同时刻 只处理一次。
// when 来自通知系统时间戳，同一条通知每次 dump 不变 → 幂等去重可靠。
func (e *Event) Fingerprint() string {
	raw := fmt.Sprintf("notify:%s|%s|%d|%s|%d|%d", e.DeviceID, e.Provider, e.Space, e.Key, e.AmountMinor, e.When)
	return sha256Hex(raw)
}

// Parse 解析 dumpsys notification --noredact 输出，返回该设备的所有收款通知。
// deviceID 由调用方传入（同一份输出只会属于一台设备）。
// 规则：
//  1. 只关心 pkg ∈ {微信, 支付宝} 的 NotificationRecord；
//  2. 从 user=UserHandle{0}/{999} 判定空间；
//  3. 从标题/正文提取金额（收款/到账 XX元），提取不到金额的跳过（非收款通知）；
//  4. 同一条通知被 Android 聚合/折叠时可能产生多条记录，按 (key,when) 幂等去重。
func Parse(out string, deviceID string) []Event {
	var events []Event
	seen := make(map[string]struct{})

	// 按 NotificationRecord( 分块解析：块内收集 pkg/user/key/title/text/when
	for _, block := range splitRecords(out) {
		pkg := fieldOf(block, "pkg=")
		if pkg != pkgWechat && pkg != pkgAlipay {
			continue
		}
		provider := "wechat"
		if pkg == pkgAlipay {
			provider = "alipay"
		}
		// 空间：user=UserHandle{999} → 双开；user=UserHandle{0} → 主空间
		space := 0
		if m := regexp.MustCompile(`user=UserHandle\{(\d+)\}`).FindStringSubmatch(block); len(m) == 2 {
			if v, err := strconv.Atoi(m[1]); err == nil {
				space = v
			}
		}
		// 通知 key：NotificationRecord(0x...: pkg=... key=0|pkg|id|tag|uid)
		key := fieldOf(block, "key=")
		// when 毫秒时间戳
		var when int64
		if m := regexp.MustCompile(`when=(\d+)`).FindStringSubmatch(block); len(m) == 2 {
			when, _ = strconv.ParseInt(m[1], 10, 64)
		}
		title, text := textsOf(block)
		// 金额：优先标题（支付宝在标题，微信标题也可能含），再正文
		amount := extractAmount(title)
		if amount <= 0 {
			amount = extractAmount(text)
		}
		if amount <= 0 {
			continue // 非收款通知（无金额）或金额格式未知 → 不处理
		}
		if when <= 0 || key == "" {
			continue // 缺时间戳/缺key 的畸形记录不处理
		}
		// 幂等：同一 (key,when) 只保留一条（聚合折叠场景）
		uid := key + "|" + strconv.FormatInt(when, 10)
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}

		events = append(events, Event{
			DeviceID:    deviceID,
			Provider:    provider,
			Space:       space,
			AmountMinor: amount,
			When:        when,
			Key:         key,
			Title:       title,
			Text:        text,
			Raw:         fmt.Sprintf("notify:%s|%s|%d|%s", deviceID, provider, space, key),
		})
	}
	return events
}

// splitRecords 按 NotificationRecord( 拆分通知块（每块含完整通知字段）。
func splitRecords(out string) []string {
	var blocks []string
	cur := strings.Builder{}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "NotificationRecord(") {
			if cur.Len() > 0 {
				blocks = append(blocks, cur.String())
			}
			cur.Reset()
		}
		cur.WriteString(line)
		cur.WriteString("\n")
	}
	if cur.Len() > 0 {
		blocks = append(blocks, cur.String())
	}
	return blocks
}

// fieldOf 提取形如 "name=value" 的字段值（value 到下一个空格/冒号为止）。
func fieldOf(block, prefix string) string {
	i := strings.Index(block, prefix)
	if i < 0 {
		return ""
	}
	rest := block[i+len(prefix):]
	end := strings.IndexAny(rest, " \n")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// textsOf 提取通知标题与正文（android.title / android.text 的 String(...) 内容）。
func textsOf(block string) (title, text string) {
	title = quotedStringOf(block, "android.title=String (")
	text = quotedStringOf(block, "android.text=String (")
	// 标题缺失时用 tickerText 兜底（tickerText=微信收款助手: 微信支付收款0.10元）
	if title == "" {
		if t := quotedStringOf(block, "tickerText="); t != "" {
			if i := strings.Index(t, ": "); i >= 0 {
				title = t[:i]
				if text == "" {
					text = t[i+2:]
				}
			} else {
				title = t
			}
		}
	}
	return title, text
}

// quotedStringOf 提取 "prefix<内容>)" 形式的值（内容到最近右括号）。
func quotedStringOf(block, prefix string) string {
	i := strings.Index(block, prefix)
	if i < 0 {
		return ""
	}
	rest := block[i+len(prefix):]
	j := strings.Index(rest, ")")
	if j < 0 {
		return rest
	}
	return rest[:j]
}

// extractAmount 从通知文本提取金额（分）。"收款0.10元" → 10。
func extractAmount(s string) int64 {
	m := amountRe.FindStringSubmatch(s)
	if len(m) != 2 {
		return 0
	}
	return amountToMinor(m[1])
}

// amountToMinor 金额字符串转分（"0.10" → 10，"1" → 100，"1.5" → 150）。
func amountToMinor(s string) int64 {
	var major, minor int64
	if _, err := fmt.Sscanf(s, "%d.%d", &major, &minor); err != nil {
		// 无小数：整数元
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return v * 100
		}
		return 0
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		frac := s[i+1:]
		switch len(frac) {
		case 1:
			minor *= 10
		case 2:
			// 保持原样
		default:
			minor = 0
		}
	} else {
		minor = 0
	}
	return major*100 + minor
}
