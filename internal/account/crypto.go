// Package account 账户池：多支付账户管理 + 加密存储 + 调度器
package account

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MasterKeySize AES-256 密钥长度（32 字节）
const MasterKeySize = 32

// GenerateMasterKey 生成新的主密钥（hex 编码，64 字符）
func GenerateMasterKey() (string, error) {
	key := make([]byte, MasterKeySize)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("生成主密钥失败: %w", err)
	}
	return hex.EncodeToString(key), nil
}

// masterKey 运行时主密钥（字节形式）
var masterKey []byte

// InitCrypto 初始化加密模块（传入 hex 编码的主密钥）
func InitCrypto(hexKey string) error {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return fmt.Errorf("主密钥格式错误: %w", err)
	}
	if len(key) != MasterKeySize {
		return fmt.Errorf("主密钥长度错误: 需要 %d 字节，实际 %d", MasterKeySize, len(key))
	}
	masterKey = key
	return nil
}

// Encrypt AES-256-GCM 加密，返回 hex 编码密文
func Encrypt(plaintext []byte) (string, error) {
	if masterKey == nil {
		return "", errors.New("加密模块未初始化")
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return "", fmt.Errorf("创建加密器失败: %w", err)
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("创建 GCM 失败: %w", err)
	}
	nonce := make([]byte, aesGCM.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("生成 nonce 失败: %w", err)
	}
	ciphertext := aesGCM.Seal(nonce, nonce, plaintext, nil)
	return hex.EncodeToString(ciphertext), nil
}

// Decrypt 解密 hex 编码的 AES-256-GCM 密文
func Decrypt(encryptedHex string) ([]byte, error) {
	if masterKey == nil {
		return nil, errors.New("加密模块未初始化")
	}
	ciphertext, err := hex.DecodeString(encryptedHex)
	if err != nil {
		return nil, fmt.Errorf("密文格式错误: %w", err)
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("创建解密器失败: %w", err)
	}
	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("创建 GCM 失败: %w", err)
	}
	nonceSize := aesGCM.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, errors.New("密文太短")
	}
	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := aesGCM.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("解密失败（密钥错误或数据损坏）: %w", err)
	}
	return plaintext, nil
}

// EncryptJSON 将任意结构体加密为 hex 密文（JSON 序列化后加密）
func EncryptJSON(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("JSON 序列化失败: %w", err)
	}
	return Encrypt(data)
}

// DecryptJSON 解密 hex 密文并反序列化到目标结构体
func DecryptJSON(encryptedHex string, v any) error {
	data, err := Decrypt(encryptedHex)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("JSON 反序列化失败: %w", err)
	}
	return nil
}
