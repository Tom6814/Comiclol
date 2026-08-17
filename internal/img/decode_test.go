package img

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"testing"
)

// TestComputeNumPrecisely 精确复刻 Python get_num 公式，逐分支验证。
func TestComputeNumPrecisely(t *testing.T) {
	cases := []struct {
		name        string
		scrambleID  int64
		aid         int64
		filename    string
		wantFormula func() int
	}{
		{"aid < scrambleID（未切割）", 999999, 123, "00001", func() int { return 0 }},
		{"aid < 268850（旧固定 10）", 0, 200000, "00001", func() int { return 10 }},
		{"aid < 421926（x=10 分支）", 0, 300000, "00001", nil},
		{"aid >= 421926（x=8 分支）", 0, 500000, "00001", nil},
	}
	for _, tc := range cases {
		got := ComputeNum(tc.scrambleID, tc.aid, tc.filename)
		var want int
		if tc.wantFormula != nil {
			want = tc.wantFormula()
		} else {
			want = expectedNum(tc.scrambleID, tc.aid, tc.filename)
		}
		if got != want {
			t.Fatalf("[%s] ComputeNum=%d want=%d", tc.name, got, want)
		}
	}
}

// expectedNum 独立实现一份 Python get_num 公式，用于交叉验证 ComputeNum。
func expectedNum(scrambleID, aid int64, filename string) int {
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
	last := hexStr[len(hexStr)-1]
	n := int(last) % x
	return n*2 + 2
}

// TestComputeNumRange 验证新算法下 num 为偶数且落在合理范围。
func TestComputeNumRange(t *testing.T) {
	for aid := int64(421926); aid < 422026; aid++ {
		for _, fn := range []string{"00001", "00010", "00234"} {
			n := ComputeNum(0, aid, fn)
			if n < 2 || n > 16 || n%2 != 0 {
				t.Fatalf("aid=%d fn=%s num=%d 不在 [2..16] 偶数范围", aid, fn, n)
			}
		}
	}
	for aid := int64(268850); aid < 421926; aid += 1000 {
		n := ComputeNum(0, aid, "00001")
		if n < 2 || n > 20 || n%2 != 0 {
			t.Fatalf("aid=%d num=%d 不在 [2..20] 偶数范围", aid, n)
		}
	}
}

// TestRestoreIdentityWhenNumLE1 验证 num<=1 时图片原样返回。
func TestRestoreIdentityWhenNumLE1(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 10, 10))
	src.Set(5, 5, color.RGBA{R: 255, G: 0, B: 0, A: 255})
	if got := Restore(src, 1); got != src {
		t.Fatalf("num=1 时应原样返回")
	}
}

// TestRestoreVerticalFlip 构造已知图像，验证 num=2 时的垂直翻转。
func TestRestoreVerticalFlip(t *testing.T) {
	w, h := 4, 4
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if y < h/2 {
				src.Set(x, y, color.RGBA{R: 255, A: 255})
			} else {
				src.Set(x, y, color.RGBA{G: 255, A: 255})
			}
		}
	}
	// num=2，h%num=0：垂直翻转 → 上半绿、下半红
	dst := Restore(src, 2)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, _, _ := dst.At(x, y).RGBA()
			if y < h/2 {
				if r != 0 || g == 0 {
					t.Fatalf("y=%d 应为绿色，得到 r=%d g=%d", y, r, g)
				}
			} else {
				if r == 0 || g != 0 {
					t.Fatalf("y=%d 应为红色，得到 r=%d g=%d", y, r, g)
				}
			}
		}
	}
}

// TestParseScrambleID 验证 scramble_id 字符串解析。
func TestParseScrambleID(t *testing.T) {
	cases := []struct{ in string; want int64 }{
		{"", 0},
		{"421926", 421926},
		{"abc", 0},
		{"999999", 999999},
	}
	for _, c := range cases {
		if got := parseScrambleID(c.in); got != c.want {
			t.Fatalf("parseScrambleID(%q)=%d want=%d", c.in, got, c.want)
		}
	}
}
