// Package ledger 资金审计账本：哈希链 + 数字签名，保证每笔交易不可篡改
package ledger

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"sync"
	"time"

	"paygateway/internal/store"
)

// Ledger 审计账本
type Ledger struct {
	mu      sync.Mutex
	store   store.LedgerStore
	signKey *ecdsa.PrivateKey
}

// New 创建账本；privKeyPEM 为空时自动生成新密钥对（打印公钥供保存）
func New(st store.LedgerStore, privKeyPEM string) (*Ledger, error) {
	var key *ecdsa.PrivateKey
	var err error
	if privKeyPEM == "" {
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("生成审计密钥失败: %w", err)
		}
		// 输出私钥 PEM 供保存（首次运行）
		der, err := x509.MarshalECPrivateKey(key)
		if err == nil {
			pemBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
			fmt.Printf("[ledger] 首次运行，生成审计密钥，请保存到环境变量 LEDGER_PRIVATE_KEY:\n%s\n", string(pemBytes))
		}
	} else {
		key, err = parsePrivateKey(privKeyPEM)
		if err != nil {
			return nil, fmt.Errorf("解析审计私钥失败: %w", err)
		}
	}
	return &Ledger{store: st, signKey: key}, nil
}

func parsePrivateKey(pemStr string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("无效的 PEM 私钥")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	return key, nil
}

// PublicKeyPEM 返回公钥 PEM（用于校验签名）
func (l *Ledger) PublicKeyPEM() string {
	der, _ := x509.MarshalPKIXPublicKey(&l.signKey.PublicKey)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// HasEvent 按 eventID（订单ID）查询账本是否已有记录（崩溃恢复去重用）
func (l *Ledger) HasEvent(ctx context.Context, eventID int64) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.store.HasLedgerEvent(ctx, eventID)
}

// Append 追加一条不可篡改记录：哈希链 + 签名
// payload 为事件全部内容（订单号+金额+状态+通道交易号+时间+原始数据），必须包含所有不可变字段
func (l *Ledger) Append(ctx context.Context, eventID int64, payload string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// 取链尾哈希
	last, err := l.store.GetLastLedgerEntry(ctx)
	if err != nil {
		return fmt.Errorf("读取链尾失败: %w", err)
	}
	prevHash := ""
	if last != nil {
		prevHash = last.EntryHash
	}

	payloadHash := sha256Hex(payload)
	entryHash := sha256Hex(prevHash + payloadHash)

	// 签名 entry_hash
	sig, err := l.sign(entryHash)
	if err != nil {
		return fmt.Errorf("签名失败: %w", err)
	}

	// 写入账本（只增不改，数据库层有触发器保护）
	if err := l.store.AppendLedger(ctx, prevHash, payloadHash, entryHash, sig, eventID); err != nil {
		return fmt.Errorf("写入账本失败: %w", err)
	}
	return nil
}

// Verify 全链完整性校验：从 genesis 重算哈希链 + 校验签名
func (l *Ledger) Verify(ctx context.Context) (*VerifyResult, error) {
	res := &VerifyResult{StartTime: time.Now()}

	count, err := l.store.CountLedger(ctx)
	if err != nil {
		return nil, err
	}
	res.TotalEntries = count

	var prevHash string
	// 快照锚点校验：锚点必须存在于链上（快照是历史时刻的锚点，允许落后于链尾）
	sn, _ := l.store.GetLatestSnapshot(ctx)
	anchorFound := false
	for id := int64(1); id <= count; id++ {
		entry, err := l.store.GetLedgerEntry(ctx, id)
		if err != nil {
			return nil, err
		}
		if entry == nil {
			res.Errors = append(res.Errors, fmt.Sprintf("账本条目缺失: id=%d", id))
			break
		}
		// 校验链
		expected := sha256Hex(prevHash + entry.PayloadHash)
		if entry.EntryHash != expected {
			res.Errors = append(res.Errors, fmt.Sprintf("哈希链断裂: id=%d 期望=%s 实际=%s", entry.ID, expected, entry.EntryHash))
		}
		// 校验签名
		if !l.verify(entry.EntryHash, entry.Signature) {
			res.Errors = append(res.Errors, fmt.Sprintf("签名校验失败: id=%d", entry.ID))
		}
		if sn != nil && entry.EntryHash == sn.LastHash {
			anchorFound = true
		}
		prevHash = entry.EntryHash
	}

	// 快照锚点存在性：在链上即代表快照覆盖的链段完整（历史锚点，非必须等于链尾）
	if sn != nil {
		if anchorFound {
			res.SnapshotOK = true
		} else {
			res.Errors = append(res.Errors, fmt.Sprintf("快照锚点不在链上: 快照=%s", sn.LastHash))
		}
	}
	res.Valid = len(res.Errors) == 0
	res.EndTime = time.Now()
	return res, nil
}

// CreateSnapshot 生成每日快照（链尾哈希 + 签名）
func (l *Ledger) CreateSnapshot(ctx context.Context, date string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	last, err := l.store.GetLastLedgerEntry(ctx)
	if err != nil {
		return err
	}
	if last == nil {
		return fmt.Errorf("账本为空，无法生成快照")
	}
	sig, err := l.sign(last.EntryHash)
	if err != nil {
		return err
	}
	return l.store.CreateSnapshot(ctx, date, last.EntryHash, sig)
}

// VerifyResult 校验结果
type VerifyResult struct {
	Valid        bool
	TotalEntries int64
	Errors       []string
	SnapshotOK   bool
	StartTime    time.Time
	EndTime      time.Time
}

func (r *VerifyResult) Summary() string {
	if r.Valid {
		return fmt.Sprintf("账本完整 ✅ 共 %d 条，快照锚点匹配", r.TotalEntries)
	}
	return fmt.Sprintf("账本校验失败 ❌ 共 %d 条，%d 处错误", r.TotalEntries, len(r.Errors))
}

// ---------- 工具 ----------

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (l *Ledger) sign(data string) (string, error) {
	h := sha256.Sum256([]byte(data))
	r, s, err := ecdsa.Sign(rand.Reader, l.signKey, h[:])
	if err != nil {
		return "", err
	}
	// 编码 r||s（各32字节）
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return hex.EncodeToString(out), nil
}

func (l *Ledger) verify(data, sigHex string) bool {
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != 64 {
		return false
	}
	h := sha256.Sum256([]byte(data))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	return ecdsa.Verify(&l.signKey.PublicKey, h[:], r, s)
}
