//go:build !ocr

package automation

import (
	"context"
	"fmt"
)

// 本文件在未启用 OCR 编译标签(-tags ocr)时编译,提供 TesseractOCR 的
// 占位实现,使整个项目保持纯 Go 编译(无需 CGO 与 tesseract 工具链)。
// 需要 OCR 时,在装有 tesseract 开发库的环境用
// `CGO_ENABLED=1 go build -tags ocr` 构建,此时将改用 ocr.go 的 gosseract 实现。

// TesseractOCR 的占位类型(未编译 OCR 能力时使用)。
type TesseractOCR struct{}

// NewTesseractOCR 在未启用 OCR 时构造占位实现;参数被忽略。
func NewTesseractOCR(langs, tessdataPrefix string) *TesseractOCR {
	return &TesseractOCR{}
}

// Recognize 未启用 OCR 时始终返回明确错误,提示需要 -tags ocr 构建。
func (t *TesseractOCR) Recognize(ctx context.Context, imagePath, target string) (x, y int, found bool, err error) {
	return 0, 0, false, fmt.Errorf("OCR 未启用:请用 -tags ocr 构建并配置 tesseract(见 docs/automation-engine.md)")
}
