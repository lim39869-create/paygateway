package notifywatcher

import (
	"strings"
	"testing"
)

// 真机实测数据（2026-08-30，红米 k60 MIUI14 Android 13）。
// 数据来源：dumpsys notification --noredact 实测输出（转账 0.10 元触发微信/支付宝收款通知）。

// realWechatMain 微信主号收款通知（uid=10277，文本含"(朋友到店)"场景标签）。
const realWechatMain = `    NotificationRecord(0x0eb536c3: pkg=com.tencent.mm user=UserHandle{0} id=-124356843 tag=null importance=4 key=0|com.tencent.mm|-124356843|null|10277: Notification(channel=message_channel_new_id shortcut=null contentView=null vibrate=null sound=null tick defaults=0x0 flags=0x11 color=0x00000000 category=msg vis=PRIVATE))
      uid=10277 userId=0
      opPkg=com.tencent.mm
      icon=Icon(typ=RESOURCE pkg=com.tencent.mm id=0x7f080bc8)
      flags=0x11
      originalFlags=0x11
      pri=1
      key=0|com.tencent.mm|-124356843|null|10277
      seen=true
      groupKey=0|com.tencent.mm|-124356843|null|10277
      notification=
            fullscreenIntent=null
            contentIntent=PendingIntent{7a9eb40: PendingIntentRecord{e3b9b0e com.tencent.mm startActivity (allowlist: 8cc672e:+30s0ms/0/NOTIFICATION_SERVICE/NotificationManagerService)}}
            deleteIntent=null
            number=700
            groupAlertBehavior=0
            when=1788083827846
            tickerText=微信收款助手: 微信支付收款0.10元(朋友到店)
            contentView=null
            bigContentView=null
            headsUpContentView=null
            color=0x00000000
            timeout=unknown
            extras={
                android.title=String (微信收款助手)
                android.reduced.images=Boolean (true)
                android.subText=null
                android.showChronometer=Boolean (false)
                android.text=String (微信支付收款0.10元(朋友到店))
                android.progress=Integer (0)
                android.progressMax=Integer (0)
                android.appInfo=ApplicationInfo (ApplicationInfo{c86da79 com.tencent.mm})
                android.showWhen=Boolean (true)
                android.largeIcon=Icon (Icon(typ=BITMAP size=130x130))
                android.infoText=null
                android.progressIndeterminate=Boolean (false)
                android.remoteInputHistory=null
            }`

// realWechatDual 微信双开(XSpace)收款通知（uid=99910277，聚合 [2条]）。
const realWechatDual = `    NotificationRecord(0x0e9213be: pkg=com.tencent.mm user=UserHandle{999} id=-124356843 tag=null importance=4 key=999|com.tencent.mm|-124356843|null|99910277: Notification(channel=message_channel_new_id shortcut=null contentView=null vibrate=null sound=null tick defaults=0x0 flags=0x11 color=0x00000000 category=msg vis=PRIVATE))
      uid=99910277 userId=999
      icon=Icon(typ=RESOURCE pkg=com.tencent.mm id=0x7f080bc8)
      pri=1
      key=999|com.tencent.mm|-124356843|null|99910277
      when=1788083804845
      tickerText=微信收款助手: 微信支付收款0.10元
      extras={
            android.title=String (微信收款助手)
            android.subText=null
            android.text=String ([2条]微信收款助手: 微信支付收款0.10元)
            android.showWhen=Boolean (true)
            android.infoText=null
      }`

