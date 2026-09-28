package personal

import (
	"strings"
	"testing"
	"time"
)

func TestAllocFormat(t *testing.T) {
	a := NewNoteCodeAllocator()
	for i := 0; i < 200; i++ {
		code, err := a.Alloc()
		if err != nil {
			t.Fatalf("Alloc: %v", err)
		}
		if len(code) != NoteCodeLen {
			t.Fatalf("code length: got %q want %d", code, NoteCodeLen)
		}
		for _, ch := range code {
			if !strings.ContainsRune(noteChars, ch) {
				t.Fatalf("code %q contains invalid char %q", code, ch)
			}
		}
	}
}

func TestAllocUniqueInWindow(t *testing.T) {
	a := NewNoteCodeAllocator()
	seen := make(map[string]bool)
	for i := 0; i < 5000; i++ {
		code, err := a.Alloc()
		if err != nil {
			t.Fatalf("Alloc: %v", err)
		}
		if seen[code] {
			t.Fatalf("duplicate code in window: %q", code)
		}
		seen[code] = true
	}
}

func TestAllocNoConfusableChars(t *testing.T) {
	a := NewNoteCodeAllocator()
	for i := 0; i < 1000; i++ {
		code, _ := a.Alloc()
		for _, ch := range code {
			if ch == 'o' || ch == '0' || ch == 'l' || ch == '1' {
				t.Fatalf("confusable char %q in code %q", ch, code)
			}
		}
	}
}

func TestAllocExpiryReuse(t *testing.T) {
	a := NewNoteCodeAllocator()
	code, _ := a.Alloc()
	// 手动过期（模拟 5 分钟后）
	a.mu.Lock()
	a.used[code] = time.Now().Add(-time.Second)
	a.mu.Unlock()
	// 再分配应能拿到同一 code（或至少不报错）
	code2, err := a.Alloc()
	if err != nil {
		t.Fatalf("Alloc after expiry: %v", err)
	}
	_ = code2
}

func TestAllocRelease(t *testing.T) {
	a := NewNoteCodeAllocator()
	code, _ := a.Alloc()
	a.Release(code)
	// 释放后可立即复用
	code2, _ := a.Alloc()
	if code2 == code {
		return // OK
	}
	// 不强制相同（随机），但必须成功
	if code2 == "" {
		t.Fatalf("released code should be allocatable")
	}
}
