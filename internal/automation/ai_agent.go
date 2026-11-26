package automation

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/scrcpy"
)

// AIConfig 是视觉大模型（OpenAI 兼容多模态协议）与护栏配置。
type AIConfig struct {
	BaseURL    string `json:"baseUrl"`
	APIKey     string `json:"apiKey"`
	Model      string `json:"model"`
	MaxSteps   int    `json:"maxSteps"`
	GuardWords string `json:"guardWords"` // 逗号分隔的高危词（命中时自动中止并提醒人工介入）
}

// AIStepLog 记录 AI 智能体执行单步时的思考、动作与带编号截屏轨迹。
type AIStepLog struct {
	Index      int    `json:"index"`
	Thought    string `json:"thought"`
	Action     string `json:"action"`
	TargetID   int    `json:"targetId,omitempty"`
	TargetText string `json:"targetText,omitempty"`
	X          int    `json:"x,omitempty"`
	Y          int    `json:"y,omitempty"`
	InputText  string `json:"inputText,omitempty"`
	Direction  string `json:"direction,omitempty"`
	Status     string `json:"status"` // success / failed / guarded
	Error      string `json:"error,omitempty"`
	ShotBase64 string `json:"shotBase64,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

// AITask 表示一次在某台真机上运行的自然语言 AI 自动化任务。
type AITask struct {
	ID         string      `json:"id"`
	DeviceID   string      `json:"deviceId"`
	DeviceName string      `json:"deviceName"`
	Serial     string      `json:"serial"`
	Prompt     string      `json:"prompt"`
	Mode       string      `json:"mode"`   // "fast" (无障碍+SoM编号+视觉) | "pure" (纯视觉坐标, 0无障碍)
	Status     string      `json:"status"` // running / success / failed / stopped / guarded
	Result     string      `json:"result,omitempty"`
	Error      string      `json:"error,omitempty"`
	Steps      []AIStepLog `json:"steps"`
	CreatedAt  int64       `json:"createdAt"`
	UpdatedAt  int64       `json:"updatedAt"`
}

// aiDecision 是大模型严格返回的 JSON 动作结构。
type aiDecision struct {
	Thought   string `json:"thought"`
	Action    string `json:"action"` // click / inputText / swipe / pressKey / wait / done / fail
	TargetID  int    `json:"targetId"`
	X         int    `json:"x"` // 归一化坐标 0~1000（用于 pure 模式或无控件树兜底）
	Y         int    `json:"y"`
	Text      string `json:"text"`
	Direction string `json:"direction"` // up / down / left / right
	Key       string `json:"key"`       // back / home / enter
	Result    string `json:"result"`
}

// AIManager 管理视觉大模型任务调度、历史轨迹与取消控制。
type AIManager struct {
	adb    *scrcpy.ADB
	engine *Engine
	client *http.Client

	mu      sync.RWMutex
	tasks   map[string]*AITask
	order   []string
	cancels map[string]context.CancelFunc
}

func NewAIManager(adb *scrcpy.ADB, engine *Engine) *AIManager {
	return &AIManager{
		adb:     adb,
		engine:  engine,
		client:  &http.Client{Timeout: 60 * time.Second},
		tasks:   make(map[string]*AITask),
		order:   make([]string, 0, 32),
		cancels: make(map[string]context.CancelFunc),
	}
}

// StartTask 在后台异步启动一个 AI 真机执行任务并立即返回任务快照。
func (m *AIManager) StartTask(cfg AIConfig, deviceID, deviceName, serial, prompt, mode string) (*AITask, error) {
	if m.adb == nil || m.engine == nil {
		return nil, fmt.Errorf("adb / automation engine not available")
	}
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("请先在「模型配置」中填写视觉大模型 API Base URL 与模型名称")
	}
	if err := validateBaseURL(cfg.BaseURL); err != nil {
		return nil, err
	}
	if mode != "pure" {
		mode = "fast"
	}
	if cfg.MaxSteps <= 0 || cfg.MaxSteps > 40 {
		cfg.MaxSteps = 15
	}

	m.mu.Lock()
	for _, existing := range m.tasks {
		if existing.DeviceID == deviceID && existing.Status == "running" {
			m.mu.Unlock()
			return nil, fmt.Errorf("该设备当前已有 AI 任务正在运行，请等待完成或先中止")
		}
	}

	now := time.Now().Unix()
	id := fmt.Sprintf("ai-%d", time.Now().UnixNano())
	task := &AITask{
		ID:         id,
		DeviceID:   deviceID,
		DeviceName: deviceName,
		Serial:     serial,
		Prompt:     strings.TrimSpace(prompt),
		Mode:       mode,
		Status:     "running",
		Steps:      make([]AIStepLog, 0, cfg.MaxSteps),
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	ctx, cancel := context.WithCancel(context.Background())

	m.tasks[id] = task
	m.cancels[id] = cancel
	m.order = append([]string{id}, m.order...)
	if len(m.order) > 50 {
		oldID := m.order[len(m.order)-1]
		m.order = m.order[:len(m.order)-1]
		delete(m.tasks, oldID)
		delete(m.cancels, oldID)
	}
	snap := m.cloneTask(task)
	m.mu.Unlock()

	go m.runLoop(ctx, cfg, task)
	return snap, nil
}

// StopTask 主动中止正在运行的 AI 任务。
func (m *AIManager) StopTask(id string) bool {
	m.mu.Lock()
	cancel, ok := m.cancels[id]
	if t, exists := m.tasks[id]; exists && t.Status == "running" {
		t.Status = "stopped"
		t.UpdatedAt = time.Now().Unix()
	}
	m.mu.Unlock()
	if ok && cancel != nil {
		cancel()
		return true
	}
	return false
}

// ListTasks 返回最近 50 条 AI 任务摘要（列表不含大体积截屏 base64，加快加载）。
func (m *AIManager) ListTasks() []*AITask {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*AITask, 0, len(m.order))
	for _, id := range m.order {
		if t, ok := m.tasks[id]; ok {
			cp := *t
			cp.Steps = make([]AIStepLog, len(t.Steps))
			for i, s := range t.Steps {
				s.ShotBase64 = "" // 列表页省流量
				cp.Steps[i] = s
			}
			out = append(out, &cp)
		}
	}
	return out
}

// GetTask 返回单条 AI 任务的完整详情（包含每一步思考与 SoM 标注截图）。
func (m *AIManager) GetTask(id string) (*AITask, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	if !ok {
		return nil, false
	}
	return m.cloneTask(t), true
}

func (m *AIManager) cloneTask(t *AITask) *AITask {
	cp := *t
	cp.Steps = append([]AIStepLog(nil), t.Steps...)
	return &cp
}

// TestConnection 发送一条极简文本探针请求，验证模型地址、API Key 和模型名是否可用。
func (m *AIManager) TestConnection(ctx context.Context, cfg AIConfig) (string, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Model) == "" {
		return "", fmt.Errorf("baseUrl 与 model 不能为空")
	}
	reqBody := map[string]any{
		"model": cfg.Model,
		"messages": []map[string]any{
			{"role": "user", "content": "请回复 OK"},
		},
		"max_tokens": 16,
	}
	applyFastInferenceParams(reqBody, cfg)
	raw, err := m.callChatCompletions(ctx, cfg, reqBody)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(raw), nil
}

// RunSubGoal 供传统 RPA 流程中的「AI 智能处理」积木块调用，同步执行一个轻量 AI 子目标（最多 5 步）。
func (m *AIManager) RunSubGoal(ctx context.Context, cfg AIConfig, deviceID, serial, prompt string) error {
	if cfg.MaxSteps <= 0 || cfg.MaxSteps > 6 {
		cfg.MaxSteps = 5
	}
	task := &AITask{
		ID:       fmt.Sprintf("sub-%d", time.Now().UnixNano()),
		DeviceID: deviceID,
		Serial:   serial,
		Prompt:   prompt,
		Mode:     "fast",
		Status:   "running",
	}
	m.runLoop(ctx, cfg, task)
	if task.Status == "success" {
		return nil
	}
	if task.Error != "" {
		return fmt.Errorf("AI 智能处理未完成: %s", task.Error)
	}
	return fmt.Errorf("AI 智能处理状态: %s", task.Status)
}

func (m *AIManager) runLoop(ctx context.Context, cfg AIConfig, task *AITask) {
	defer func() {
		m.mu.Lock()
		if cancel, ok := m.cancels[task.ID]; ok {
			cancel()
			delete(m.cancels, task.ID)
		}
		m.mu.Unlock()
	}()

	guardList := parseGuardWords(cfg.GuardWords)
	historyLines := make([]string, 0, cfg.MaxSteps)

	// 任务开始前先唤醒息屏并解除无密码锁屏（KEYCODE_WAKEUP 224 幂等，已亮屏时无副作用）。
	_, _ = m.adb.Shell(ctx, task.Serial, "input keyevent 224 && wm dismiss-keyguard")

	for stepIdx := 1; stepIdx <= cfg.MaxSteps; stepIdx++ {
		if ctx.Err() != nil {
			m.finishTask(task, "stopped", "", "任务已中止")
			return
		}
		startAt := time.Now()

		// 1 & 2. 并行抓取实时手机屏幕与控件树（fast 模式走 50ms 无障碍树；pure 模式零无障碍）
		var (
			rawPNG   []byte
			capErr   error
			nodes    []UINode
			capMs    int64
			treeMs   int64
			wg       sync.WaitGroup
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			t0 := time.Now()
			rawPNG, capErr = m.adb.ScreencapBytes(ctx, task.Serial)
			capMs = time.Since(t0).Milliseconds()
		}()
		if task.Mode == "fast" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				t0 := time.Now()
				nodes, _ = DumpUIHierarchyForDevice(ctx, m.adb, task.DeviceID, task.Serial)
				treeMs = time.Since(t0).Milliseconds()
			}()
		}
		wg.Wait()
		if capErr != nil {
			m.appendStep(task, AIStepLog{
				Index: stepIdx, Action: "screencap", Status: "failed",
				Error: "抓屏失败: " + capErr.Error(), DurationMs: time.Since(startAt).Milliseconds(),
			})
			m.finishTask(task, "failed", "", "抓屏失败: "+capErr.Error())
			return
		}

		// SoM 编号标注（fast 模式）或纯压缩（pure 模式）
		var shotJPEG []byte
		var origW, origH int
		var somItems []SoMItem
		var err error
		if task.Mode == "fast" && len(nodes) > 0 {
			if som, sErr := AnnotateSoM(rawPNG, nodes); sErr == nil {
				shotJPEG = som.JPEG
				origW, origH = som.OrigW, som.OrigH
				somItems = som.Items
			}
		}
		if len(shotJPEG) == 0 {
			shotJPEG, origW, origH, err = CompressScreenshotForVision(rawPNG)
			if err != nil {
				m.finishTask(task, "failed", "", "图像编码失败: "+err.Error())
				return
			}
		}
		shotDataURI := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(shotJPEG)

		// 3. 请求视觉大模型决策下一步动作
		tLLM := time.Now()
		dec, err := m.decideNextStep(ctx, cfg, task.Prompt, task.Mode, historyLines, somItems, shotDataURI)
		llmMs := time.Since(tLLM).Milliseconds()
		log.Printf("ai-agent step=%d device=%s mode=%s cap=%dms tree=%dms(nodes=%d) llm=%dms imgKB=%d",
			stepIdx, task.DeviceID, task.Mode, capMs, treeMs, len(nodes), llmMs, len(shotJPEG)/1024)
		if err != nil {
			m.appendStep(task, AIStepLog{
				Index: stepIdx, Action: "think", Status: "failed",
				Error: "模型决策失败: " + err.Error(), ShotBase64: shotDataURI,
				DurationMs: time.Since(startAt).Milliseconds(),
			})
			m.finishTask(task, "failed", "", "模型决策失败: "+err.Error())
			return
		}

		stepLog := AIStepLog{
			Index:      stepIdx,
			Thought:    dec.Thought,
			Action:     dec.Action,
			TargetID:   dec.TargetID,
			InputText:  dec.Text,
			Direction:  dec.Direction,
			ShotBase64: shotDataURI,
			Status:     "success",
		}

		// 解析目标坐标与文本
		targetX, targetY := 0, 0
		targetLabel := ""
		if dec.TargetID > 0 && dec.TargetID <= len(somItems) {
			it := somItems[dec.TargetID-1]
			targetX, targetY = it.CenterX, it.CenterY
			targetLabel = strings.TrimSpace(it.Text)
			if targetLabel == "" {
				targetLabel = strings.TrimSpace(it.Desc)
			}
		} else if dec.X > 0 || dec.Y > 0 {
			// 0~1000 归一化坐标映射到真实设备像素
			targetX = dec.X * origW / 1000
			targetY = dec.Y * origH / 1000
		}
		stepLog.X, stepLog.Y = targetX, targetY
		stepLog.TargetText = targetLabel

		// 4. 高危词安全护栏检测（命中支付/付款/删除等关键词立即暂停，防止误操作）
		action := strings.ToLower(strings.TrimSpace(dec.Action))
		if action == "click" || action == "inputtext" {
			if hit := matchGuardWord(guardList, dec.Thought+" "+targetLabel+" "+dec.Text); hit != "" {
				stepLog.Status = "guarded"
				stepLog.Error = fmt.Sprintf("触发高危安全护栏（命中关键词「%s」），已阻止自动操作，请人工接管", hit)
				stepLog.DurationMs = time.Since(startAt).Milliseconds()
				m.appendStep(task, stepLog)
				m.finishTask(task, "guarded", "", stepLog.Error)
				return
			}
		}

		// 5. 执行动作
		switch action {
		case "done":
			stepLog.DurationMs = time.Since(startAt).Milliseconds()
			m.appendStep(task, stepLog)
			res := strings.TrimSpace(dec.Result)
			if res == "" {
				res = dec.Thought
			}
			m.finishTask(task, "success", res, "")
			return
		case "fail":
			stepLog.Status = "failed"
			stepLog.Error = dec.Result
			stepLog.DurationMs = time.Since(startAt).Milliseconds()
			m.appendStep(task, stepLog)
			m.finishTask(task, "failed", "", dec.Result)
			return
		case "click":
			if targetX <= 0 && targetY <= 0 {
				stepLog.Status = "failed"
				stepLog.Error = "模型未返回有效的 targetId 或坐标"
			} else {
				err = m.engine.tap(ctx, task.Serial, map[string]string{
					"x": strconv.Itoa(targetX),
					"y": strconv.Itoa(targetY),
				})
				if err != nil {
					stepLog.Status = "failed"
					stepLog.Error = err.Error()
				}
			}
		case "inputtext":
			if targetX > 0 && targetY > 0 {
				_ = m.engine.tap(ctx, task.Serial, map[string]string{
					"x": strconv.Itoa(targetX),
					"y": strconv.Itoa(targetY),
				})
				time.Sleep(400 * time.Millisecond)
			}
			if err := m.engine.inputText(ctx, task.Serial, map[string]string{"text": dec.Text}); err != nil {
				stepLog.Status = "failed"
				stepLog.Error = err.Error()
			}
		case "swipe":
			dir := strings.ToLower(strings.TrimSpace(dec.Direction))
			if dir == "" {
				dir = "up"
			}
			if err := m.engine.swipe(ctx, task.Serial, map[string]string{"direction": dir, "durationMs": "350"}); err != nil {
				stepLog.Status = "failed"
				stepLog.Error = err.Error()
			}
		case "presskey":
			k := strings.TrimSpace(dec.Key)
			if k == "" {
				k = "back"
			}
			if err := m.engine.pressKey(ctx, task.Serial, map[string]string{"key": k}); err != nil {
				stepLog.Status = "failed"
				stepLog.Error = err.Error()
			}
		case "wait":
			select {
			case <-ctx.Done():
				m.finishTask(task, "stopped", "", "任务已中止")
				return
			case <-time.After(2 * time.Second):
			}
		default:
			stepLog.Status = "failed"
			stepLog.Error = "未知动作: " + dec.Action
		}

		stepLog.DurationMs = time.Since(startAt).Milliseconds()
		m.appendStep(task, stepLog)
		historyLines = append(historyLines, fmt.Sprintf("第%d步: [%s] target=%d(%s) (%d,%d) text=%q thought=%s",
			stepIdx, dec.Action, dec.TargetID, targetLabel, targetX, targetY, dec.Text, dec.Thought))

		// 等待页面渲染稳定后再进入下一轮感知
		select {
		case <-ctx.Done():
			m.finishTask(task, "stopped", "", "任务已中止")
			return
		case <-time.After(900 * time.Millisecond):
		}
	}

	m.finishTask(task, "failed", "", fmt.Sprintf("已达到最大步数限制（%d 步）仍未完成目标", cfg.MaxSteps))
}

func (m *AIManager) decideNextStep(
	ctx context.Context,
	cfg AIConfig,
	goal, mode string,
	history []string,
	items []SoMItem,
	shotDataURI string,
) (*aiDecision, error) {
	systemPrompt := `你是一个专业的 Android 真机自动化操作 AI 智能体。请根据当前手机屏幕截图、任务目标与历史步骤，决定下一步最精准的单个操作，并严格只输出一个 JSON 对象（不要输出任何额外解释或 markdown 代码块外文字）。

可用动作（action 字段）：
1. "click": 点击元素。若截图上有红色数字编号框，请优先返回 "targetId": 编号；若无编号框或目标未标注，返回 0~1000 归一化坐标 "x" 和 "y"（左上角 0,0，右下角 1000,1000）。
2. "inputText": 输入文本。若需先点输入框可同时传 "targetId"（或 "x","y"），并传 "text": "要输入的内容"。
3. "swipe": 滑动屏幕，传 "direction": "up"（上滑浏览下方内容）/"down"/"left"/"right"。
4. "pressKey": 按系统键，传 "key": "back" / "home" / "enter"。
5. "wait": 等待页面加载。
6. "done": 目标已完成，在 "result" 字段中给出简洁总结或提取的数据。
7. "fail": 确认无法完成（如黑屏、无网络、需要人工登录验证码），在 "result" 说明原因。

返回 JSON 格式示例：
{"thought":"看到首页顶部搜索框编号为3，点击它","action":"click","targetId":3}
或（无编号框/纯视觉模式）：
{"thought":"点击屏幕下方居中的搜索按钮","action":"click","x":500,"y":860}`

	var userText strings.Builder
	userText.WriteString("【任务目标】\n" + goal + "\n")
	if len(history) > 0 {
		userText.WriteString("\n【已执行历史】\n")
		for _, h := range history {
			userText.WriteString("- " + h + "\n")
		}
	}
	if mode == "fast" && len(items) > 0 {
		userText.WriteString("\n【当前画面红色编号控件清单】\n")
		for _, it := range items {
			label := it.Text
			if label == "" {
				label = it.Desc
			}
			if label == "" {
				label = it.ResID
			}
			userText.WriteString(fmt.Sprintf("[%d] %s\n", it.ID, label))
		}
	} else {
		userText.WriteString("\n【模式】当前为纯净视觉模式（无编号框），请直接看图输出 0~1000 归一化坐标 x, y。\n")
	}

	reqBody := map[string]any{
		"model":       cfg.Model,
		"temperature": 0.1,
		"max_tokens":  300,
		"messages": []map[string]any{
			{"role": "system", "content": systemPrompt},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": userText.String()},
					{"type": "image_url", "image_url": map[string]string{"url": shotDataURI}},
				},
			},
		},
	}
	applyFastInferenceParams(reqBody, cfg)

	content, err := m.callChatCompletions(ctx, cfg, reqBody)
	if err != nil {
		return nil, err
	}
	return parseAIDecision(content)
}

// applyFastInferenceParams 针对默认开启长思维链（CoT）的模型关闭思考模式，
// 避免每步看图操作耗费 20~30 秒生成冗余推理过程。
func applyFastInferenceParams(reqBody map[string]any, cfg AIConfig) {
	lowerURL := strings.ToLower(cfg.BaseURL)
	lowerModel := strings.ToLower(cfg.Model)
	if strings.Contains(lowerURL, "aliyuncs.com") || strings.Contains(lowerModel, "qwen3") {
		reqBody["enable_thinking"] = false
	}
}

func (m *AIManager) callChatCompletions(ctx context.Context, cfg AIConfig, payload map[string]any) (string, error) {
	if err := validateBaseURL(cfg.BaseURL); err != nil {
		return "", err
	}
	endpoint := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		endpoint += "/chat/completions"
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := strings.TrimSpace(cfg.APIKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(respBytes))
		if len(msg) > 240 {
			msg = msg[:240]
		}
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", fmt.Errorf("解析模型响应失败: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("模型返回空 choices")
	}
	return extractContentText(parsed.Choices[0].Message.Content)
}

func validateBaseURL(raw string) error {
	u := strings.ToLower(strings.TrimSpace(raw))
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return fmt.Errorf("Base URL 必须以 http:// 或 https:// 开头")
	}
	return nil
}

// extractContentText 同时兼容 OpenAI 协议返回的纯字符串 content 与数组分片 [{"type":"text","text":"..."}]。
func extractContentText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("模型返回空内容")
	}
	var asStr string
	if err := json.Unmarshal(raw, &asStr); err == nil {
		return asStr, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				sb.WriteString(p.Text)
			}
		}
		if sb.Len() > 0 {
			return sb.String(), nil
		}
	}
	return "", fmt.Errorf("无法解析模型 content 字段: %s", string(raw))
}

func parseAIDecision(raw string) (*aiDecision, error) {
	s := strings.TrimSpace(raw)
	if idx := strings.Index(s, "{"); idx >= 0 {
		if end := strings.LastIndex(s, "}"); end > idx {
			s = s[idx : end+1]
		}
	}
	var dec aiDecision
	if err := json.Unmarshal([]byte(s), &dec); err != nil {
		return nil, fmt.Errorf("模型输出非合法 JSON: %s", raw)
	}
	if strings.TrimSpace(dec.Action) == "" {
		return nil, fmt.Errorf("模型输出缺少 action 字段")
	}
	return &dec, nil
}

func (m *AIManager) appendStep(task *AITask, step AIStepLog) {
	m.mu.Lock()
	task.Steps = append(task.Steps, step)
	task.UpdatedAt = time.Now().Unix()
	m.mu.Unlock()
}

func (m *AIManager) finishTask(task *AITask, status, result, errMsg string) {
	m.mu.Lock()
	if task.Status == "running" {
		task.Status = status
		task.Result = result
		task.Error = errMsg
		task.UpdatedAt = time.Now().Unix()
	}
	m.mu.Unlock()
}

func parseGuardWords(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		raw = "确认付款,立即支付,确认转账,注销账号,删除账号,确认删除"
	}
	raw = strings.ReplaceAll(raw, "，", ",")
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if w := strings.TrimSpace(p); w != "" {
			out = append(out, w)
		}
	}
	return out
}

func matchGuardWord(words []string, text string) string {
	lower := strings.ToLower(text)
	for _, w := range words {
		if strings.Contains(lower, strings.ToLower(w)) {
			return w
		}
	}
	return ""
}
