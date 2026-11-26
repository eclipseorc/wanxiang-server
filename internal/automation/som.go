package automation

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"strconv"
	"strings"
)

// SoMItem 表示画面上标注的一个可交互元素编号及其在原手机屏幕上的真实中心坐标。
type SoMItem struct {
	ID      int    `json:"id"`
	Text    string `json:"text"`
	Desc    string `json:"desc"`
	ResID   string `json:"resId"`
	CenterX int    `json:"centerX"`
	CenterY int    `json:"centerY"`
}

// SoMResult 是 Set-of-Mark 视觉打标后的结果（压缩标注图 + 编号坐标映射表）。
type SoMResult struct {
	JPEG  []byte
	OrigW int
	OrigH int
	Items []SoMItem
}

const (
	somMaxWidth = 420
	somMaxItems = 60
	somQuality  = 75
)

// 5x7 像素点阵数字字模（0-9），零外部字体依赖，放大 2 倍绘制后在截屏上清晰锐利。
var digitGlyphs = [10][7]uint8{
	{0x0E, 0x11, 0x13, 0x15, 0x19, 0x11, 0x0E}, // 0
	{0x04, 0x0C, 0x04, 0x04, 0x04, 0x04, 0x0E}, // 1
	{0x0E, 0x11, 0x01, 0x02, 0x04, 0x08, 0x1F}, // 2
	{0x1F, 0x02, 0x04, 0x02, 0x01, 0x11, 0x0E}, // 3
	{0x02, 0x06, 0x0A, 0x12, 0x1F, 0x02, 0x02}, // 4
	{0x1F, 0x10, 0x1E, 0x01, 0x01, 0x11, 0x0E}, // 5
	{0x06, 0x08, 0x10, 0x1E, 0x11, 0x11, 0x0E}, // 6
	{0x1F, 0x01, 0x02, 0x04, 0x08, 0x08, 0x08}, // 7
	{0x0E, 0x11, 0x11, 0x0E, 0x11, 0x11, 0x0E}, // 8
	{0x0E, 0x11, 0x11, 0x0F, 0x01, 0x02, 0x0C}, // 9
}

// CompressScreenshotForVision 将手机原始截图等比缩放到 540px 宽 JPEG（用于纯净视觉模式）。
func CompressScreenshotForVision(rawPNG []byte) ([]byte, int, int, error) {
	src, _, err := image.Decode(bytes.NewReader(rawPNG))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decode screenshot: %w", err)
	}
	b := src.Bounds()
	origW, origH := b.Dx(), b.Dy()
	if origW <= 0 || origH <= 0 {
		return nil, 0, 0, fmt.Errorf("invalid screenshot bounds")
	}
	dst := resizeRGBA(src, somMaxWidth)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: somQuality}); err != nil {
		return nil, origW, origH, err
	}
	return buf.Bytes(), origW, origH, nil
}

// AnnotateSoM 将手机截图缩放到 540px 宽并在每个可交互控件上绘制红框与 [1][2][3] 编号角标。
// 这样视觉大模型只需看图输出编号 targetId，无需手猜像素坐标，点击准确率可提升至 90%~95%+。
func AnnotateSoM(rawPNG []byte, nodes []UINode) (*SoMResult, error) {
	src, _, err := image.Decode(bytes.NewReader(rawPNG))
	if err != nil {
		return nil, fmt.Errorf("decode screenshot: %w", err)
	}
	b := src.Bounds()
	origW, origH := b.Dx(), b.Dy()
	if origW <= 0 || origH <= 0 {
		return nil, fmt.Errorf("invalid screenshot bounds")
	}

	canvas := resizeRGBA(src, somMaxWidth)
	cb := canvas.Bounds()
	scaleX := float64(cb.Dx()) / float64(origW)
	scaleY := float64(cb.Dy()) / float64(origH)

	screenArea := origW * origH
	items := make([]SoMItem, 0, 32)
	boxColor := color.RGBA{R: 239, G: 68, B: 68, A: 255}  // 醒目红框
	tagBg := color.RGBA{R: 220, G: 38, B: 38, A: 255}     // 编号红底
	tagFg := color.RGBA{R: 255, G: 255, B: 255, A: 255}   // 编号白字

	for _, n := range nodes {
		if len(items) >= somMaxItems {
			break
		}
		l, t, r, bot := n.Bounds[0], n.Bounds[1], n.Bounds[2], n.Bounds[3]
		w, h := r-l, bot-t
		if w < 12 || h < 12 {
			continue
		}
		// 跳过占满全屏的背景容器（面积 > 80%）
		if screenArea > 0 && w*h*100/screenArea > 80 {
			continue
		}
		// 优先保留可点击元素或带清晰文本/描述的节点
		txt := strings.TrimSpace(n.Text)
		desc := strings.TrimSpace(n.ContentDesc)
		if !n.Clickable && txt == "" && desc == "" {
			continue
		}

		id := len(items) + 1
		items = append(items, SoMItem{
			ID:      id,
			Text:    txt,
			Desc:    desc,
			ResID:   n.ResourceID,
			CenterX: n.CenterX,
			CenterY: n.CenterY,
		})

		// 映射到缩放后的画布坐标并绘制 2px 边框 + 左上角数字角标
		x1 := int(float64(l) * scaleX)
		y1 := int(float64(t) * scaleY)
		x2 := int(float64(r) * scaleX)
		y2 := int(float64(bot) * scaleY)
		drawRectBorder(canvas, x1, y1, x2, y2, 2, boxColor)
		drawNumberBadge(canvas, x1, y1, id, tagBg, tagFg)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: somQuality}); err != nil {
		return nil, err
	}
	return &SoMResult{
		JPEG:  buf.Bytes(),
		OrigW: origW,
		OrigH: origH,
		Items: items,
	}, nil
}

