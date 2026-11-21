// automation.go 提供「自动化引擎」的管理 API:流程 CRUD、手动触发执行、
// 任务列表与详情(含步骤日志)。流程以 JSON 步骤数组定义,执行引擎复用
// scrcpy.ADB 在目标真机上逐步骤执行。
package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
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
}

// flowResponse 是流程的展示结构(含解析后的步骤)。
type flowResponse struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Steps       []automation.Step `json:"steps"`
	Schedule    string            `json:"schedule"`
	CreatedAt   int64             `json:"createdAt"`
	UpdatedAt   int64             `json:"updatedAt"`
}

func flowToResponse(f sqlite.AutomationFlow) flowResponse {
	var steps []automation.Step
	_ = json.Unmarshal([]byte(f.StepsJSON), &steps)
	if steps == nil {
		steps = []automation.Step{}
	}
	return flowResponse{
		ID:          f.ID,
		Name:        f.Name,
		Description: f.Description,
		Steps:       steps,
		Schedule:    f.Schedule,
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

	// 异步执行,避免长时间任务阻塞 HTTP 请求。
	go func() {
		if err := r.automation.Run(req.Context(), jobID); err != nil {
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

// failNotFound 统一处理「不存在」错误:ErrNotFound 返回 404,其余内部错误。
func failNotFound(w http.ResponseWriter, what string, err error) {
	if err == sqlite.ErrNotFound {
		fail(w, http.StatusNotFound, what+" not found")
		return
	}
	failInternal(w, "automation "+what, err)
}
