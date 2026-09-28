// 无订单收款行为标记测试：只做信息标注（tags/level），不影响入账链路。
package main

import (
	"context"
	"testing"
	"time"

	"paygateway/internal/personal"
)

func TestAssessUnmatched_BigAmount(t *testing.T) {
	app, _ := newMatchTestEnv(t)
	app.cfg = &Config{PersonalBigAmount: 20000} // 200 元
	tags, level := app.assessUnmatched(context.Background(), "alipay", 20000, "", "dev-1")
	if len(tags) == 0 || tags[0] != "big_amount" || level != riskHighWarning {
		t.Fatalf("大额应标记 big_amount 高风险，实际 tags=%v level=%d", tags, level)
	}
}

func TestAssessUnmatched_BurstWindow(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	for i := 0; i < 5; i++ {
		_, _ = pdb.CreateUnmatched(ctx, &personal.UnmatchedPayment{
			Provider: "alipay", AmountMinor: 100, DeviceID: "dev-burst",
			CreatedAt: time.Now(),
		})
	}
	tags, level := app.assessUnmatched(ctx, "alipay", 100, "", "dev-burst")
	has := false
	for _, tag := range tags {
		if tag == "burst" {
			has = true
		}
	}
	if !has || level != riskSuspect {
		t.Fatalf("5 笔/30min 应标记 burst 疑似核实，实际 tags=%v level=%d", tags, level)
	}
}

// 窗口外的历史记录不构成 burst（防陈旧计数）。
func TestAssessUnmatched_StaleNotCounted(t *testing.T) {
	users := make([]*personal.UnmatchedPayment, 0, 6)
	for i := 0; i < 6; i++ {
		users = append(users, &personal.UnmatchedPayment{
			Provider: "alipay", AmountMinor: 100, DeviceID: "dev-old",
			CreatedAt: time.Now().Add(-2 * time.Hour),
		})
	}
	tags := scoreFromRows(100, "", "dev-old", users, time.Now(), riskWindow)
	for _, tag := range tags {
		if tag == "burst" {
			t.Fatalf("窗口外记录不应构成 burst: %v", tags)
		}
	}
}

// 同付款人窗口内重复 ≥2 笔 → repeat_payer。
func TestAssessUnmatched_RepeatPayer(t *testing.T) {
	rows := []*personal.UnmatchedPayment{
		{Provider: "alipay", AmountMinor: 200, Payer: "*杰", CreatedAt: time.Now()},
	}
	tags := scoreFromRows(200, "*杰", "dev-x", rows, time.Now(), riskWindow)
	if len(tags) != 1 || tags[0] != "repeat_payer" {
		t.Fatalf("同付款人应标记 repeat_payer，实际 %v", tags)
	}
}
