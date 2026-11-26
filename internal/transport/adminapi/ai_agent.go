package adminapi

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/eclipseorc/wanxiang-server/internal/automation"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

func (r *Router) loadAIConfig() (automation.AIConfig, error) {
	s, err := r.svc.Store.GetAISettings()
	if err != nil {
		return automation.AIConfig{}, err
	}
	return automation.AIConfig{
		BaseURL:    s.BaseURL,
		APIKey:     s.APIKey,
		Model:      s.Model,
		MaxSteps:   s.MaxSteps,
		GuardWords: s.GuardWords,
	}, nil
}

// handleAISettings 处理 GET/POST /api/admin/ai/settings。
func (r *Router) handleAISettings(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireAdmin(w, u) {
		return
	}
	switch req.Method {
	case http.MethodGet:
		s, err := r.svc.Store.GetAISettings()
		if err != nil {
			failInternal(w, "get ai settings", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"baseUrl":       s.BaseURL,
			"hasApiKey":     strings.TrimSpace(s.APIKey) != "",
			"apiKeyMasked":  maskAPIKey(s.APIKey),
			"model":         s.Model,
			"maxSteps":      s.MaxSteps,
			"guardWords":    s.GuardWords,
			"updatedAt":     s.UpdatedAt,
		})
	case http.MethodPost:
		var body struct {
			BaseURL    string `json:"baseUrl"`
			APIKey     string `json:"apiKey"`
			Model      string `json:"model"`
			MaxSteps   int    `json:"maxSteps"`
			GuardWords string `json:"guardWords"`
		}
		if decode(w, req, &body) != nil {
			fail(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		cfg := sqlite.AISettings{
			BaseURL:    strings.TrimSpace(body.BaseURL),
			APIKey:     strings.TrimSpace(body.APIKey),
			Model:      strings.TrimSpace(body.Model),
			MaxSteps:   body.MaxSteps,
			GuardWords: strings.TrimSpace(body.GuardWords),
		}
		if err := r.svc.Store.SaveAISettings(cfg); err != nil {
			failInternal(w, "save ai settings", err)
			return
		}
		_ = r.svc.Store.Audit(u.ID, "ai-settings-save", "", cfg.Model)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleAITest 处理 POST /api/admin/ai/test，测试当前填写或已保存的模型配置是否连通。
func (r *Router) handleAITest(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if r.ai == nil {
		fail(w, http.StatusServiceUnavailable, "ai agent not configured")
		return
	}
	var body struct {
		BaseURL string `json:"baseUrl"`
		APIKey  string `json:"apiKey"`
		Model   string `json:"model"`
	}
	_ = decode(w, req, &body)
	saved, _ := r.loadAIConfig()
	cfg := automation.AIConfig{
		BaseURL: strings.TrimSpace(body.BaseURL),
		APIKey:  strings.TrimSpace(body.APIKey),
		Model:   strings.TrimSpace(body.Model),
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = saved.BaseURL
	}
	if cfg.APIKey == "" {
		cfg.APIKey = saved.APIKey
	}
	if cfg.Model == "" {
		cfg.Model = saved.Model
	}

	reply, err := r.ai.TestConnection(req.Context(), cfg)
	if err != nil {
		fail(w, http.StatusBadGateway, "模型连通性测试失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reply": reply})
}

// handleAIRun 处理 POST /api/admin/ai/run，在目标设备上异步启动自然语言 AI 任务。
func (r *Router) handleAIRun(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if r.ai == nil {
		fail(w, http.StatusServiceUnavailable, "ai agent not configured")
		return
	}
	var body struct {
		DeviceID string `json:"deviceId"`
		Prompt   string `json:"prompt"`
		Mode     string `json:"mode"` // "fast" | "pure"
	}
	if decode(w, req, &body) != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.DeviceID = strings.TrimSpace(body.DeviceID)
	body.Prompt = strings.TrimSpace(body.Prompt)
	if body.DeviceID == "" || body.Prompt == "" {
		fail(w, http.StatusBadRequest, "请选择目标设备并输入任务指令")
		return
	}

	dev, err := r.svc.Store.DeviceByID(body.DeviceID)
	if err != nil {
		failNotFound(w, "device", err)
		return
	}
	if !dev.Online || !dev.AdbOnline || strings.TrimSpace(dev.AdbIP) == "" {
		fail(w, http.StatusConflict, "目标设备 ADB 未在线，请先连接设备")
		return
	}
	port := dev.AdbPort
	if port <= 0 {
		port = 5555
	}
	serial := fmt.Sprintf("%s:%d", strings.TrimSpace(dev.AdbIP), port)
	devName := strings.TrimSpace(dev.Name)
	if devName == "" {
		devName = strings.TrimSpace(dev.Model)
	}
	if devName == "" {
		devName = dev.ID
	}

	cfg, err := r.loadAIConfig()
	if err != nil {
		failInternal(w, "load ai config", err)
		return
	}

	task, err := r.ai.StartTask(cfg, dev.ID, devName, serial, body.Prompt, body.Mode)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = r.svc.Store.Audit(u.ID, "ai-run", dev.ID, body.Prompt)
	writeJSON(w, http.StatusOK, task)
}

// handleAITasks 处理 GET /api/admin/ai/tasks。
func (r *Router) handleAITasks(w http.ResponseWriter, req *http.Request, u sqlite.User) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	if !requireAdmin(w, u) {
		return
	}
	if r.ai == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, r.ai.ListTasks())
}

// handleAITaskDetail 处理 GET /api/admin/ai/tasks/{id} 与 POST /api/admin/ai/tasks/{id}/stop。
func (r *Router) handleAITaskDetail(w http.ResponseWriter, req *http.Request, u sqlite.User, rest string) {
	if !requireAdmin(w, u) {
		return
	}
	if r.ai == nil {
		fail(w, http.StatusServiceUnavailable, "ai agent not configured")
		return
	}
	if strings.HasSuffix(rest, "/stop") {
		if !requireMethod(w, req, http.MethodPost) {
			return
		}
		id := strings.TrimSuffix(rest, "/stop")
		r.ai.StopTask(id)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if !requireMethod(w, req, http.MethodGet) {
		return
	}
	task, ok := r.ai.GetTask(rest)
	if !ok {
		fail(w, http.StatusNotFound, "task not found")
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func maskAPIKey(k string) string {
	k = strings.TrimSpace(k)
	if len(k) <= 8 {
		if k == "" {
			return ""
		}
		return "****"
	}
	return k[:4] + "****" + k[len(k)-4:]
}