func resizeRGBA(src image.Image, maxW int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dw, dh := sw, sh
	if sw > maxW && maxW > 0 {
		dw = maxW
		dh = sh * dw / sw
		if dh < 1 {
			dh = 1
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	if dw == sw && dh == sh {
		draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
		return dst
	}
	for y := 0; y < dh; y++ {
		sy := b.Min.Y + y*sh/dh
		for x := 0; x < dw; x++ {
			sx := b.Min.X + x*sw/dw
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}

func drawRectBorder(img *image.RGBA, x1, y1, x2, y2, thickness int, c color.RGBA) {
	b := img.Bounds()
	if x1 < b.Min.X {
		x1 = b.Min.X
	}
	if y1 < b.Min.Y {
		y1 = b.Min.Y
	}
	if x2 >= b.Max.X {
		x2 = b.Max.X - 1
	}
	if y2 >= b.Max.Y {
		y2 = b.Max.Y - 1
	}
	for t := 0; t < thickness; t++ {
		for x := x1; x <= x2; x++ {
			img.SetRGBA(x, clampInt(y1+t, b.Min.Y, b.Max.Y-1), c)
			img.SetRGBA(x, clampInt(y2-t, b.Min.Y, b.Max.Y-1), c)
		}
		for y := y1; y <= y2; y++ {
			img.SetRGBA(clampInt(x1+t, b.Min.X, b.Max.X-1), y, c)
			img.SetRGBA(clampInt(x2-t, b.Min.X, b.Max.X-1), y, c)
		}
	}
}

func drawNumberBadge(img *image.RGBA, x, y, num int, bg, fg color.RGBA) {
	s := strconv.Itoa(num)
	scale := 2
	charW := 5 * scale
	charH := 7 * scale
	gap := 2
	padX, padY := 4, 3
	badgeW := len(s)*charW + (len(s)-1)*gap + padX*2
	badgeH := charH + padY*2

	b := img.Bounds()
	if x+badgeW >= b.Max.X {
		x = b.Max.X - badgeW - 1
	}
	if y+badgeH >= b.Max.Y {
		y = b.Max.Y - badgeH - 1
	}
	if x < b.Min.X {
		x = b.Min.X
	}
	if y < b.Min.Y {
		y = b.Min.Y
	}

	// 绘制背景块
	for py := y; py < y+badgeH; py++ {
		for px := x; px < x+badgeW; px++ {
			img.SetRGBA(px, py, bg)
		}
	}

	// 绘制点阵数字
	curX := x + padX
	for i := 0; i < len(s); i++ {
		d := s[i] - '0'
		if d > 9 {
			continue
		}
		glyph := digitGlyphs[d]
		for row := 0; row < 7; row++ {
			bits := glyph[row]
			for col := 0; col < 5; col++ {
				if (bits & (1 << (4 - col))) != 0 {
					for sy := 0; sy < scale; sy++ {
						for sx := 0; sx < scale; sx++ {
							img.SetRGBA(curX+col*scale+sx, y+padY+row*scale+sy, fg)
						}
					}
				}
			}
		}
		curX += charW + gap
	}
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
