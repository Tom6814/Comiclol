// Package jmcomic 的 crypto 子模块实现禁漫 APP API 的加解密。
//
// 算法经 jmcomic v2.7.4 源码交叉验证：
//
//	签名：token = md5hex(ts + secret)，tokenparam = "{ts},{ver}"
//	解密：data(base64) → AES-256-ECB(key=md5hex(ts+secret)[:32]) → PKCS7 unpad → UTF-8 JSON
//
// 关键常量来自 JmMagicConstants（APP_TOKEN_SECRET 等）。
package jmcomic

import (
	"crypto/aes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// APP API 加解密相关常量（来自 jmcomic.jm_config.JmMagicConstants）。
const (
	APPTokenSecret   = "185Hcomic3PAPP7R" // 绝大多数接口的 token/data 密钥
	APPTokenSecret2  = "18comicAPPContent" // 仅 /chapter_view_template 使用
	APPServerSecret  = "diosfjckwpqpdfjkvnqQjsik" // 仅获取最新 API 域名
	APPVersion       = "2.0.30"
)

// tokenPair 缓存：禁漫服务端容忍固定 ts（FLAG_USE_FIX_TIMESTAMP 默认开启），
// 进程级缓存一次即可。
var (
	fixTokenOnce sync.Once
	fixTS        string
	fixToken     string
	fixTokenParam string
)

// md5hex 返回 32 字符小写十六进制摘要。
func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TokenTriple 封装一次签名结果。
type TokenTriple struct {
	TS         string // 秒级时间戳（字符串）
	Token      string // md5hex(ts + secret)
	TokenParam string // "{ts},{ver}"
}

// signToken 用指定密钥计算签名。
func signToken(ts, ver, secret string) TokenTriple {
	return TokenTriple{
		TS:         ts,
		Token:      md5hex(ts + secret),
		TokenParam: ts + "," + ver,
	}
}

// fixTokenTriple 返回进程级缓存的固定签名（默认密钥）。
// 对齐 Python JmModuleConfig.get_fix_ts_token_tokenparam。
func fixTokenTriple() (ts, token, tokenParam string) {
	fixTokenOnce.Do(func() {
		fixTS = strconv.FormatInt(time.Now().Unix(), 10)
		tp := signToken(fixTS, APPVersion, APPTokenSecret)
		fixToken = tp.Token
		fixTokenParam = tp.TokenParam
	})
	return fixTS, fixToken, fixTokenParam
}

// newTimestamp 返回当前秒级时间戳字符串。
func newTimestamp() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}

// aesECBDecrypt 执行 AES-256-ECB 解密 + PKCS7 去填充。
//
// 标准库不直接提供 ECB（因不安全），这里按块手动 Decrypt。
func aesECBDecrypt(cipherText, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("aes-ecb: key must be 32 bytes (got %d)", len(key))
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	bs := blk.BlockSize()
	if len(cipherText) == 0 || len(cipherText)%bs != 0 {
		return nil, errors.New("aes-ecb: ciphertext length not block-aligned")
	}
	out := make([]byte, len(cipherText))
	for i := 0; i < len(cipherText); i += bs {
		blk.Decrypt(out[i:i+bs], cipherText[i:i+bs])
	}
	// PKCS7 unpad
	pad := int(out[len(out)-1])
	if pad < 1 || pad > bs || pad > len(out) {
		return nil, errors.New("aes-ecb: bad PKCS7 padding")
	}
	return out[:len(out)-pad], nil
}

// DecodeRespData 解密响应 data 字段。
//
//	data:   响应 JSON 的 "data" 字段（base64 字符串）
//	ts:     请求时使用的同一个时间戳
//	secret: 默认 APPTokenSecret；/chapter_view_template 用 APPTokenSecret2；
//	        获取最新域名接口用 APPServerSecret（此时 ts 传 ""）
func DecodeRespData(data, ts, secret string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	key := []byte(md5hex(ts + secret)) // 32 字节
	pt, err := aesECBDecrypt(raw, key)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// pkcs7Pad 对明文做 PKCS7 填充至 blockSize 的整数倍（用于请求体加密，备用）。
func pkcs7Pad(in []byte, blockSize int) []byte {
	pad := blockSize - len(in)%blockSize
	out := make([]byte, len(in)+pad)
	copy(out, in)
	for i := len(in); i < len(out); i++ {
		out[i] = byte(pad)
	}
	return out
}

// aesECBEncrypt 执行 AES-256-ECB 加密（请求体加密备用）。
func aesECBEncrypt(plain, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("aes-ecb: key must be 32 bytes (got %d)", len(key))
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	bs := blk.BlockSize()
	padded := pkcs7Pad(plain, bs)
	out := make([]byte, len(padded))
	for i := 0; i < len(padded); i += bs {
		blk.Encrypt(out[i:i+bs], padded[i:i+bs])
	}
	return out, nil
}
