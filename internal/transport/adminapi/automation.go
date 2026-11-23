// automation.go 提供「自动化引擎」的管理 API:流程 CRUD、手动触发执行、
// 任务列表与详情(含步骤日志)。流程以 JSON 步骤数组定义,执行引擎复用
// scrcpy.ADB 在目标真机上逐步骤执行。
package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/eclipseorc/wanxiang-server/internal/automation"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

// automationFlowBody 是创建/更新流程的请求体。
type automationFlowBody struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Steps       []automation.Step  `json:"steps"`
	Schedule    string             `json:"schedule"`
	DeviceIDs   []string           `json:"deviceIds"`
}

// flowResponse 是流程的展示结构(含解析后的步骤)。
type flowResponse struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Steps       []automation.Step `json:"steps"`
	Schedule    string            `json:"schedule"`
	DeviceIDs   []string          `json:"deviceIds"`
	CreatedAt   int64             `json:"createdAt"`
	UpdatedAt   int64             `json:"updatedAt"`
}

func flowToResponse(f sqlite.AutomationFlow) flowResponse {
	var steps []automation.Step
	_ = json.Unmarshal([]byte(f.StepsJSON), &steps)
	if steps == nil {
		steps = []automation.Step{}
	}
	deviceIDs := []string{}
	for _, p := range strings.Split(f.DeviceIDs, ",") {
		if p = strings.TrimSpace(p); p != "" {
			deviceIDs = append(deviceIDs, p)
		}
	}
	return flowResponse{
		ID:          f.ID,
		Name:        f.Name,
		Description: f.Description,
		Steps:       steps,
		Schedule:    f.Schedule,
		DeviceIDs:   deviceIDs,
		CreatedAt:   f.CreatedAt,
		UpdatedAt:   f.UpdatedAt,
	}
}

// handleAutomationFlows 处理 GET(列表)与 POST(创建)。
func (r *Router) handleAutomationFlows(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	switch req.Method {
	case http.MethodGet:
		r.listAutomationFlows(w, req, u)
	case http.MethodPost:
		r.createAutomationFlow(w, req, u)
	default:
		requireMethod(w, req, http.MethodGet, http.MethodPost)
	}
}

func (r *Router) listAutomationFlows(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireAdmin(w, u) {
		return
	}
	flows, err := r.svc.Store.ListAutomationFlows()
	if err != nil {
		failInternal(w, "list automation flows", err)
		return
	}
	out := make([]flowResponse, 0, len(flows))
	for _, f := range flows {
		out = append(out, flowToResponse(f))
	}
	writeJSON(w, http.StatusOK, out)
}

func (r *Router) createAutomationFlow(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireAdmin(w, u) {
		return
	}
	var body automationFlowBody
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		fail(w, http.StatusBadRequest, "name required")
		return
	}
	stepsJSON, err := json.Marshal(body.Steps)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid steps")
		return
	}
	id, err := randomID()
	if err != nil {
		failInternal(w, "generate flow id", err)
		return
	}
	flow := sqlite.AutomationFlow{
		ID:          id,
		Name:        body.Name,
		Description: body.Description,
		StepsJSON:   string(stepsJSON),
		Schedule:    strings.TrimSpace(body.Schedule),
		DeviceIDs:   strings.Join(body.DeviceIDs, ","),
	}
	if err := r.svc.Store.CreateAutomationFlow(flow); err != nil {
		failInternal(w, "create automation flow", err)
		return
	}
	_ = r.svc.Store.Audit(u.ID, "automation-flow-create", "", id)
	writeJSON(w, http.StatusCreated, flowToResponse(flow))
}

