// apk.go 提供 APK 批量安装/卸载的接口：上传 APK（解析包名）、并发安装到
// 多台设备、并发卸载、查询已上传 APK 列表。
//
// 设计约定：
//   - APK 上传后存入进程临时目录（用户已确认不持久化，重启即失效），
//     元数据（apkId / 文件名 / 包名 / 大小 / 上传时间）存内存 map，与临时
//     目录同生命周期。
//   - 批量安装/卸载复用设备级 serial 约定（adbIp:adbPort，端口动态分配），
//     并发执行并逐台返回成功/失败，前端据此展示进度。
//   - 包名通过纯 Go 解析 APK 内 AndroidManifest.xml（AXML）得到，不依赖
//     外部 aapt；卸载既可手填包名，也可从已上传 APK 列表选包名。
package adminapi

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/scrcpy"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

// maxAPKSize 限制单个 APK 上传大小（256MB），防止异常大文件耗尽磁盘/内存。
const maxAPKSize = 256 << 20

// apkRecord 记录一个已上传 APK 的元数据，供「卸载时从列表选包名」与「安装」使用。
type apkRecord struct {
	ID          string `json:"id"`
	FileName    string `json:"fileName"`
	PackageName string `json:"packageName"`
	Size        int64  `json:"size"`
	Path        string `json:"-"` // 临时目录中的实际路径，不下发
	UploadedAt  int64  `json:"uploadedAt"`
}

// apkStore 进程内维护已上传 APK（内存态，与临时目录同生命周期）。
type apkStore struct {
	mu      sync.RWMutex
	records map[string]*apkRecord
}

func newAPKStore() *apkStore {
	return &apkStore{records: make(map[string]*apkRecord)}
}

func (s *apkStore) put(r *apkRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[r.ID] = r
}

func (s *apkStore) get(id string) (*apkRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.records[id]
	return r, ok
}

func (s *apkStore) list() []*apkRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*apkRecord, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, r)
	}
	return out
}

// handleAPKUpload 接收 multipart 上传的 .apk，存入临时目录并解析包名。
// POST /api/admin/apk/upload   （multipart，字段名 file）
func (r *Router) handleAPKUpload(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if r.scrcpy == nil {
		fail(w, http.StatusServiceUnavailable, "scrcpy not configured")
		return
	}
	// 限制请求体大小（APK + multipart 开销）。
	req.Body = http.MaxBytesReader(w, req.Body, maxAPKSize+1<<20)
	if err := req.ParseMultipartForm(maxAPKSize); err != nil {
		fail(w, http.StatusBadRequest, "上传失败：文件过大或格式错误")
		return
	}
	file, header, err := req.FormFile("file")
	if err != nil {
		fail(w, http.StatusBadRequest, "缺少文件字段 file")
		return
	}
	defer file.Close()

	fileName := filepath.Base(header.Filename)
	if !strings.HasSuffix(strings.ToLower(fileName), ".apk") {
		fail(w, http.StatusBadRequest, "仅支持 .apk 文件")
		return
	}

	// 读入内存解析包名（APK 受 maxAPKSize 限制，几十 MB 内存可接受）。
	data, err := io.ReadAll(file)
	if err != nil {
		fail(w, http.StatusBadRequest, "读取上传内容失败："+err.Error())
		return
	}
	pkg, err := scrcpy.APKPackageName(data)
	if err != nil {
		fail(w, http.StatusBadRequest, "解析 APK 包名失败："+err.Error())
		return
	}

	// 写入临时目录。
	tmpDir, err := os.MkdirTemp("", "wanxiang-apk-")
	if err != nil {
		failInternal(w, "apk upload tempdir", err)
		return
	}
	local := filepath.Join(tmpDir, fileName)
	if err := os.WriteFile(local, data, 0o644); err != nil {
		_ = os.RemoveAll(tmpDir)
		failInternal(w, "apk upload write", err)
		return
	}

	rec := &apkRecord{
		ID:          newAPKID(),
		FileName:    fileName,
		PackageName: pkg,
		Size:        int64(len(data)),
		Path:        local,
		UploadedAt:  time.Now().Unix(),
	}
	r.apks.put(rec)

	_ = r.svc.Store.Audit(u.ID, "apk-upload", pkg, fileName)
	writeJSON(w, http.StatusOK, rec)
}

// handleAPKList 返回已上传 APK 列表（含包名），供前端安装/卸载选择。
// GET /api/admin/apk/list
func (r *Router) handleAPKList(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"apks": r.apks.list()})
}

