package adminapi

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// safeDevicePath 应拒绝空路径、相对路径、含 .. 逃逸段和 NUL 字节的输入。
func TestSafeDevicePath(t *testing.T) {
	ok := []string{"/sdcard/", "/sdcard/DCIM", "/storage/emulated/0/Download/a b.txt"}
	for _, p := range ok {
		if !safeDevicePath(p) {
			t.Fatalf("safeDevicePath(%q) = false, want true", p)
		}
	}
	bad := []string{"", "sdcard", "relative/path", "/sdcard/../etc", "/a/../../b", "/a\x00b"}
	for _, p := range bad {
		if safeDevicePath(p) {
			t.Fatalf("safeDevicePath(%q) = true, want false", p)
		}
	}
}

// makeThumb 应能把一张较大的 PNG 缩到 thumbMaxSide 以内并返回 image/png。
func TestMakeThumb(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "big.png")

	// 生成 512x256 的纯色 PNG。
	img := image.NewRGBA(image.Rect(0, 0, 512, 256))
	for y := 0; y < 256; y++ {
		for x := 0; x < 512; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 30, B: 30, A: 255})
		}
	}
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	data, ctype, err := makeThumb(src)
	if err != nil {
		t.Fatalf("makeThumb: %v", err)
	}
	if ctype != "image/png" {
		t.Fatalf("ctype = %q, want image/png", ctype)
	}
	out, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode thumb: %v", err)
	}
	if out.Bounds().Dx() > thumbMaxSide || out.Bounds().Dy() > thumbMaxSide {
		t.Fatalf("thumb size %dx%d exceeds %d", out.Bounds().Dx(), out.Bounds().Dy(), thumbMaxSide)
	}
	if out.Bounds().Dx() != thumbMaxSide || out.Bounds().Dy() != thumbMaxSide/2 {
		t.Fatalf("thumb size = %dx%d, want %dx%d", out.Bounds().Dx(), out.Bounds().Dy(), thumbMaxSide, thumbMaxSide/2)
	}
}
