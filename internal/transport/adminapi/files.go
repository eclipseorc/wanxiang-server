// files.go 提供读取设备文件的接口：列目录、下载文件、生成图片缩略图。
//
// 与远程控制不同，文件读写是「按需的一次性操作」，因此放在 admin-server 上，
// 复用现有的管理员认证（requireAdmin）与审计，避免破坏 scrcpy-server 的
// 单一职责（实时视频/控制流桥接）。底层复用 scrcpy.ADB 的 Shell / Pull。
package adminapi

import (
	"context"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

// thumbMaxSide 是缩略图最长边的像素上限。原图按此等比缩小，避免整张原图
// 直接下发导致文件列表加载变慢。标准库不支持的格式（webp 等）不缩略，
// 由前端直接显示原图。
const thumbMaxSide = 256

// handleFilesList 列出设备上指定目录的内容。
// GET /api/admin/files/list?serial=<ip:port>&path=<dir>
func (r *Router) handleFilesList(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if r.scrcpy == nil {
		fail(w, http.StatusServiceUnavailable, "scrcpy not configured")
		return
	}
	serial := strings.TrimSpace(req.URL.Query().Get("serial"))
	path := strings.TrimSpace(req.URL.Query().Get("path"))
	if serial == "" {
		fail(w, http.StatusBadRequest, "serial is required")
		return
	}
	if path == "" {
		path = "/sdcard/"
	}
	// 路径限定在设备存储内，避免误触系统敏感目录；管理员仍可手动输入其它路径。
	if !safeDevicePath(path) {
		fail(w, http.StatusBadRequest, "invalid path")
		return
	}
	files, err := r.scrcpy.ListFiles(req.Context(), serial, path)
	if err != nil {
		fail(w, http.StatusBadRequest, friendlyAdbError(err))
		return
	}
	_ = r.svc.Store.Audit(u.ID, "files-list", serial, path)
	writeJSON(w, http.StatusOK, map[string]any{"path": path, "files": files})
}

// handleFilesDownload 拉取设备上的单个文件并以附件流下发到浏览器。
// GET /api/admin/files/download?serial=<ip:port>&path=<file>
func (r *Router) handleFilesDownload(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if r.scrcpy == nil {
		fail(w, http.StatusServiceUnavailable, "scrcpy not configured")
		return
	}
	serial := strings.TrimSpace(req.URL.Query().Get("serial"))
	path := strings.TrimSpace(req.URL.Query().Get("path"))
	if serial == "" || path == "" {
		fail(w, http.StatusBadRequest, "serial and path are required")
		return
	}
	if !safeDevicePath(path) {
		fail(w, http.StatusBadRequest, "invalid path")
		return
	}

	// 先用 stat 拿文件大小，以便在拉取前就把响应头（含 Content-Length）发给浏览器。
	// 大文件经 Wi-Fi 拉取可能耗时十几秒，若不提前下发响应头，浏览器会在长时间
	// 收不到任何字节后重置连接（表现为 fetch 报 "Failed to fetch"）。
	size, err := r.deviceFileSize(req.Context(), serial, path)
	if err != nil {
		fail(w, http.StatusBadRequest, friendlyAdbError(err))
		return
	}

	name := filepath.Base(path)
	w.Header().Set("Content-Type", contentType(name))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
	// 提前 flush 响应头，让浏览器立即确认连接正常，随后耐心等待 body。
	w.WriteHeader(http.StatusOK)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}

	// 边拉边写：通过 exec-out 直接把设备文件内容流式写入响应体，从第一条字节起
	// 就有数据产出，避免大文件「响应头已发但 body 迟迟不来」导致浏览器断连。
	// 不再落盘、不再一次性 ReadFile，内存占用恒定。
	_ = r.svc.Store.Audit(u.ID, "files-download", serial, path)
	if err := r.scrcpy.PullStream(req.Context(), serial, path, w); err != nil {
		// 响应头已发、状态码无法再改；仅记录日志，浏览器会得到截断文件并报错。
		// adb 无线连接偶发抖动，这里不再重试（流式无法安全重试，重试会污染已写数据）。
		log.Printf("adminapi files download stream failed serial=%s path=%s: %v", serial, path, err)
	}
}