// handleAPKInstall 把已上传的 APK 并发安装到多台设备。
// POST /api/admin/apk/install   { apkId, deviceIds: [] }
func (r *Router) handleAPKInstall(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if r.scrcpy == nil {
		fail(w, http.StatusServiceUnavailable, "scrcpy not configured")
		return
	}
	var body struct {
		APKID     string   `json:"apkId"`
		DeviceIDs []string `json:"deviceIds"`
	}
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.APKID = strings.TrimSpace(body.APKID)
	if body.APKID == "" || len(body.DeviceIDs) == 0 {
		fail(w, http.StatusBadRequest, "apkId and deviceIds are required")
		return
	}
	rec, ok := r.apks.get(body.APKID)
	if !ok {
		fail(w, http.StatusNotFound, "APK 不存在或已失效（服务重启后需重新上传）")
		return
	}

	results := r.runOnDevices(req, body.DeviceIDs, func(serial string) error {
		return r.scrcpy.Install(req.Context(), serial, rec.Path)
	})

	_ = r.svc.Store.Audit(u.ID, "apk-install", rec.PackageName, fmt.Sprintf("%d devices", len(body.DeviceIDs)))
	writeJSON(w, http.StatusOK, map[string]any{
		"apkId":   rec.ID,
		"package": rec.PackageName,
		"results": results,
	})
}

// handleAPKUninstall 把指定包名并发卸载到多台设备。
// POST /api/admin/apk/uninstall   { packageName, deviceIds: [] }
func (r *Router) handleAPKUninstall(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if r.scrcpy == nil {
		fail(w, http.StatusServiceUnavailable, "scrcpy not configured")
		return
	}
	var body struct {
		PackageName string   `json:"packageName"`
		DeviceIDs   []string `json:"deviceIds"`
	}
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.PackageName = strings.TrimSpace(body.PackageName)
	if body.PackageName == "" || len(body.DeviceIDs) == 0 {
		fail(w, http.StatusBadRequest, "packageName and deviceIds are required")
		return
	}

	results := r.runOnDevices(req, body.DeviceIDs, func(serial string) error {
		return r.scrcpy.Uninstall(req.Context(), serial, body.PackageName)
	})

	_ = r.svc.Store.Audit(u.ID, "apk-uninstall", body.PackageName, fmt.Sprintf("%d devices", len(body.DeviceIDs)))
	writeJSON(w, http.StatusOK, map[string]any{
		"package": body.PackageName,
		"results": results,
	})
}

// deviceResult 是单台设备的操作结果。
type deviceResult struct {
	DeviceID string `json:"deviceId"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// runOnDevices 并发对多台设备执行 fn，返回逐台结果（顺序与 deviceIDs 一致）。
// serial 由设备落库的 adb_ip + adb_port 拼出（无线调试端口动态分配，不能假设
// 5555）；端口缺失（旧数据）时兜底 5555。无法取得 serial 的设备直接判失败。
func (r *Router) runOnDevices(req *http.Request, deviceIDs []string, fn func(serial string) error) []deviceResult {
	results := make([]deviceResult, len(deviceIDs))
	var wg sync.WaitGroup
	for i, id := range deviceIDs {
		wg.Add(1)
		go func(idx int, deviceID string) {
			defer wg.Done()
			deviceID = strings.TrimSpace(deviceID)
			res := deviceResult{DeviceID: deviceID}
			if deviceID == "" {
				res.Error = "deviceId 为空"
				results[idx] = res
				return
			}
			dev, err := r.svc.Store.DeviceByID(deviceID)
			if err != nil {
				res.Error = "设备不存在：" + err.Error()
				results[idx] = res
				return
			}
			if dev.AdbIP == "" {
				res.Error = "设备未连接 adb（无 IP）"
				results[idx] = res
				return
			}
			port := dev.AdbPort
			if port <= 0 {
				port = 5555 // 旧数据无端口，兜底传统模式端口
			}
			serial := fmt.Sprintf("%s:%d", dev.AdbIP, port)
			if err := fn(serial); err != nil {
				res.Error = friendlyAdbError(err)
				results[idx] = res
				return
			}
			res.OK = true
			results[idx] = res
		}(i, id)
	}
	wg.Wait()
	return results
}

// newAPKID 生成随机的 apkId（hex 编码的随机字节）。
func newAPKID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极端情况下回退到时间戳，保证不为空。
		return fmt.Sprintf("apk-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
