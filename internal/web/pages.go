// Package web 支付页面渲染：二维码页 / 跳转中转页
package web

import (
	"encoding/base64"
	"fmt"
	"html"
	"net/url"
	"strconv"
	"strings"
	"time"

	"paygateway/internal/logger"
	"paygateway/internal/model"

	qrcode "github.com/skip2/go-qrcode"
)

// BuildQRPage 构建支付二维码展示页面（服务端生成 base64，不依赖 CDN）
// channelName: alipay / wechat，用于页面品牌文案
func BuildQRPage(order *model.Order, qrCode, channelName string) string {
	qrPNG, err := qrcode.Encode(qrCode, qrcode.Medium, 280)
	if err != nil {
		logger.Error("web", "生成二维码失败", "err", err)
		qrPNG = []byte{}
	}
	qrBase64 := base64.StdEncoding.EncodeToString(qrPNG)

	// 根据通道选择品牌文案（支持 "alipay"、"merchant.alipay"、"personal.alipay" 等格式）
	title := "请使用支付宝扫码支付"
	sub := "打开支付宝扫一扫完成付款"
	imgAlt := "支付宝收款二维码"
	tip := "提示：测试环境请使用<b>沙箱版支付宝</b>扫码支付"
	if strings.HasSuffix(channelName, "wechat") {
		title = "请使用微信扫码支付"
		sub = "打开微信扫一扫完成付款"
		imgAlt = "微信收款二维码"
		tip = "提示：请使用<b>微信</b>扫一扫完成付款"
	}

	// 轮询 /qr/status：订单到达 TRADE_SUCCESS 后原地切换为成功视图（免跳转闪烁）
	successDoc := buildSuccessDoc(order, "支付成功")

	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>%s</title>
<style>
body { font-family: -apple-system, BlinkMacSystemFont, sans-serif; text-align: center; padding: 40px 16px; background: #f7f8fa; }
.card { background: #fff; border-radius: 12px; padding: 32px 20px; max-width: 420px; margin: 0 auto; box-shadow: 0 2px 12px rgba(0,0,0,0.08); }
h2 { color: #333; margin: 0 0 6px; font-size: 20px; }
.sub { color: #999; font-size: 14px; margin-bottom: 20px; }
.qr { margin: 0 auto 20px; width: 280px; height: 280px; }
.qr img { width: 280px; height: 280px; border-radius: 8px; }
.info { color: #666; font-size: 14px; margin-bottom: 16px; line-height: 1.8; }
.tip { color: #e6a23c; font-size: 13px; margin-bottom: 16px; line-height: 1.6; }
.btns { display: flex; gap: 12px; justify-content: center; flex-wrap: wrap; }
.btn { padding: 12px 24px; border-radius: 8px; font-size: 15px; text-decoration: none; display: inline-block; }
.btn-primary { background: #1677ff; color: #fff; }
.btn-outline { border: 1px solid #1677ff; color: #1677ff; background: #fff; }
.hidden { display: none; }
@media (max-width: 600px) {
  .qr { width: 240px; height: 240px; }
  .qr img { width: 240px; height: 240px; }
}
</style>
</head>
<body>
<div class="card">
  <h2>%s</h2>
  <div class="sub">%s</div>
  <div class="qr"><img src="data:image/png;base64,%s" alt="%s"></div>
  <div class="info">订单号: %s<br>金额: <b>¥%s</b></div>
  <div class="tip">%s</div>
  <div class="btns">
    <a class="btn btn-outline" href="/qr/refresh?out_trade_no=%s">刷新二维码</a>
  </div>
</div>
<script>
(function(){
  var no = '%s';
  if (!no) return;
  var successHtml = %s;
  var polled = false;
  var timer = setInterval(function(){
    try {
      var x = new XMLHttpRequest();
      x.open('GET', '/qr/status?out_trade_no=' + encodeURIComponent(no), true);
      x.timeout = 4000;
      x.onload = function(){
        if (x.status !== 200) return;
        try {
          var d = JSON.parse(x.responseText);
          if (d.status === 'TRADE_SUCCESS' && !polled) {
            polled = true;
            clearInterval(timer);
            document.open(); document.write(successHtml); document.close();
          }
        } catch(e) {}
      };
      x.send();
    } catch(e) {}
  }, 2000);
})();
</script>
</body>
</html>`, title, title, sub, qrBase64, imgAlt, order.OutTradeNo, order.MoneyYuan(), tip, order.OutTradeNo,
		order.OutTradeNo, strconv.Quote(successDoc))
}

// BuildPersonalQRPage 个人免签支付页：真实收款码 + 标题上方内嵌关单倒计时条
//
// 转账提示含备注码（order.NoteCode）：备注码是收款匹配引擎的主手段
// （noteMatchFromPool 精确匹配 → 失败才回退金额/时间窗口），
// 用户转账时填写备注码可避免同金额订单串单。订单无备注码时该区块不渲染。
// 收款方名称由用户的原生付款页展示（防劫持核对在支付 App 内完成）。
// expireAt 为零时（未开启自动关单）不显示倒计时；倒计时归零自动跳转 /qr/expired 重新下单页。
// 支付方式切换不在此页提供：上游换支付模式入口（同号不同 type）下单时服务端自动迁移订单。
func BuildPersonalQRPage(order *model.Order, qrImageSrc, channelName string, expireAt time.Time) string {
	provider := "支付宝"
	scanHint := "打开支付宝扫一扫，向商家收款码转账"
	imgAlt := "收款二维码"
	if strings.HasSuffix(channelName, "wechat") {
		provider = "微信"
		scanHint = "打开微信扫一扫，向商家收款码转账"
		imgAlt = "微信收款二维码"
	}
	title := "请使用" + provider + "扫码转账"

	// 备注码提示区块（仅在订单有备注码时渲染；无备注码=金额/时间窗口兜底匹配）
	noteBlock := ""
	if order.NoteCode != "" {
		noteBlock = `<div class="note">转账时请备注 <b>` + html.EscapeString(order.NoteCode) + `</b>，用于快速确认到账</div>`
	}

	// 倒计时开关：expireAt 为零则整块隐藏
	expireRFC3339 := ""
	if !expireAt.IsZero() {
		expireRFC3339 = expireAt.Format(time.RFC3339)
	}
	expiredURL := "/qr/expired?out_trade_no=" + url.QueryEscape(order.OutTradeNo) + "&return_url=" + url.QueryEscape(order.ReturnURL)

	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>%s</title>
<style>
body { font-family: -apple-system, BlinkMacSystemFont, sans-serif; text-align: center; padding: 40px 16px; background: #f7f8fa; }
.card { background: #fff; border-radius: 12px; padding: 32px 20px; max-width: 420px; margin: 0 auto; box-shadow: 0 2px 12px rgba(0,0,0,0.08); }
.countdown { background: #fff7e6; border: 1px solid #ffd591; border-radius: 8px; padding: 10px 14px; color: #d46b08; font-size: 14px; margin-bottom: 18px; }
.countdown b { font-size: 20px; font-variant-numeric: tabular-nums; }
h2 { color: #333; margin: 0 0 6px; font-size: 20px; }
.sub { color: #999; font-size: 14px; margin-bottom: 20px; }
.qr { margin: 0 auto 20px; width: 280px; height: 280px; }
.qr img { width: 280px; height: 280px; border-radius: 8px; }
.info { color: #666; font-size: 14px; margin-bottom: 16px; line-height: 1.8; }
.note { background: #f0f5ff; border: 1px solid #adc6ff; border-radius: 8px; padding: 10px 14px; color: #1d39c4; font-size: 15px; margin-bottom: 16px; }
.note b { font-size: 20px; letter-spacing: 2px; }
.btns { display: flex; gap: 12px; justify-content: center; flex-wrap: wrap; }
.care-broadcast { margin-top: 16px; padding: 10px 14px; background: #fff7e6; border: 1px solid #ffd591; border-radius: 8px; color: #d46b08; font-size: 13px; line-height: 1.8; text-align: center; }
.care-broadcast b { color: #d4380d; }
.btn { padding: 12px 24px; border-radius: 8px; font-size: 15px; text-decoration: none; display: inline-block; }
.btn-outline { border: 1px solid #1677ff; color: #1677ff; background: #fff; }
</style>
</head>
<body>
<div class="card">
  <div class="countdown" id="countdown">支付剩余时间 <b id="cd">--:--</b>，超时订单将自动关闭</div>
  <h2>%s</h2>
  <div class="sub">%s</div>
  <div class="qr"><img src="%s" alt="%s"></div>
  <div class="info">订单号: %s<br>金额: <b>¥%s</b></div>
  %s
  <div class="btns">
    <a class="btn btn-outline" href="/qr/refresh?out_trade_no=%s">刷新二维码</a>
  </div>
  <div class="care-broadcast">支付完成后若<b>未到账/未升级套餐</b>，请联系平台管理人员处理；请勿重复支付。<br>若提示<b>备注填写过于频繁</b>，请勿再备注、直接转账即可；或更换账户/稍等片刻再支付。</div>
</div>
<script>
(function(){
  var expire = Date.parse('%s');
  var url = %s;
  var cdEl = document.getElementById('cd');
  var cdBar = document.getElementById('countdown');
  if (!expire) { cdBar.style.display = 'none'; return; }
  function fmt(s){ var m = Math.floor(s/60), r = s%%60; return m + ':' + (r<10?'0':'') + r; }
  function tick(){
    var s = Math.floor((expire - Date.now()) / 1000);
    if (s <= 0) { location.replace(url); return; }
    cdEl.textContent = fmt(s);
    setTimeout(tick, 1000);
  }
  tick();
  try {
    var c = document.createElement('canvas'); c.width = 200; c.height = 40;
    var ctx = c.getContext('2d');
    ctx.textBaseline = 'top'; ctx.font = '14px Arial'; ctx.fillStyle = '#f60';
    ctx.fillRect(50, 0, 50, 40); ctx.fillStyle = '#069';
    ctx.fillText('pg-fp-2837', 2, 15); ctx.fillText('canvas-fp', 4, 30);
    var canvas = c.toDataURL();
    var fp = {
      canvas: canvas,
      tz: new Date().getTimezoneOffset(),
      scr: screen.width + 'x' + screen.height + 'x' + screen.colorDepth,
      lang: (navigator.language || ''),
      hw: navigator.hardwareConcurrency || 0,
      mem: (navigator.deviceMemory || 0),
      pf: (navigator.platform || ''),
      cookie: document.cookie
    };
    try {
      var fpStr = btoa(unescape(encodeURIComponent(JSON.stringify(fp))));
      navigator.sendBeacon('/personal/bind-fp?o=' + encodeURIComponent('%s') + '&fp=' + encodeURIComponent(fpStr));
    } catch(e) {
      var x = new XMLHttpRequest(); x.open('GET', '/personal/bind-fp?o=' + encodeURIComponent('%s') + '&fp=' + encodeURIComponent(btoa(unescape(encodeURIComponent(JSON.stringify(fp))))), true); x.send();
    }
  } catch(e) {}
  // 支付成功探测：轮询 /qr/status，订单到达 TRADE_SUCCESS 后跳转成功页。
  // 不用 document.write 原地覆盖：微信 X5/XWeb 内核对文档加载完成后的
  // document.write 兼容性差（静默失败/白屏，真机实测切页无效），location.replace
  // 全内核可靠。轮询间隔 2s。
  (function(){
    var no = '%s';
    if (!no) return;
    var jumped = false;
    var timer = setInterval(function(){
      try {
        var x = new XMLHttpRequest();
        x.open('GET', '/qr/status?out_trade_no=' + encodeURIComponent(no), true);
        x.timeout = 4000;
        x.onload = function(){
          if (x.status !== 200) return;
          try {
            var d = JSON.parse(x.responseText);
            if (d.status === 'TRADE_SUCCESS' && !jumped) {
              jumped = true;
              clearInterval(timer);
              location.replace('/qr/success?out_trade_no=' + encodeURIComponent(no));
            }
          } catch(e) {}
        };
        x.send();
      } catch(e) {}
    }, 2000);
  })();
})();
</script>
</body>
</html>`, title, title, scanHint, qrImageSrc, imgAlt,
		order.OutTradeNo, order.MoneyYuan(), noteBlock, order.OutTradeNo, expireRFC3339, strconv.Quote(expiredURL),
		order.OutTradeNo, order.OutTradeNo, order.OutTradeNo)
}

// BuildExpiredPage 订单过期页：倒计时归零后跳转至此。
// 过期订单是终态（不允许手动复活，防僵尸订单刷单），仅引导返回上游重新下单；
// 但上游同号重提会让服务端复活订单 → 本页 JS 轮询 /qr/status，探测到复活自动跳回支付页。
func BuildExpiredPage(provider, outTradeNo, returnURL string) string {
	title := "订单已过期"
	reorder := "javascript:history.back()"
	btnText := "返回重新下单"
	if returnURL != "" {
		reorder = returnURL
	}
	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>%s</title>
<style>
body { font-family: -apple-system, BlinkMacSystemFont, "PingFang SC", "Microsoft YaHei", sans-serif; text-align: center; padding: 0; margin: 0; background: linear-gradient(180deg, #f0f5ff 0%%, #f7f8fa 40%%); min-height: 100vh; display: flex; align-items: center; justify-content: center; }
.card { background: #fff; border-radius: 16px; padding: 36px 28px; max-width: 400px; width: 100%%; box-sizing: border-box; margin: 16px; box-shadow: 0 8px 32px rgba(22, 119, 255, 0.08); }
.icon-wrap { width: 72px; height: 72px; border-radius: 50%%; background: #fff1f0; display: flex; align-items: center; justify-content: center; margin: 0 auto 18px; }
.icon { font-size: 38px; line-height: 1; }
.badge { display: inline-block; padding: 3px 14px; border-radius: 20px; background: #fff1f0; color: #f5222d; font-size: 13px; font-weight: 600; margin-bottom: 10px; }
h2 { color: #333; margin: 0 0 10px; font-size: 21px; font-weight: 700; }
.sub { color: #888; font-size: 14px; margin-bottom: 22px; line-height: 1.6; }
.order-info { background: #fafafa; border-radius: 10px; padding: 14px 16px; margin-bottom: 24px; font-size: 13px; color: #666; }
.order-info .row { display: flex; justify-content: space-between; padding: 4px 0; }
.order-info .row .label { color: #999; }
.order-info .row .val { color: #333; font-weight: 600; word-break: break-all; }
.tip { color: #bbb; font-size: 12px; margin-bottom: 20px; line-height: 1.6; }
.btns { display: flex; gap: 12px; justify-content: center; flex-wrap: wrap; }
.btn { display: inline-block; padding: 13px 34px; border-radius: 10px; font-size: 15px; text-decoration: none; font-weight: 600; box-sizing: border-box; }
.btn-primary { background: #1677ff; color: #fff; box-shadow: 0 4px 12px rgba(22, 119, 255, 0.25); }
</style>
</head>
<body>
<div class="card">
  <div class="icon-wrap"><div class="icon">⏰</div></div>
  <div class="badge">订单已过期</div>
  <div class="sub">未在限定时间内完成%s支付<br>订单已自动关闭，未产生任何扣款</div>
  <div class="order-info">
    <div class="row"><span class="label">订单号</span><span class="val">%s</span></div>
    <div class="row"><span class="label">状态</span><span class="val" style="color:#f5222d;">已关闭</span></div>
  </div>
  <div class="btns">
    <a class="btn btn-primary" href="%s">%s</a>
  </div>
  <div class="tip">若您已完成转账但页面未更新，请稍候刷新查看</div>
</div>
<script>
// 探测订单是否已被上游同号重提复活：复活则自动跳回支付页继续支付
(function(){
  var no = '%s';
  if (!no) return;
  var timer = setInterval(function(){
    try {
      var x = new XMLHttpRequest();
      x.open('GET', '/qr/status?out_trade_no=' + encodeURIComponent(no), true);
      x.timeout = 4000;
      x.onload = function(){
        if (x.status !== 200) return;
        try {
          var d = JSON.parse(x.responseText);
          if (d.status === 'TRADE_SUCCESS') {
            // 已入账成功（含账本/通知延迟入账场景）：补跳成功页，用户不因倒计时
            // 早跳过期页而错过支付成功界面
            clearInterval(timer);
            location.replace('/qr/success?out_trade_no=' + encodeURIComponent(no));
            return;
          }
          if (d.alive) {
            clearInterval(timer);
            location.replace('/qr/refresh?out_trade_no=' + encodeURIComponent(no));
          }
        } catch(e) {}
      };
      x.send();
    } catch(e) {}
  }, 2000);
})();
</script>
</body>
</html>`, title, provider, outTradeNo, reorder, btnText, outTradeNo)
}

// BuildSuccessPage 支付成功状态页：付款人完成支付后展示（跳转落地页）。
//
// 该页为终态展示，仅由服务端在订单状态确认为 TRADE_SUCCESS 后渲染
// （见 handleQrSuccess 校验），不可由付款人任意触发（防伪造"支付成功"页）。
// 与 BuildExpiredPage 同套卡片风格，绿色成功主题。
// channelName: merchant.alipay / personal.alipay / merchant.wechat / personal.wechat ... 决定品牌文案。
func BuildSuccessPage(order *model.Order, channelName string) string {
	badge := "支付成功"
	if strings.HasSuffix(channelName, "wechat") {
		badge = "微信支付成功"
	} else if strings.HasSuffix(channelName, "alipay") {
		badge = "支付宝支付成功"
	}
	return buildSuccessDoc(order, badge)
}

// buildSuccessDoc 生成完整支付成功文档（供 /qr/success 落地页与支付页原地切换共用，
// 保证付款人无论走跳转还是轮询原地切换看到的一致成功界面）。
func buildSuccessDoc(order *model.Order, badge string) string {
	paidText := ""
	if order.PaidAt != nil && !order.PaidAt.IsZero() {
		paidText = order.PaidAt.Format("2006-01-02 15:04:05")
	}
	returnBtn := ""
	if order.ReturnURL != "" {
		returnBtn = `<a class="btn btn-primary" href="` + html.EscapeString(order.ReturnURL) + `" id="returnBtn">返回商户</a>`
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>%s</title>
<style>
body { font-family: -apple-system, BlinkMacSystemFont, "PingFang SC", "Microsoft YaHei", sans-serif; text-align: center; padding: 0; margin: 0; background: linear-gradient(180deg, #f6ffed 0%%, #f7f8fa 40%%); min-height: 100vh; display: flex; align-items: center; justify-content: center; }
.card { background: #fff; border-radius: 16px; padding: 36px 28px; max-width: 400px; width: 100%%; box-sizing: border-box; margin: 16px; box-shadow: 0 8px 32px rgba(22, 163, 74, 0.1); }
.icon-wrap { width: 72px; height: 72px; border-radius: 50%%; background: #f6ffed; display: flex; align-items: center; justify-content: center; margin: 0 auto 18px; border: 1px solid #d9f7be; }
.icon { font-size: 38px; line-height: 1; }
.badge { display: inline-block; padding: 3px 14px; border-radius: 20px; background: #f6ffed; color: #389e0d; font-size: 13px; font-weight: 600; margin-bottom: 10px; }
h2 { color: #333; margin: 0 0 10px; font-size: 21px; font-weight: 700; }
.sub { color: #889; font-size: 14px; margin-bottom: 22px; line-height: 1.6; }
.order-info { background: #fafafa; border-radius: 10px; padding: 14px 16px; margin-bottom: 24px; font-size: 13px; color: #666; }
.order-info .row { display: flex; justify-content: space-between; padding: 4px 0; }
.order-info .row .label { color: #999; }
.order-info .row .val { color: #333; font-weight: 600; word-break: break-all; }
.order-info .row .val.amount { color: #52c41a; font-size: 15px; }
.tip { color: #bbb; font-size: 12px; margin-bottom: 20px; line-height: 1.6; }
.btns { display: flex; gap: 12px; justify-content: center; flex-wrap: wrap; }
.btn { display: inline-block; padding: 13px 34px; border-radius: 10px; font-size: 15px; text-decoration: none; font-weight: 600; box-sizing: border-box; }
.btn-primary { background: #52c41a; color: #fff; box-shadow: 0 4px 12px rgba(82, 196, 26, 0.25); }
.btn-outline { border: 1px solid #d9d9d9; color: #666; background: #fff; }
</style>
</head>
<body>
<div class="card">
  <div class="icon-wrap"><div class="icon">✅</div></div>
  <div class="badge">%s</div>
  <h2>已支付成功</h2>
  <div class="sub">您的订单已支付成功，交易已完成</div>
  <div class="order-info">
    <div class="row"><span class="label">订单号</span><span class="val">%s</span></div>
    <div class="row"><span class="label">支付金额</span><span class="val amount">¥%s</span></div>
    <div class="row"><span class="label">支付时间</span><span class="val">%s</span></div>
    <div class="row"><span class="label">状态</span><span class="val" style="color:#52c41a;">支付成功</span></div>
  </div>
  %s
  <div class="tip">建议<b style="color:#d48806;">保存本页截图</b>（订单号+付款凭据），有疑问时方便平台快速查单<br>商家将尽快为您发货，请留意订单状态</div>
</div>
</body>
</html>`, badge, badge,
		order.OutTradeNo, order.MoneyYuan(), paidText, returnBtn)
}

// QRPng 生成二维码 PNG（280x280，Medium 纠错）。失败返回空切片。
func QRPng(content string) []byte {
	png, err := qrcode.Encode(content, qrcode.Medium, 280)
	if err != nil {
		logger.Error("web", "生成二维码 PNG 失败", "err", err)
		return nil
	}
	return png
}

// QRPngDataURI 生成二维码 data URI（token 套码模式支付页内嵌用）
func QRPngDataURI(content string) string {
	png := QRPng(content)
	if len(png) == 0 {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

// BuildRedirectPage 支付中转页：从本站跳转支付通道，携带正确 Referer
func BuildRedirectPage(order *model.Order, payURL, title string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>正在跳转到支付宝...</title>
<style>
body { font-family: -apple-system, sans-serif; text-align: center; padding: 60px 20px; background: #f7f8fa; }
.card { background: #fff; border-radius: 12px; padding: 40px 20px; max-width: 400px; margin: 0 auto; box-shadow: 0 2px 12px rgba(0,0,0,0.06); }
.spinner { width: 40px; height: 40px; border: 3px solid #e8e8e8; border-top-color: #1677ff; border-radius: 50%%; margin: 0 auto 20px; animation: spin 1s linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }
h3 { color: #333; margin-bottom: 8px; }
.info { color: #999; font-size: 14px; margin-bottom: 24px; }
.btn { display: block; width: 100%%; padding: 12px; background: #1677ff; color: #fff; border: none; border-radius: 8px; font-size: 16px; text-decoration: none; box-sizing: border-box; }
.btn:hover { opacity: 0.9; }
.manual { display: block; margin-top: 16px; color: #999; font-size: 13px; }
</style>
</head>
<body>
<div class="card">
  <div class="spinner"></div>
  <h3>%s</h3>
  <div class="info">订单号: %s<br>金额: ¥%s</div>
  <a class="btn" href="%s" id="payBtn">立即支付</a>
  <span class="manual">如果未自动跳转，请点击上方按钮</span>
</div>
<script>
window.location.href = %q;
</script>
</body>
</html>`, title, order.OutTradeNo, order.MoneyYuan(), payURL, payURL)
}

// IsMobileUA 判断是否为移动端 User-Agent
func IsMobileUA(ua string) bool {
	uaLower := strings.ToLower(ua)
	mobileKeywords := []string{
		"mobile", "android", "iphone", "ipad", "ipod",
		"windows phone", "blackberry", "opera mini", "iemobile",
	}
	for _, kw := range mobileKeywords {
		if strings.Contains(uaLower, kw) {
			return true
		}
	}
	return false
}
