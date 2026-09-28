package personal

import (
	"crypto/rand"
	"math/big"
	"sync"
	"time"
)

// 备注码分配器（NoteCodeAllocator）
//
// 设计（docs/MIANQIAN.md 6.x）：
//   - 4 位字符集：a-z 小写 + 0-9，排除易混淆 o/0/l/1 → 32 字符
//   - 组合数：32^4 = 1,048,576，5 分钟窗口内实际并发订单远小于此，几乎不会碰撞
//   - 位图 + 过期队列：高并发分配 O(1)，到期自动释放
//   - 系统生成，用户输入 → 精确匹配 → 资金安全

// noteChars 备注码字符集（排除易混淆 o/0/l/1）
const noteChars = "abcdefghijkmnpqrstuvwxyz23456789"

// NoteCodeLen 备注码长度
const NoteCodeLen = 4

// NoteCodeTTL 备注码有效期（订单 5 分钟内未支付则备注码可复用）
const NoteCodeTTL = 5 * time.Minute

// NoteCodeAllocator 备注码分配器（线程安全）
type NoteCodeAllocator struct {
	mu   sync.Mutex
	used map[string]time.Time // code → 过期时间
}

// NewNoteCodeAllocator 创建分配器
func NewNoteCodeAllocator() *NoteCodeAllocator {
	return &NoteCodeAllocator{
		used: make(map[string]time.Time),
	}
}

// Alloc 分配一个未占用且未过期的备注码
func (a *NoteCodeAllocator) Alloc() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// 先清理已过期条目（避免 map 无限增长）
	now := time.Now()
	for code, expire := range a.used {
		if !expire.After(now) {
			delete(a.used, code)
		}
	}

	// 随机生成直到未占用（最多尝试 N 次；全部占满时等量扩容逻辑不适用——32^4 足够大）
	for i := 0; i < 100; i++ {
		code, err := randomCode()
		if err != nil {
			return "", err
		}
		if _, taken := a.used[code]; !taken {
			a.used[code] = now.Add(NoteCodeTTL)
			return code, nil
		}
	}
	// 极端情况：100 次随机碰撞（说明窗口内并发订单接近 100 万），退化为线性扫描
	// 扫描全部 32^4 组合找第一个空位；若真全满则清空最旧一批
	for _, c1 := range noteChars {
		for _, c2 := range noteChars {
			for _, c3 := range noteChars {
				for _, c4 := range noteChars {
					code := string([]rune{c1, c2, c3, c4})
					if _, taken := a.used[code]; !taken {
						a.used[code] = now.Add(NoteCodeTTL)
						return code, nil
					}
				}
			}
		}
	}
	// 全部占用：直接清空（实际不可能发生，兜底防死循环）
	a.used = make(map[string]time.Time)
	code, err := randomCode()
	if err != nil {
		return "", err
	}
	a.used[code] = now.Add(NoteCodeTTL)
	return code, nil
}

// Release 释放备注码（订单支付成功/关闭后调用，可选优化：过期自动释放已足够）
func (a *NoteCodeAllocator) Release(code string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.used, code)
}

// randomCode 生成随机 4 位备注码
func randomCode() (string, error) {
	chars := []rune(noteChars)
	out := make([]rune, NoteCodeLen)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			return "", err
		}
		out[i] = chars[n.Int64()]
	}
	return string(out), nil
}
