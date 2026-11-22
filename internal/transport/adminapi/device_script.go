// device_script.go 提供「设备自动配对」机型脚本的管理接口：脚本 CRUD 与
// 「已上报型号列表」查询（供前端做型号下拉选择，避免管理员手打型号出错）。
//
// 设计约定：
//   - 机型脚本是「设备端自动配对流程」的配置：一份脚本描述一台机型的动作序列
//     （打开无线调试、点开关、读配对码等），由服务端经 WebSocket 下发给设备端
//     通用引擎执行。加新机型 = 加一份脚本，不改 APK。
//   - 匹配用「厂商+型号」字符串：设备端上报 Build.MANUFACTURER+" "+Build.MODEL
//     （已落库 devices.model），脚本 matchPatterns 里写候选串，命中即匹配。
//   - 本文件只负责脚本数据的读写与「已上报型号」的汇总；真正把脚本推给设备的
//     动作由 websocket 转发进程完成（见 websocket 包，与 mdm-policy 同链路）。
package adminapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

// deviceScriptReq 是新增/更新机型脚本的入参。
type deviceScriptReq struct {
	Name          string   `json:"name"`
	MatchPatterns []string `json:"matchPatterns"`
	Steps         []any    `json:"steps"`
}

// handleDeviceScripts 处理 GET /api/admin/device-scripts（列出全部脚本）与
// POST /api/admin/device-scripts（新增/更新一条脚本，带 id 表示更新）。
func (r *Router) handleDeviceScripts(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireAdmin(w, u) {
		return
	}
	switch req.Method {
	case http.MethodGet:
		scripts, err := r.svc.Store.ListDeviceModelScripts()
		if err != nil {
			failInternal(w, "list device model scripts", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"scripts": scripts})
	case http.MethodPost:
		r.upsertDeviceScript(w, req, u)
	default:
		requireMethod(w, req, http.MethodGet, http.MethodPost)
	}
}

// upsertDeviceScript 写入一条脚本：name + matchPatterns(字符串数组) + steps(动作数组)。
// steps 会重新序列化为 JSON 存 steps_json；带 id 时覆盖并自增 version。
func (r *Router) upsertDeviceScript(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	var body struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		MatchPatterns []string `json:"matchPatterns"`
		Steps         []any    `json:"steps"`
	}
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || len(body.MatchPatterns) == 0 {
		fail(w, http.StatusBadRequest, "name and matchPatterns are required")
		return
	}
	// 规范化匹配串：去首尾空白、去空串。
	patterns := make([]string, 0, len(body.MatchPatterns))
	for _, p := range body.MatchPatterns {
		p = strings.TrimSpace(p)
		if p != "" {
			patterns = append(patterns, p)
		}
	}
	if len(patterns) == 0 {
		fail(w, http.StatusBadRequest, "matchPatterns must not be empty")
		return
	}
	if body.Steps == nil {
		body.Steps = []any{}
	}

	matchJSON, err := json.Marshal(patterns)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid matchPatterns")
		return
	}
	stepsJSON, err := json.Marshal(body.Steps)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid steps")
		return
	}

	id := strings.TrimSpace(body.ID)
	if id == "" {
		// 新建：生成脚本 ID（以可读前缀开头，便于后台识别）。
		id, err = randomID()
		if err != nil {
			failInternal(w, "generate script id", err)
			return
		}
	}

	sc := sqlite.DeviceModelScript{
		ID:            id,
		Name:          body.Name,
		MatchPatterns: string(matchJSON),
		StepsJSON:     string(stepsJSON),
	}
	if err := r.svc.Store.UpsertDeviceModelScript(sc); err != nil {
		failInternal(w, "upsert device model script", err)
		return
	}
	_ = r.svc.Store.Audit(u.ID, "device-script-upsert", "", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// handleDeviceScriptDelete 处理 DELETE /api/admin/device-scripts/{id}。
func (r *Router) handleDeviceScriptDelete(w http.ResponseWriter, req *http.Request, u sqlite.User, id string) {
	if !requireMethod(w, req, http.MethodDelete) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if err := r.svc.Store.DeleteDeviceModelScript(id); err != nil {
		if err == sqlite.ErrNotFound {
			fail(w, http.StatusNotFound, "script not found")
			return
		}
		failInternal(w, "delete device model script", err)
		return
	}
	_ = r.svc.Store.Audit(u.ID, "device-script-delete", "", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleDeviceModelOptions 处理 GET /api/admin/device-models：返回所有设备上报过的
// 型号去重列表（含设备数），供「机型脚本」编辑页做型号下拉选择。数据源就是
// devices.model（设备自己上报的 Build.MANUFACTURER+" "+Build.MODEL），与脚本匹配
// 用的是同一个字段，保证「管理员填的 = 系统显示的 = 设备上报的」，无出入。
func (r *Router) handleDeviceModelOptions(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	models, err := r.svc.Store.DistinctDeviceModels()
	if err != nil {
		failInternal(w, "list device models", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}
