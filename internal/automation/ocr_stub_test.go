package automation

import (
	"context"
	"testing"
)

// TestOCRStub 验证未启用 OCR 时,NewTesseractOCR 返回的占位实现会明确报错,
// 而不是静默失败或 panic。
func TestOCRStub(t *testing.T) {
	ocr := NewTesseractOCR("chi_sim+eng", "")
	if ocr == nil {
		t.Fatal("NewTesseractOCR should never return nil")
	}
	_, _, found, err := ocr.Recognize(context.Background(), "/tmp/x.png", "登录")
	if err == nil {
		t.Fatal("stub OCR should return an error when OCR is not enabled")
	}
	if found {
		t.Fatal("stub OCR should never report found=true")
	}
}
