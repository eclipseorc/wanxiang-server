//go:build ocr

package automation

import (
	"context"
	"fmt"
	"strings"

	"github.com/otiai10/gosseract/v2"
)

// TesseractOCR 用本地 Tesseract 引擎(经 gosseract 绑定)实现 OCRClient 接口。
// 它依赖系统已安装的 tesseract 二进制与对应语言包(中文需 chi_sim.traineddata),
// 与万象已有的 adb / scrcpy 属于同一量级的「系统依赖」,而非独立服务。
//
// 每次 Recognize 新建一个 client,执行完立即 Close,以保证在多设备并发执行
// (执行引擎里一个 job 会并发跑多台设备)时线程安全。代价是单次识别多一次
// 引擎初始化,对「截图定位文字」这类低频动作可接受。
type TesseractOCR struct {
	// langs 是识别语言,如 "chi_sim+eng"。为空时由实现取默认值。
	langs string
	// tessdataPrefix 是 tesseract 语言包目录;为空时用 gosseract 默认查找路径。
	tessdataPrefix string
}

// NewTesseractOCR 构造 Tesseract OCR 客户端。langs 如 "chi_sim+eng";
// tessdataPrefix 可空(用系统默认 tessdata 目录)。
func NewTesseractOCR(langs, tessdataPrefix string) *TesseractOCR {
	if strings.TrimSpace(langs) == "" {
		langs = "chi_sim+eng"
	}
	return &TesseractOCR{langs: langs, tessdataPrefix: strings.TrimSpace(tessdataPrefix)}
}

// Recognize 对 imagePath 做文字识别,在所有识别出的词框中查找 target,
// 命中则返回该词框的中心坐标。
//
// 匹配规则:先做完全相等(去空白后),再做包含匹配;命中取置信度最高者。
// 未命中返回 found=false。
func (t *TesseractOCR) Recognize(ctx context.Context, imagePath, target string) (x, y int, found bool, err error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return 0, 0, false, fmt.Errorf("ocr target text required")
	}

	client := gosseract.NewClient()
	defer client.Close()

	if err := client.SetLanguage(t.langs); err != nil {
		return 0, 0, false, fmt.Errorf("ocr set language %q: %w", t.langs, err)
	}
	if t.tessdataPrefix != "" {
		if err := client.SetTessdataPrefix(t.tessdataPrefix); err != nil {
			return 0, 0, false, fmt.Errorf("ocr set tessdata prefix: %w", err)
		}
	}
	if err := client.SetImage(imagePath); err != nil {
		return 0, 0, false, fmt.Errorf("ocr load image: %w", err)
	}

	boxes, err := client.GetBoundingBoxes(gosseract.RIL_WORD)
	if err != nil {
		return 0, 0, false, fmt.Errorf("ocr recognize: %w", err)
	}

	// 两轮匹配:先精确相等,再包含。保留置信度最高者。
	best := -1.0
	bestIdx := -1
	for i, b := range boxes {
		w := strings.TrimSpace(b.Word)
		if w == "" {
			continue
		}
		if w == target {
			if b.Confidence > best {
				best, bestIdx = b.Confidence, i
			}
		}
	}
	if bestIdx == -1 {
		for i, b := range boxes {
			w := strings.TrimSpace(b.Word)
			if w != "" && strings.Contains(w, target) {
				if b.Confidence > best {
					best, bestIdx = b.Confidence, i
				}
			}
		}
	}
	if bestIdx == -1 {
		return 0, 0, false, nil
	}

	b := boxes[bestIdx]
	cx := b.Box.Min.X + (b.Box.Max.X-b.Box.Min.X)/2
	cy := b.Box.Min.Y + (b.Box.Max.Y-b.Box.Min.Y)/2
	return cx, cy, true, nil
}
