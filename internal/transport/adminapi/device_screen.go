// device_screen.go 提供卡片模式下的轻量级设备实时画面缩略图接口（GET /api/admin/device/screen）。
// 通过 adb exec-out screencap -p 抓屏并压缩为 240px 宽度的 JPEG（约 15~25KB），
// 内置 2 秒内存缓存防抖，避免多标签页或高频轮询压垮 ADB，且与 Scrcpy 控屏会话零冲突。
package adminapi

import (
	"bytes"
	"image"
	"image/jpeg"
	_ "image/png"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

type cachedScreen struct {
	data        []byte
	contentType string
	at          time.Time
}

var (
	screenCacheMu sync.Mutex
	screenCache   = make(map[string]cachedScreen)
)

const (
	screenCacheTTL     = 2 * time.Second
	screenThumbMaxW    = 240
	screenThumbQuality = 78
)

func (r *Router) handleDeviceScreen(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	if r.scrcpy == nil {
		fail(w, http.StatusServiceUnavailable, "adb not configured")
		return
	}
	deviceID := strings.TrimSpace(req.URL.Query().Get("deviceId"))
	if deviceID == "" {
		fail(w, http.StatusBadRequest, "deviceId is required")
		return
	}
	if u.Role != "admin" {
		ok, err := r.svc.Store.CanAccess(u.ID, deviceID, false)
		if err != nil || !ok {
			fail(w, http.StatusForbidden, "forbidden")
			return
		}
	}

	dev, err := r.svc.Store.DeviceByID(deviceID)
	if err != nil {
		fail(w, http.StatusNotFound, "device not found")
		return
	}
	if !dev.Online || !dev.AdbOnline || strings.TrimSpace(dev.AdbIP) == "" {
		fail(w, http.StatusConflict, "device adb offline")
		return
	}
	port := dev.AdbPort
	if port <= 0 {
		port = 5555
	}
	serial := strings.TrimSpace(dev.AdbIP) + ":" + strconv.Itoa(port)

	// 命中 2 秒防抖缓存直接返回
	screenCacheMu.Lock()
	if hit, ok := screenCache[serial]; ok && time.Since(hit.at) < screenCacheTTL {
		screenCacheMu.Unlock()
		w.Header().Set("Content-Type", hit.contentType)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(hit.data)
		return
	}
	screenCacheMu.Unlock()

	rawPNG, err := r.scrcpy.ScreencapBytes(req.Context(), serial)
	if err != nil {
		fail(w, http.StatusBadGateway, friendlyAdbError(err))
		return
	}

	outData, contentType := compressScreenThumb(rawPNG)

	screenCacheMu.Lock()
	screenCache[serial] = cachedScreen{data: outData, contentType: contentType, at: time.Now()}
	screenCacheMu.Unlock()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(outData)
}

// compressScreenThumb 把原始全分辨率 PNG 缩放到 240px 宽并编码为 JPEG，
// 把单帧流量从 ~500KB 降到 ~18KB；解码失败时原样回退原始 PNG。
func compressScreenThumb(rawPNG []byte) ([]byte, string) {
	src, _, err := image.Decode(bytes.NewReader(rawPNG))
	if err != nil {
		return rawPNG, "image/png"
	}
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return rawPNG, "image/png"
	}

	dstImg := src
	if sw > screenThumbMaxW {
		dw := screenThumbMaxW
		dh := sh * dw / sw
		if dh < 1 {
			dh = 1
		}
		dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
		for y := 0; y < dh; y++ {
			sy := b.Min.Y + y*sh/dh
			for x := 0; x < dw; x++ {
				sx := b.Min.X + x*sw/dw
				dst.Set(x, y, src.At(sx, sy))
			}
		}
		dstImg = dst
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dstImg, &jpeg.Options{Quality: screenThumbQuality}); err != nil {
		return rawPNG, "image/png"
	}
	return buf.Bytes(), "image/jpeg"
}