// handleAutomationFlow 处理单条流程的 GET/PUT/DELETE。
func (r *Router) handleAutomationFlow(w http.ResponseWriter, req *http.Request, u sqlite.User, id string) {
	switch req.Method {
	case http.MethodGet:
		r.getAutomationFlow(w, req, u, id)
	case http.MethodPut:
		r.updateAutomationFlow(w, req, u, id)
	case http.MethodDelete:
		r.deleteAutomationFlow(w, req, u, id)
	default:
		requireMethod(w, req, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

func (r *Router) getAutomationFlow(w http.ResponseWriter, req *http.Request, u sqlite.User, id string) {
	if !requireAdmin(w, u) {
		return
	}
	f, err := r.svc.Store.AutomationFlowByID(id)
	if err != nil {
		failNotFound(w, "flow", err)
		return
	}
	writeJSON(w, http.StatusOK, flowToResponse(f))
}

func (r *Router) updateAutomationFlow(w http.ResponseWriter, req *http.Request, u sqlite.User, id string) {
	if !requireAdmin(w, u) {
		return
	}
	var body automationFlowBody
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		fail(w, http.StatusBadRequest, "name required")
		return
	}
	stepsJSON, err := json.Marshal(body.Steps)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid steps")
		return
	}
	flow := sqlite.AutomationFlow{
		ID:          id,
		Name:        body.Name,
		Description: body.Description,
		StepsJSON:   string(stepsJSON),
		Schedule:    strings.TrimSpace(body.Schedule),
		DeviceIDs:   strings.Join(body.DeviceIDs, ","),
	}
	if err := r.svc.Store.UpdateAutomationFlow(flow); err != nil {
		failNotFound(w, "flow", err)
		return
	}
	_ = r.svc.Store.Audit(u.ID, "automation-flow-update", "", id)
	writeJSON(w, http.StatusOK, flowToResponse(flow))
}

func (r *Router) deleteAutomationFlow(w http.ResponseWriter, req *http.Request, u sqlite.User, id string) {
	if !requireAdmin(w, u) {
		return
	}
	if err := r.svc.Store.DeleteAutomationFlow(id); err != nil {
		failNotFound(w, "flow", err)
		return
	}
	_ = r.svc.Store.Audit(u.ID, "automation-flow-delete", "", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAutomationRun 手动触发流程执行,创建任务并异步执行。
// POST /api/admin/automation/flows/{id}/run   { deviceIds: [] }
func (r *Router) handleAutomationRun(w http.ResponseWriter, req *http.Request, u sqlite.User, id string) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if r.automation == nil {
		fail(w, http.StatusServiceUnavailable, "automation engine not configured")
		return
	}
	var body struct {
		DeviceIDs []string `json:"deviceIds"`
	}
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if len(body.DeviceIDs) == 0 {
		fail(w, http.StatusBadRequest, "deviceIds required")
		return
	}

	flow, err := r.svc.Store.AutomationFlowByID(id)
	if err != nil {
		failNotFound(w, "flow", err)
		return
	}

	jobID, err := randomID()
	if err != nil {
		failInternal(w, "generate job id", err)
		return
	}
	// device_ids 存逗号分隔(与 executor 的 splitDevices 对齐)。
	deviceIDs := strings.Join(body.DeviceIDs, ",")
	job := sqlite.AutomationJob{
		ID:           jobID,
		FlowID:       flow.ID,
		DeviceIDs:    deviceIDs,
		Status:       "pending",
		SnapshotJSON: flow.StepsJSON,
	}
	if err := r.svc.Store.CreateAutomationJob(job); err != nil {
		failInternal(w, "create automation job", err)
		return
	}

	// 异步执行,避免长时间任务阻塞 HTTP 请求（必须用独立 Background context，不可用会随 HTTP 返回即取消的 req.Context()）。
	go func() {
		if err := r.automation.Run(context.Background(), jobID); err != nil {
			// 执行器内部已尽量把失败落库;这里只记录底层错误。
			_ = r.svc.Store.UpdateAutomationJobStatus(jobID, "failed")
		}
	}()

	_ = r.svc.Store.Audit(u.ID, "automation-run", id, fmt.Sprintf("job=%s devices=%d", jobID, len(body.DeviceIDs)))
	writeJSON(w, http.StatusAccepted, map[string]string{"jobId": jobID})
}

// handleAutomationJobs 返回任务列表,可选 ?flowId= 过滤。
func (r *Router) handleAutomationJobs(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	flowID := strings.TrimSpace(req.URL.Query().Get("flowId"))
	jobs, err := r.svc.Store.ListAutomationJobs(flowID)
	if err != nil {
		failInternal(w, "list automation jobs", err)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

// handleAutomationJob 返回单条任务详情(含步骤日志)。
func (r *Router) handleAutomationJob(w http.ResponseWriter, req *http.Request, u sqlite.User, id string) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	job, err := r.svc.Store.AutomationJobByID(id)
	if err != nil {
		failNotFound(w, "job", err)
		return
	}
	logs, err := r.svc.Store.ListAutomationStepLogs(id)
	if err != nil {
		failInternal(w, "list automation step logs", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"job":  job,
		"logs": logs,
	})
}

// handleAutomationUITree 抓取目标设备当前屏幕的 UI 控件树（供前端 Scrcpy 屏幕元素拾取器悬停高亮与一键提取选择器）。
// GET /api/admin/automation/ui-tree?deviceId=xxx
func (r *Router) handleAutomationUITree(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if r.scrcpy == nil {
		fail(w, http.StatusServiceUnavailable, "adb not configured")
		return
	}
	deviceID := strings.TrimSpace(req.URL.Query().Get("deviceId"))
	serial := strings.TrimSpace(req.URL.Query().Get("serial"))
	if serial == "" && deviceID != "" {
		dev, err := r.svc.Store.DeviceByID(deviceID)
		if err != nil {
			failNotFound(w, "device", err)
			return
		}
		if dev.AdbIP == "" {
			fail(w, http.StatusBadRequest, "设备尚未建立 ADB 连接（无 IP）")
			return
		}
		port := dev.AdbPort
		if port <= 0 {
			port = 5555
		}
		serial = fmt.Sprintf("%s:%d", dev.AdbIP, port)
	}
	if serial == "" {
		fail(w, http.StatusBadRequest, "deviceId or serial required")
		return
	}
	nodes, err := automation.DumpUIHierarchy(req.Context(), r.scrcpy, serial)
	if err != nil {
		fail(w, http.StatusBadGateway, "抓取手机控件树失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"serial": serial,
		"nodes":  nodes,
	})
}

// buildOCR 按环境变量构造 OCR 客户端。OCR 属于可选能力:未显式启用时返回
// nil,ocFind 动作会返回「OCR not configured」;启用时用本地 Tesseract。
// 通过 AUTOMATION_OCR=1 启用;可选 AUTOMATION_OCR_LANGS(如 chi_sim+eng)、
// AUTOMATION_OCR_TESSDATA(语言包目录)微调。
func buildOCR() automation.OCRClient {
	if os.Getenv("AUTOMATION_OCR") != "1" {
		return nil
	}
	langs := os.Getenv("AUTOMATION_OCR_LANGS")
	prefix := os.Getenv("AUTOMATION_OCR_TESSDATA")
	return automation.NewTesseractOCR(langs, prefix)
}

// automationMaxConcurrency 从环境变量 AUTOMATION_MAX_CONCURRENCY 读取批量设备
// 并发上限;非法或未设置时返回 0(由 Executor 回退到默认 8)。
func automationMaxConcurrency() int {
	v := strings.TrimSpace(os.Getenv("AUTOMATION_MAX_CONCURRENCY"))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// failNotFound 统一处理「不存在」错误:ErrNotFound 返回 404,其余内部错误。
func failNotFound(w http.ResponseWriter, what string, err error) {
	if err == sqlite.ErrNotFound {
		fail(w, http.StatusNotFound, what+" not found")
		return
	}
	failInternal(w, "automation "+what, err)
}
