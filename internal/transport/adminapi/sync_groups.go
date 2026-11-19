package adminapi

import (
	"log"
	"net/http"
	"strings"

	"github.com/eclipseorc/wanxiang-server/internal/logic"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

// 固定镜像组接口：把「主控 + 成员设备」预存为可复用配置，一键镜像时下发。
// 成员用 device.id 绑定，与 adb serial（ip:真实端口）解耦，IP/端口变化不影响复用。
// 底层群控引擎（scrcpy sync.go）不变，固定组只是「预存编排 + 一键下发」的壳。

// handleSyncGroups 处理 GET /api/admin/sync-groups（列出）与 POST（创建）。
func (r *Router) handleSyncGroups(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	switch req.Method {
	case http.MethodGet:
		groups, err := r.svc.Store.ListSyncGroups()
		if err != nil {
			failInternal(w, "list sync groups", err)
			return
		}
		writeJSON(w, http.StatusOK, groups)
	case http.MethodPost:
		r.createSyncGroup(w, req, u)
	default:
		requireMethod(w, req, http.MethodGet, http.MethodPost)
	}
}

func (r *Router) createSyncGroup(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireAdmin(w, u) {
		return
	}
	var body struct {
		Name      string   `json:"name"`
		MasterID  string   `json:"masterId"`
		DeviceIDs []string `json:"deviceIds"`
	}
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		fail(w, http.StatusBadRequest, "name required")
		return
	}
	deviceIDs, err := normalizeSyncMembers(body.DeviceIDs)
	if err != nil {
		failInput(w, "create sync group", err)
		return
	}
	masterID := strings.TrimSpace(body.MasterID)
	if masterID == "" {
		masterID = deviceIDs[0]
	}
	if !contains(deviceIDs, masterID) {
		fail(w, http.StatusBadRequest, "masterId must be one of deviceIds")
		return
	}
	id, err := randomID()
	if err != nil {
		failInternal(w, "generate sync group id", err)
		return
	}
	if err := r.svc.Store.CreateSyncGroup(id, name, masterID, deviceIDs); err != nil {
		log.Printf("adminapi create sync group failed: %v", err)
		fail(w, http.StatusInternalServerError, "operation failed")
		return
	}
	_ = r.svc.Store.Audit(u.ID, "create-sync-group", "", id)
	writeJSON(w, http.StatusCreated, map[string]string{"groupId": id})
}

// handleSyncGroup 处理单个固定镜像组：PUT（更新）/ DELETE（删除）。
// 路径形如 /api/admin/sync-groups/{id}。
func (r *Router) handleSyncGroup(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	id := strings.TrimPrefix(req.URL.Path, "/api/admin/sync-groups/")
	if id == "" {
		http.NotFound(w, req)
		return
	}
	switch req.Method {
	case http.MethodPut:
		r.updateSyncGroup(w, req, u, id)
	case http.MethodDelete:
		r.deleteSyncGroup(w, req, u, id)
	default:
		requireMethod(w, req, http.MethodPut, http.MethodDelete)
	}
}

func (r *Router) updateSyncGroup(w http.ResponseWriter, req *http.Request, u sqlite.User, id string) {
	if !requireAdmin(w, u) {
		return
	}
	var body struct {
		Name      string   `json:"name"`
		MasterID  string   `json:"masterId"`
		DeviceIDs []string `json:"deviceIds"`
	}
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		fail(w, http.StatusBadRequest, "name required")
		return
	}
	deviceIDs, err := normalizeSyncMembers(body.DeviceIDs)
	if err != nil {
		failInput(w, "update sync group", err)
		return
	}
	masterID := strings.TrimSpace(body.MasterID)
	if masterID == "" {
		masterID = deviceIDs[0]
	}
	if !contains(deviceIDs, masterID) {
		fail(w, http.StatusBadRequest, "masterId must be one of deviceIds")
		return
	}
	if err := r.svc.Store.UpdateSyncGroup(id, name, masterID, deviceIDs); err != nil {
		log.Printf("adminapi update sync group failed: %v", err)
		fail(w, http.StatusBadRequest, "sync group not found or invalid")
		return
	}
	_ = r.svc.Store.Audit(u.ID, "update-sync-group", "", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (r *Router) deleteSyncGroup(w http.ResponseWriter, req *http.Request, u sqlite.User, id string) {
	if !requireAdmin(w, u) {
		return
	}
	if err := r.svc.Store.DeleteSyncGroup(id); err != nil {
		log.Printf("adminapi delete sync group failed: %v", err)
		fail(w, http.StatusBadRequest, "sync group not found")
		return
	}
	_ = r.svc.Store.Audit(u.ID, "delete-sync-group", "", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// normalizeSyncMembers 去空、去重、保持顺序，并校验至少 2 台设备。
// 群控最少需要 2 台才有意义（主控 + 至少 1 台从机）。
func normalizeSyncMembers(deviceIDs []string) ([]string, error) {
	seen := make(map[string]bool)
	out := make([]string, 0, len(deviceIDs))
	for _, id := range deviceIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) < 2 {
		return nil, &logic.InputError{Message: "至少需要 2 台设备"}
	}
	return out, nil
}

// contains 判断 s 是否包含 v。
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
