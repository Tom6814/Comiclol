package jmcomic

import (
	"crypto/aes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// TestTokenSignature 验证 token 签名算法：token = md5hex(ts + secret)。
func TestTokenSignature(t *testing.T) {
	ts := "1700566805"
	secret := APPTokenSecret
	want := md5hex(ts + secret)
	got := signToken(ts, APPVersion, secret).Token
	if got != want {
		t.Fatalf("token mismatch: got %s want %s", got, want)
	}
	// 自洽校验：md5hex 实现
	sum := md5.Sum([]byte(ts + secret))
	if hex.EncodeToString(sum[:]) != want {
		t.Fatalf("md5hex 实现错误")
	}
}

// TestTokenParamFormat 验证 tokenparam = "{ts},{ver}"。
func TestTokenParamFormat(t *testing.T) {
	tp := signToken("123", "2.0.30", APPTokenSecret)
	if tp.TokenParam != "123,2.0.30" {
		t.Fatalf("tokenparam = %q", tp.TokenParam)
	}
	if tp.TS != "123" {
		t.Fatalf("TS = %q", tp.TS)
	}
}

// TestRespDataEncryptDecrypt 用内置 AES-ECB 加密一段明文，再用 DecodeRespData 解密，
// 验证解密链路与 PKCS7 处理正确。
func TestRespDataEncryptDecrypt(t *testing.T) {
	ts := "1700000000"
	secret := APPTokenSecret
	plain := `{"id":"123","name":"测试本子","tags":["a","b"]}`

	key := []byte(md5hex(ts + secret))
	enc, err := aesECBEncrypt([]byte(plain), key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	data := base64.StdEncoding.EncodeToString(enc)

	got, err := DecodeRespData(data, ts, secret)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != plain {
		t.Fatalf("roundtrip mismatch:\n got=%s\nwant=%s", got, plain)
	}
}

// TestAESECBBlockAlignment 验证非 16 字节整数倍的输入也能正确加解密。
func TestAESECBBlockAlignment(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	for _, n := range []int{1, 5, 16, 17, 31, 32, 33, 100} {
		in := []byte(strings.Repeat("x", n))
		enc, err := aesECBEncrypt(in, key)
		if err != nil {
			t.Fatalf("n=%d encrypt: %v", n, err)
		}
		if len(enc)%aes.BlockSize != 0 {
			t.Fatalf("n=%d ciphertext not block-aligned", n)
		}
		dec, err := aesECBDecrypt(enc, key)
		if err != nil {
			t.Fatalf("n=%d decrypt: %v", n, err)
		}
		if string(dec) != string(in) {
			t.Fatalf("n=%d roundtrip mismatch", n)
		}
	}
}

// TestFixTokenCached 验证 fixTokenTriple 进程级只计算一次。
func TestFixTokenCached(t *testing.T) {
	ts1, tok1, tp1 := fixTokenTriple()
	ts2, tok2, tp2 := fixTokenTriple()
	if ts1 != ts2 || tok1 != tok2 || tp1 != tp2 {
		t.Fatalf("fixTokenTriple 未缓存：两次结果不一致")
	}
	if tok1 != md5hex(ts1+APPTokenSecret) {
		t.Fatalf("fixTokenTriple token 错误")
	}
}