// realAlipay 支付宝收款通知（uid=10250，标题含"（老顾客消费）"场景标签）。
const realAlipay = `    NotificationRecord(0x03b84a6f: pkg=com.eg.android.AlipayGphone user=UserHandle{0} id=1223552217 tag=null importance=4 key=0|com.eg.android.AlipayGphone|1223552217|null|10250: Notification(channel=alipay_default shortcut=null contentView=null vibrate=null sound=null tick defaults=0x4 flags=0x11 color=0x00000000 groupKey=com.eg.android.AlipayGphone.POPULAR_EVENT_GROUP vis=PRIVATE))
      uid=10250 userId=0
      when=1788083627490
      tickerText=已转入余额 发起店铺充值活动>>
      extras={
            android.title=String (你已成功收款0.10元（老顾客消费）)
            android.text=String (已转入余额 发起店铺充值活动>>)
            android.showWhen=Boolean (true)
      }`

// realAlipayDual 支付宝双开(XSpace)收款通知（uid=99910250）。
const realAlipayDual = `    NotificationRecord(0x0d7d01ac: pkg=com.eg.android.AlipayGphone user=UserHandle{999} id=1027977183 tag=null importance=4 key=999|com.eg.android.AlipayGphone|1027977183|null|99910250: Notification(channel=alipay_default shortcut=null contentView=null vibrate=null sound=null tick defaults=0x4 flags=0x11 color=0x00000000 groupKey=com.eg.android.AlipayGphone.POPULAR_EVENT_GROUP vis=PRIVATE))
      uid=99910250 userId=999
      when=1788083627491
      extras={
            android.title=String (你已成功收款0.10元)
            android.text=String (已转入余额)
      }`

// nonPaymentAlipay 支付宝非收款通知（活动/营销，无"收款"文本，应被跳过）。
const nonPaymentAlipay = `    NotificationRecord(0x03c8c250: pkg=com.eg.android.AlipayGphone user=UserHandle{0} id=2049664768 tag=null importance=4 key=0|com.eg.android.AlipayGphone|2049664768|null|10250: Notification(channel=alipay_default shortcut=null contentView=null vibrate=null sound=null defaults=0x0 flags=0x200 color=0x00000000 groupKey=com.eg.android.AlipayGphone.POPULAR_EVENT_GROUP vis=PRIVATE))
      uid=10250 userId=0
      when=1788083628051
      extras={
            android.title=String (活动通知)
            android.text=String (限时优惠)
      }`

// unrelated 其他 APP 通知（MIUI 系统/视频），应被忽略。
const unrelated = `    NotificationRecord(0x0015f4bf: pkg=com.android.updater user=UserHandle{0} id=0 tag=null importance=5 key=0|com.android.updater|0|null|6102: Notification(channel=updater_channel_id shortcut=null contentView=null vibrate=null sound=null tick defaults=0x4 flags=0x22 color=0x00000000 vis=PRIVATE))
      uid=6102 userId=0
      when=1788080945841
      extras={
            android.title=String (发现系统有新版本)
            android.text=String (1.0.16.0.UMNCNXM)
      }`

func TestParse_WechatMain(t *testing.T) {
	evs := Parse(realWechatMain, "dev-01")
	if len(evs) != 1 {
		t.Fatalf("期望 1 条事件，实际 %d", len(evs))
	}
	e := evs[0]
	if e.Provider != "wechat" {
		t.Errorf("provider = %q, 期望 wechat", e.Provider)
	}
	if e.Space != 0 {
		t.Errorf("space = %d, 期望 0（主空间）", e.Space)
	}
	if e.AmountMinor != 10 {
		t.Errorf("amount = %d, 期望 10（0.10元）", e.AmountMinor)
	}
	if e.When != 1788083827846 {
		t.Errorf("when = %d, 期望 1788083827846", e.When)
	}
	if !strings.Contains(e.Key, "com.tencent.mm") {
		t.Errorf("key 异常: %q", e.Key)
	}
	// 指纹稳定：同一事件两次生成必须相同
	if e.Fingerprint() != evs[0].Fingerprint() {
		t.Error("指纹不稳定")
	}
}

