// Package img implements JMComic image de-scrambling and format handling.
//
// 算法精确对齐 jmcomic.JmImageTool（v2.7.4），经源码交叉验证：
//
//  1. 块数计算 get_num(scramble_id, aid, filename)：
//     - aid < scramble_id           → 0   (该本子早于 scramble 启用，无需还原)
//     - aid < 268850                → 10  (旧算法固定 10 块)
//     - aid < 421926                → x=10，num = md5hex(aid+filename)[-1] % 10
//     - aid >= 421926 (2023-02-08起) → x=8， num = md5hex(aid+filename)[-1] % 8
//     - 最终 num = num*2 + 2  ∈ [2..20] 或 [2..16]
//
//  2. 还原 decode_and_save(num)：源图按 floor(h/num) 等分，余数 over=h%num
//     全部加到「源图最底部那块」，取块顺序为源图底部往上、贴到目标图顶部往下
//     （等价于垂直翻转）。
//
// 非切割图片 (num==0) 原样透传；GIF 一律透传（动画帧无法无损重编码）。
// 切割图片解码后统一重编码为 JPEG(quality) 或 PNG(quality<=0)，因为浏览器
// 普遍可解码且 Go 标准库不提供 webp 编码。
package img

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math"

	// 注册 webp 解码器（纯 Go，无 CGO）。
	_ "golang.org/x/image/webp"
)

// 禁漫图片切割算法的历史分水岭（aid 阈值）。
const (
	Scramble268850 int64 = 268850 // 旧固定 10 块的上界
	Scramble421926 int64 = 421926 // 2023-02-08 起改算法的分水岭
)

var ErrUnsupported = errors.New("img: unsupported or corrupt image")

// Outcome 描述 Decode 的产出。
type Outcome struct {
	Data    []byte
	Suffix  string // ".jpg" | ".png" | 原始后缀（透传时）
	Changed bool   // 是否经过重编码
}

// Decode 还原一张禁漫图片。
//
// 参数：
//   - raw：原始图片字节（可能被切割混淆）
//   - suffix：原始后缀，如 ".webp"
//   - scrambleID：本子/章节的 scramble_id
//   - aid：章节 ID（用于判断算法版本与块数派生）
//   - filename：图片文件名（不含路径、不含后缀，如 "00001"）
//   - quality：JPEG 质量 1-100；<=0 表示输出 PNG
func Decode(raw []byte, suffix, scrambleID string, aid int64, filename string, quality int) (*Outcome, error) {
	if equalsFoldSuffix(suffix, ".gif") {
		return &Outcome{Data: raw, Suffix: ".gif", Changed: false}, nil
	}

	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}

	scrambleInt := parseScrambleID(scrambleID)
	num := ComputeNum(scrambleInt, aid, filename)
	if num <= 1 {
		// 未切割 → 无损透传，保留原始格式
		return &Outcome{Data: raw, Suffix: normalizeSuffix(format, suffix), Changed: false}, nil
	}

	restored := Restore(img, num)

	var buf bytes.Buffer
	var outSuffix string
	if quality <= 0 {
		if err := png.Encode(&buf, restored); err != nil {
			return nil, err
		}
		outSuffix = ".png"
	} else {
		if err := jpeg.Encode(&buf, restored, &jpeg.Options{Quality: clamp(quality, 1, 100)}); err != nil {
			return nil, err
		}
		outSuffix = ".jpg"
	}
	return &Outcome{Data: buf.Bytes(), Suffix: outSuffix, Changed: true}, nil
}

// ComputeNum 精确复刻 Python JmImageTool.get_num。
//
// 返回 0 表示该图片未被切割（透传即可）。
func ComputeNum(scrambleID, aid int64, filename string) int {
	if aid < scrambleID {
		return 0
	}
	if aid < Scramble268850 {
		return 10
	}
	x := 10
	if aid >= Scramble421926 {
		x = 8
	}
	sum := md5.Sum([]byte(fmt.Sprintf("%d%s", aid, filename)))
	hexStr := hex.EncodeToString(sum[:])
	last := hexStr[len(hexStr)-1] // '0'..'9' | 'a'..'f'
	n := int(last) % x
	return n*2 + 2
}

// Restore 还原切割图片，精确对齐 Python decode_and_save。
//
// 余数 over = h % num 全部并入源图最底部那块；取块方向为「源图底部→目标图顶部」
// （即垂直翻转）。
func Restore(src image.Image, num int) image.Image {
	b := src.Bounds()
	w := b.Dx()
	h := b.Dy()
	if num <= 1 || w == 0 || h == 0 {
		return src
	}
	over := h % num
	move := h / num // floor
	if move < 1 {
		return src
	}

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < num; i++ {
		curMove := move
		ySrc := h - move*(i+1) - over
		yDst := move * i
		if i == 0 {
			curMove += over // 源图最底部那块多吃余数
		} else {
			yDst += over
		}
		if ySrc < 0 {
			ySrc = 0
		}
		if yDst < 0 {
			yDst = 0
		}
		// 逐行复制 src[0..w, ySrc..ySrc+curMove] 到 dst[0..w, yDst..]
		for dy := 0; dy < curMove && ySrc+dy < h && yDst+dy < h; dy++ {
			for x := 0; x < w; x++ {
				dst.Set(x, yDst+dy, src.At(b.Min.X+x, b.Min.Y+ySrc+dy))
			}
		}
	}
	return dst
}

// 辅助：把字符串 scramble_id 解析为 int64；解析失败返回极大值（视为已启用切割）。
func parseScrambleID(s string) int64 {
	if s == "" {
		return 0
	}
	var v int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		v = v*10 + int64(c-'0')
	}
	return v
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// equalsFoldSuffix 比较后缀（不区分大小写）。
func equalsFoldSuffix(s, suffix string) bool {
	if len(s) < len(suffix) {
		return false
	}
	tail := s[len(s)-len(suffix):]
	for i := 0; i < len(suffix); i++ {
		a, b := tail[i], suffix[i]
		if 'A' <= a && a <= 'Z' {
			a += 'a' - 'A'
		}
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

// normalizeSuffix 由解码 format 推断后缀，回退到 fallback。
func normalizeSuffix(format, fallback string) string {
	switch format {
	case "jpeg":
		return ".jpg"
	case "png":
		return ".png"
	case "gif":
		return ".gif"
	case "webp":
		return ".webp"
	default:
		if fallback != "" {
			return fallback
		}
		return "." + format
	}
}

// SniffReader 探测图片格式与尺寸，不消费完整数据。
func SniffReader(r io.Reader) (format string, cfg image.Config, err error) {
	cfg, format, err = image.DecodeConfig(r)
	return
}

// Floor 兼容旧引用（math.Floor 已导出，此处保留以防外部误用）。
var _ = math.Floor