// handleFilesThumb 生成图片缩略图。
// GET /api/admin/files/thumb?serial=<ip:port>&path=<image>
//
// 标准库支持的 jpeg/png/gif 会降采样到 thumbMaxSide 以内；webp/bmp 等
// 不支持解码的格式直接返回原图（由前端 CSS 缩小显示），不引入第三方解码依赖。
func (r *Router) handleFilesThumb(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if r.scrcpy == nil {
		fail(w, http.StatusServiceUnavailable, "scrcpy not configured")
		return
	}
	serial := strings.TrimSpace(req.URL.Query().Get("serial"))
	path := strings.TrimSpace(req.URL.Query().Get("path"))
	if serial == "" || path == "" {
		fail(w, http.StatusBadRequest, "serial and path are required")
		return
	}
	if !safeDevicePath(path) {
		fail(w, http.StatusBadRequest, "invalid path")
		return
	}

	tmp, err := os.MkdirTemp("", "wanxiang-thumb-")
	if err != nil {
		failInternal(w, "files thumb tempdir", err)
		return
	}
	defer os.RemoveAll(tmp)

	local := filepath.Join(tmp, filepath.Base(path))
	if err := r.scrcpy.Pull(req.Context(), serial, path, local); err != nil {
		fail(w, http.StatusBadRequest, friendlyAdbError(err))
		return
	}

	// 读取并缩略；解码失败（非图片或损坏）返回 415。
	thumb, ctype, err := makeThumb(local)
	if err != nil {
		fail(w, http.StatusUnsupportedMediaType, "unsupported image")
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(thumb)
}

// makeThumb 读取本地图片文件并返回降采样后的字节与 Content-Type。
// 无法解码的格式返回错误（由调用方回退或报错）。
func makeThumb(local string) ([]byte, string, error) {
	f, err := os.Open(local)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()

	img, kind, err := image.Decode(f)
	if err != nil {
		return nil, "", err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, "", fmt.Errorf("empty image")
	}
	// 等比缩放，使最长边不超过 thumbMaxSide。
	if w > h {
		h = h * thumbMaxSide / w
		w = thumbMaxSide
	} else {
		w = w * thumbMaxSide / h
		h = thumbMaxSide
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}

	// 用最近邻缩放即可满足缩略图需求，避免引入 x/image/draw 依赖。
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			sx := x * b.Dx() / w
			sy := y * b.Dy() / h
			dst.Set(x, y, img.At(b.Min.X+sx, b.Min.Y+sy))
		}
	}

	var buf strings.Builder
	switch kind {
	case "png":
		_ = png.Encode(&buf, dst)
		return []byte(buf.String()), "image/png", nil
	case "gif":
		_ = gif.Encode(&buf, dst, nil)
		return []byte(buf.String()), "image/gif", nil
	default:
		_ = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 75})
		return []byte(buf.String()), "image/jpeg", nil
	}
}

// safeDevicePath 校验路径是否安全：必须是绝对路径，且不允许包含会逃逸出
// 存储目录的 .. 段。允许管理员浏览任意设备路径，但拒绝明显恶意的输入。
func safeDevicePath(path string) bool {
	if path == "" || !strings.HasPrefix(path, "/") {
		return false
	}
	if strings.ContainsRune(path, 0) {
		return false
	}
	parts := strings.Split(path, "/")
	for _, p := range parts {
		if p == ".." {
			return false
		}
	}
	return true
}

// deviceFileSize 通过 adb shell stat 读取设备上单个文件的大小（字节）。
// 用于在拉取大文件前先把 Content-Length 下发，避免浏览器长时间等不到
// 响应而重置连接。
func (r *Router) deviceFileSize(ctx context.Context, serial, path string) (int64, error) {
	if r.scrcpy == nil {
		return 0, fmt.Errorf("scrcpy not configured")
	}
	// path 已通过 safeDevicePath 校验（绝对路径、无 .. 无 NUL）；用单引号包裹
	// 避免含空格的文件名被拆词。若路径本身含单引号则放弃包裹（宁可失败也不注入）。
	quoted := path
	if !strings.Contains(path, "'") {
		quoted = "'" + path + "'"
	}
	out, err := r.scrcpy.Shell(ctx, serial, "stat -c %s "+quoted)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("stat parse failed: %q", strings.TrimSpace(out))
	}
	return n, nil
}

// contentType 根据扩展名返回常见类型，下载时用于设置响应头。
func contentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".pdf":
		return "application/pdf"
	default:
		return "application/octet-stream"
	}
}
