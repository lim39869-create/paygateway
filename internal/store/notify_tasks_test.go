package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newNotifyTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	st, err := NewSQLite(filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 失败: %v", err)
	}
	return st
}

// TestNotifyTask_CRUDPlusPayload 覆盖死信管理新增方法：GetNotifyTask / UpdateNotifyTaskPayload / DeleteNotifyTask。
func TestNotifyTask_CRUDPlusPayload(t *testing.T) {
	st := newNotifyTestStore(t)
	ctx := context.Background()

	// CreateNotifyTask 需要一个真实 order_id（外键/展示用），这里先用 0 也不影响 notify_tasks 表。
	id, err := st.CreateNotifyTask(ctx, 123, `{"notify_url":"http://old.example/cb","out_trade_no":"T1"}`)
	if err != nil {
		t.Fatalf("CreateNotifyTask err: %v", err)
	}

	task, err := st.GetNotifyTask(ctx, id)
	if err != nil || task == nil {
		t.Fatalf("GetNotifyTask = %v, %v; 期望非 nil", task, err)
	}
	if task.OrderID != 123 || task.Status != "pending" {
		t.Fatalf("GetNotifyTask 字段不符: order_id=%d status=%s", task.OrderID, task.Status)
	}

	// UpdateNotifyTaskPayload：换成最新回调地址
	newPayload := `{"notify_url":"http://new.example/cb","out_trade_no":"T1"}`
	if err := st.UpdateNotifyTaskPayload(ctx, id, newPayload); err != nil {
		t.Fatalf("UpdateNotifyTaskPayload err: %v", err)
	}
	got, _ := st.GetNotifyTask(ctx, id)
	if got.Payload != newPayload {
		t.Fatalf("payload 未更新: %s", got.Payload)
	}

	// 重置为 pending（死信重发路径）
	if err := st.UpdateNotifyTask(ctx, id, "pending", 0, time.Now()); err != nil {
		t.Fatalf("UpdateNotifyTask err: %v", err)
	}
	got, _ = st.GetNotifyTask(ctx, id)
	if got.Status != "pending" || got.RetryCount != 0 {
		t.Fatalf("重置 pending 失败: status=%s retry=%d", got.Status, got.RetryCount)
	}

	// DeleteNotifyTask：删除后 GetNotifyTask 返回 nil
	if err := st.DeleteNotifyTask(ctx, id); err != nil {
		t.Fatalf("DeleteNotifyTask err: %v", err)
	}
	if gone, _ := st.GetNotifyTask(ctx, id); gone != nil {
		t.Fatalf("删除后仍能查到任务: %+v", gone)
	}

	// GetNotifyTask 不存在的 id 返回 nil（非错误）
	if miss, _ := st.GetNotifyTask(ctx, 999999); miss != nil {
		t.Fatalf("不存在的 id 应返回 nil: %+v", miss)
	}
}