func TestParse_WechatDual(t *testing.T) {
	evs := Parse(realWechatDual, "dev-01")
	if len(evs) != 1 {
		t.Fatalf("期望 1 条事件，实际 %d", len(evs))
	}
	e := evs[0]
	if e.Provider != "wechat" {
		t.Errorf("provider = %q, 期望 wechat", e.Provider)
	}
	if e.Space != 999 {
		t.Errorf("space = %d, 期望 999（双开）", e.Space)
	}
	if e.AmountMinor != 10 {
		t.Errorf("amount = %d, 期望 10", e.AmountMinor)
	}
}

func TestParse_Alipay(t *testing.T) {
	evs := Parse(realAlipay, "dev-01")
	if len(evs) != 1 {
		t.Fatalf("期望 1 条事件，实际 %d", len(evs))
	}
	e := evs[0]
	if e.Provider != "alipay" {
		t.Errorf("provider = %q, 期望 alipay", e.Provider)
	}
	if e.Space != 0 {
		t.Errorf("space = %d, 期望 0", e.Space)
	}
	if e.AmountMinor != 10 {
		t.Errorf("amount = %d, 期望 10", e.AmountMinor)
	}
}

func TestParse_AlipayDual(t *testing.T) {
	evs := Parse(realAlipayDual, "dev-01")
	if len(evs) != 1 {
		t.Fatalf("期望 1 条事件，实际 %d", len(evs))
	}
	e := evs[0]
	if e.Space != 999 {
		t.Errorf("space = %d, 期望 999", e.Space)
	}
	if e.AmountMinor != 10 {
		t.Errorf("amount = %d, 期望 10", e.AmountMinor)
	}
}

func TestParse_SkipNonPayment(t *testing.T) {
	// 非收款通知（活动/营销/其他APP）→ 0 条
	evs := Parse(nonPaymentAlipay+unrelated, "dev-01")
	if len(evs) != 0 {
		t.Errorf("期望 0 条事件（非收款通知应跳过），实际 %d", len(evs))
	}
}

func TestParse_MixedFullDump(t *testing.T) {
	// 完整通知栏混合输出：微信主+双开、支付宝主+双开、非收款、其他APP
	out := strings.Join([]string{realWechatMain, realWechatDual, realAlipay, realAlipayDual,
		nonPaymentAlipay, unrelated}, "\n")
	evs := Parse(out, "dev-01")
	if len(evs) != 4 {
		t.Fatalf("期望 4 条收款事件，实际 %d", len(evs))
	}
	// 设备ID必须写入（跨设备隔离）
	for _, e := range evs {
		if e.DeviceID != "dev-01" {
			t.Errorf("device_id 未写入: %q", e.DeviceID)
		}
	}
}

func TestParse_DedupAggregated(t *testing.T) {
	// 同一条通知在 dump 中重复出现（模拟轮询间隔内通知未变）→ 幂等
	out := realWechatMain + "\n" + realWechatMain
	evs := Parse(out, "dev-01")
	if len(evs) != 1 {
		t.Errorf("重复记录应去重为 1 条，实际 %d", len(evs))
	}
}

func TestExtractAmount(t *testing.T) {
	cases := []struct {
		text string
		want int64
	}{
		{"微信支付收款0.10元(朋友到店)", 10},
		{"你已成功收款0.10元（老顾客消费）", 10},
		{"[2条]微信收款助手: 微信支付收款0.10元", 10},
		{"收款1元", 100},
		{"收款 12.5元", 1250},
		{"到账 100.00元", 10000},
		{"收款0.10", 0},  // 无"元"单位 → 不匹配（避免误提取）
		{"已转入余额", 0},   // 无金额
		{"限时优惠", 0},    // 非收款
		{"充值10元话费", 0}, // 有"元"但非收款/到账语境 → 不匹配
	}
	for _, c := range cases {
		got := extractAmount(c.text)
		if got != c.want {
			t.Errorf("extractAmount(%q) = %d, 期望 %d", c.text, got, c.want)
		}
	}
}
