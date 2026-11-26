// Package automation 提供内嵌轻量 RPA 引擎：把一组「动作原语」按流程编排,
// 在目标安卓真机上逐步骤执行,并回写执行日志。复用万象已有的 ADB 与 UI 控件树能力,
// 不依赖外部 RPA 软件(如影刀)。
package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/eclipseorc/wanxiang-server/internal/scrcpy"
)

// Step 是流程中的一个步骤。Action 是动作原语名,Params 是参数键值对。
// Children / ElseChildren 用于 ifElementExists（条件分支）与 loop（循环）容器积木。
type Step struct {
	Action       string            `json:"action"`
	Params       map[string]string `json:"params"`
	Children     []Step            `json:"children,omitempty"`
	ElseChildren []Step            `json:"elseChildren,omitempty"`
}

// StepResult 是单个步骤的执行结果。
type StepResult struct {
	Index      int    `json:"index"`
	Action     string `json:"action"`
	Status     string `json:"status"` // success / failed
	Error      string `json:"error,omitempty"`
	Screenshot string `json:"screenshot,omitempty"` // 截图产物路径(如有)
}

// Flow 是一次完整流程的定义(步骤数组)。
type Flow struct {
	Steps []Step `json:"steps"`
}

// Engine 在指定 adb 封装上执行动作原语。
type Engine struct {
	adb          *scrcpy.ADB
	shotsDir     string // 截图产物目录
	ocr          OCRClient
	installCache map[string]string // 已上传 APK 的本地路径缓存(见 Install 动作)
	aiRunner     func(ctx context.Context, serial, prompt string) error
}

// SetAIRunner 注入「AI 智能处理」积木的执行函数（供 RPA 流程中的 aiAct 步骤调用）。
func (e *Engine) SetAIRunner(fn func(ctx context.Context, serial, prompt string) error) {
	e.aiRunner = fn
}

// OCRClient 是 OCR 文字识别的抽象接口。
type OCRClient interface {
	// Recognize 对一张截图做文字识别,返回匹配 target 文字的中心坐标。
	// 未找到时返回 found=false。
	Recognize(ctx context.Context, imagePath, target string) (x, y int, found bool, err error)
}

// NewEngine 构造执行引擎。shotsDir 为截图产物目录;ocr 可为 nil。
func NewEngine(adb *scrcpy.ADB, shotsDir string, ocr OCRClient) *Engine {
	return &Engine{adb: adb, shotsDir: shotsDir, ocr: ocr, installCache: map[string]string{}}
}

// RunStep 执行单个步骤并返回结果。serial 是目标设备的 adb serial。
// 任何动作原语执行失败都返回 Status=failed 与 Error,不向上抛出,由调用方
// 决定是否中止整个流程。若 step.Params["onError"] == "continue"，失败时也记为 success 放行。
func (e *Engine) RunStep(ctx context.Context, serial string, index int, step Step) StepResult {
	res := StepResult{Index: index, Action: step.Action, Status: "success"}
	var err error
	switch step.Action {
	case "tap":
		err = e.tap(ctx, serial, step.Params)
		res = result(index, step.Action, err)
	case "longPress":
		err = e.longPress(ctx, serial, step.Params)
		res = result(index, step.Action, err)
	case "swipe":
		err = e.swipe(ctx, serial, step.Params)
		res = result(index, step.Action, err)
	case "clickElement":
		err = e.clickElement(ctx, serial, step.Params)
		res = result(index, step.Action, err)
	case "waitForElement":
		err = e.waitForElement(ctx, serial, step.Params)
		res = result(index, step.Action, err)
	case "ifElementExists":
		err = e.ifElementExists(ctx, serial, step)
		res = result(index, step.Action, err)
	case "loop":
		err = e.runLoop(ctx, serial, step)
		res = result(index, step.Action, err)
	case "inputText":
		err = e.inputText(ctx, serial, step.Params)
		res = result(index, step.Action, err)
	case "pressKey":
		err = e.pressKey(ctx, serial, step.Params)
		res = result(index, step.Action, err)
	case "launchApp":
		err = e.launchApp(ctx, serial, step.Params)
		res = result(index, step.Action, err)
	case "stopApp":
		err = e.stopApp(ctx, serial, step.Params)
		res = result(index, step.Action, err)
	case "installApk":
		err = e.installApk(ctx, serial, step.Params)
		res = result(index, step.Action, err)
	case "screenshot":
		var path string
		path, err = e.screenshot(ctx, serial, step.Params)
		res = result(index, step.Action, err)
		res.Screenshot = path
	case "wait":
		err = e.wait(step.Params)
		res = result(index, step.Action, err)
	case "ocrFind":
		err = e.ocrFind(ctx, serial, step.Params)
		res = result(index, step.Action, err)
	case "aiAct":
		prompt := strings.TrimSpace(step.Params["prompt"])
		if prompt == "" {
			err = fmt.Errorf("param \"prompt\" (AI 指令) required")
		} else if e.aiRunner == nil {
			err = fmt.Errorf("AI 智能体未初始化")
		} else {
			err = e.aiRunner(ctx, serial, prompt)
		}
		res = result(index, step.Action, err)
	default:
		res.Status = "failed"
		res.Error = "unknown action: " + step.Action
	}
	if res.Status == "failed" && strings.EqualFold(strings.TrimSpace(step.Params["onError"]), "continue") {
		res.Status = "success"
	}
	return res
}

// result 把 error 规整为 StepResult。
func result(index int, action string, err error) StepResult {
	if err == nil {
		return StepResult{Index: index, Action: action, Status: "success"}
	}
	return StepResult{Index: index, Action: action, Status: "failed", Error: err.Error()}
}

// --- 动作原语实现 ---

func (e *Engine) requireAdb() error {
	if e.adb == nil {
		return fmt.Errorf("adb not available")
	}
	return nil
}

// intParam 从 params 读取整型参数,缺失或非法返回错误。
func intParam(params map[string]string, key string) (int, error) {
	v, ok := params[key]
	if !ok || strings.TrimSpace(v) == "" {
		return 0, fmt.Errorf("param %q required", key)
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("param %q must be an integer", key)
	}
	return n, nil
}

func (e *Engine) tap(ctx context.Context, serial string, p map[string]string) error {
	if err := e.requireAdb(); err != nil {
		return err
	}
	x, err := intParam(p, "x")
	if err != nil {
		return err
	}
	y, err := intParam(p, "y")
	if err != nil {
		return err
	}
	_, err = e.adb.Shell(ctx, serial, fmt.Sprintf("input tap %d %d", x, y))
	return err
}

func (e *Engine) longPress(ctx context.Context, serial string, p map[string]string) error {
	if err := e.requireAdb(); err != nil {
		return err
	}
	duration := 800
	if d, err := intParam(p, "duration"); err == nil && d > 0 {
		duration = d
	}
	// 支持按控件文本/ID 长按，或者按 (x, y) 坐标长按
	if target := strings.TrimSpace(p["value"]); target != "" {
		by := strings.TrimSpace(p["by"])
		node, err := e.findNodeWithWait(ctx, serial, by, target, 5000, false)
		if err != nil {
			return err
		}
		_, err = e.adb.Shell(ctx, serial, fmt.Sprintf("input swipe %d %d %d %d %d",
			node.CenterX, node.CenterY, node.CenterX, node.CenterY, duration))
		return err
	}
	x, err := intParam(p, "x")
	if err != nil {
		return err
	}
	y, err := intParam(p, "y")
	if err != nil {
		return err
	}
	_, err = e.adb.Shell(ctx, serial, fmt.Sprintf("input swipe %d %d %d %d %d", x, y, x, y, duration))
	return err
}

func (e *Engine) swipe(ctx context.Context, serial string, p map[string]string) error {
	if err := e.requireAdb(); err != nil {
		return err
	}
	duration := 350
	if d, err := intParam(p, "duration"); err == nil && d > 0 {
		duration = d
	}
	// 快捷方向滑动：direction = up / down / left / right（当未填 x1 时生效）
	if dir := strings.ToLower(strings.TrimSpace(p["direction"])); dir != "" && strings.TrimSpace(p["x1"]) == "" {
		w, h := e.screenSize(ctx, serial)
		var x1, y1, x2, y2 int
		switch dir {
		case "up":
			x1, y1, x2, y2 = w/2, h*3/4, w/2, h/4
		case "down":
			x1, y1, x2, y2 = w/2, h/4, w/2, h*3/4
		case "left":
			x1, y1, x2, y2 = w*4/5, h/2, w/5, h/2
		case "right":
			x1, y1, x2, y2 = w/5, h/2, w*4/5, h/2
		default:
			return fmt.Errorf("unknown swipe direction %q", dir)
		}
		_, err := e.adb.Shell(ctx, serial, fmt.Sprintf("input swipe %d %d %d %d %d", x1, y1, x2, y2, duration))
		return err
	}

	x1, err := intParam(p, "x1")
	if err != nil {
		return err
	}
	y1, err := intParam(p, "y1")
	if err != nil {
		return err
	}
	x2, err := intParam(p, "x2")
	if err != nil {
		return err
	}
	y2, err := intParam(p, "y2")
	if err != nil {
		return err
	}
	_, err = e.adb.Shell(ctx, serial, fmt.Sprintf("input swipe %d %d %d %d %d", x1, y1, x2, y2, duration))
	return err
}

// clickElement 按控件选择器（text / id / desc / any）查找控件并点击中心坐标。
// 支持 timeoutMs 智能等待，以及 scroll="true" 找不到时自动下滑查找。
// 若拾取到的是无文本/无ID的纯画布坐标（value 为空但包含 x, y），自动回退为坐标点击。
func (e *Engine) clickElement(ctx context.Context, serial string, p map[string]string) error {
	if err := e.requireAdb(); err != nil {
		return err
	}
	target := strings.TrimSpace(p["value"])
	if target == "" {
		target = strings.TrimSpace(p["text"])
	}
	if target == "" {
		if strings.TrimSpace(p["x"]) != "" && strings.TrimSpace(p["y"]) != "" {
			return e.tap(ctx, serial, p)
		}
		return fmt.Errorf("param \"value\" (目标文本或资源ID) required")
	}
	by := strings.TrimSpace(p["by"])
	timeoutMs := 6000
	if t, err := intParam(p, "timeoutMs"); err == nil && t > 0 {
		timeoutMs = t
	}
	scroll := strings.EqualFold(strings.TrimSpace(p["scroll"]), "true") || strings.TrimSpace(p["scroll"]) == "1"

	node, err := e.findNodeWithWait(ctx, serial, by, target, timeoutMs, scroll)
	if err != nil {
		return err
	}
	_, err = e.adb.Shell(ctx, serial, fmt.Sprintf("input tap %d %d", node.CenterX, node.CenterY))
	return err
}

// waitForElement 等待目标控件出现在屏幕上。
func (e *Engine) waitForElement(ctx context.Context, serial string, p map[string]string) error {
	if err := e.requireAdb(); err != nil {
		return err
	}
	target := strings.TrimSpace(p["value"])
	if target == "" {
		target = strings.TrimSpace(p["text"])
	}
	if target == "" {
		return fmt.Errorf("param \"value\" required")
	}
	by := strings.TrimSpace(p["by"])
	timeoutMs := 8000
	if t, err := intParam(p, "timeoutMs"); err == nil && t > 0 {
		timeoutMs = t
	}
	scroll := strings.EqualFold(strings.TrimSpace(p["scroll"]), "true") || strings.TrimSpace(p["scroll"]) == "1"
	_, err := e.findNodeWithWait(ctx, serial, by, target, timeoutMs, scroll)
	return err
}

// ifElementExists 条件判断：检测屏幕上是否存在指定控件。
// 若存在：优先执行 step.Children 子步骤；若无 Children，则按 thenAction（默认 click 该控件 / pressBack / tap）执行。
// 若不存在：执行 step.ElseChildren（若有），不报错。
func (e *Engine) ifElementExists(ctx context.Context, serial string, step Step) error {
	if err := e.requireAdb(); err != nil {
		return err
	}
	p := step.Params
	target := strings.TrimSpace(p["value"])
	if target == "" {
		target = strings.TrimSpace(p["text"])
	}
	if target == "" {
		return fmt.Errorf("param \"value\" required")
	}
	by := strings.TrimSpace(p["by"])
	timeoutMs := 2000
	if t, err := intParam(p, "timeoutMs"); err == nil && t > 0 {
		timeoutMs = t
	}

	node, err := e.findNodeWithWait(ctx, serial, by, target, timeoutMs, false)
	if err == nil {
		// 条件成立
		if len(step.Children) > 0 {
			for idx, child := range step.Children {
				res := e.RunStep(ctx, serial, idx, child)
				if res.Status != "success" {
					return fmt.Errorf("if branch step %d (%s) failed: %s", idx+1, child.Action, res.Error)
				}
			}
			return nil
		}
		thenAction := strings.ToLower(strings.TrimSpace(p["thenAction"]))
		switch thenAction {
		case "", "click":
			_, err = e.adb.Shell(ctx, serial, fmt.Sprintf("input tap %d %d", node.CenterX, node.CenterY))
			return err
		case "back", "pressback":
			_, err = e.adb.Shell(ctx, serial, "input keyevent 4")
			return err
		case "none", "skip":
			return nil
		default:
			_, err = e.adb.Shell(ctx, serial, fmt.Sprintf("input tap %d %d", node.CenterX, node.CenterY))
			return err
		}
	}

	// 条件不成立：若有 ElseChildren 则执行，否则正常跳过
	for idx, child := range step.ElseChildren {
		res := e.RunStep(ctx, serial, idx, child)
		if res.Status != "success" {
			return fmt.Errorf("else branch step %d (%s) failed: %s", idx+1, child.Action, res.Error)
		}
	}
	return nil
}

// runLoop 循环执行：按 times 次数重复执行 step.Children 子步骤（或快捷动作）。
func (e *Engine) runLoop(ctx context.Context, serial string, step Step) error {
	if err := e.requireAdb(); err != nil {
		return err
	}
	times := 3
	if t, err := intParam(step.Params, "times"); err == nil && t > 0 {
		times = t
	}
	if times > 500 {
		times = 500
	}
	intervalMs := 1000
	if iv, err := intParam(step.Params, "intervalMs"); err == nil && iv >= 0 {
		intervalMs = iv
	}

	for round := 0; round < times; round++ {
		if len(step.Children) > 0 {
			for idx, child := range step.Children {
				res := e.RunStep(ctx, serial, idx, child)
				if res.Status != "success" {
					return fmt.Errorf("loop round %d step %d (%s) failed: %s", round+1, idx+1, child.Action, res.Error)
				}
			}
		} else if subAct := strings.TrimSpace(step.Params["subAction"]); subAct != "" {
			switch subAct {
			case "swipeUp":
				_ = e.swipe(ctx, serial, map[string]string{"direction": "up", "duration": "350"})
			case "swipeDown":
				_ = e.swipe(ctx, serial, map[string]string{"direction": "down", "duration": "350"})
			case "clickElement":
				if err := e.clickElement(ctx, serial, step.Params); err != nil {
					return err
				}
			}
		}
		if intervalMs > 0 && round < times-1 {
			time.Sleep(time.Duration(intervalMs) * time.Millisecond)
		}
	}
	return nil
}

func (e *Engine) findNodeWithWait(ctx context.Context, serial, by, target string, timeoutMs int, scrollIfMissing bool) (UINode, error) {
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	scrollCount := 0
	for {
		nodes, err := DumpUIHierarchy(ctx, e.adb, serial)
		if err == nil {
			if node, ok := FindBestNode(nodes, by, target); ok {
				return node, nil
			}
		}
		if time.Now().After(deadline) {
			break
		}
		if scrollIfMissing && scrollCount < 8 {
			scrollCount++
			_ = e.swipe(ctx, serial, map[string]string{"direction": "up", "duration": "380"})
			time.Sleep(700 * time.Millisecond)
		} else {
			time.Sleep(600 * time.Millisecond)
		}
	}
	return UINode{}, fmt.Errorf("未找到目标控件 [%s=%s]（等待 %dms）", byOrAny(by), target, timeoutMs)
}

func byOrAny(by string) string {
	if by == "" {
		return "any"
	}
	return by
}

func (e *Engine) screenSize(ctx context.Context, serial string) (int, int) {
	out, err := e.adb.Shell(ctx, serial, "wm size")
	if err == nil {
		// 形如 "Physical size: 720x1612"
		for _, line := range strings.Split(out, "\n") {
			if idx := strings.LastIndex(line, ":"); idx >= 0 {
				dim := strings.TrimSpace(line[idx+1:])
				if wStr, hStr, ok := strings.Cut(dim, "x"); ok {
					w, err1 := strconv.Atoi(strings.TrimSpace(wStr))
					h, err2 := strconv.Atoi(strings.TrimSpace(hStr))
					if err1 == nil && err2 == nil && w > 0 && h > 0 {
						return w, h
					}
				}
			}
		}
	}
	return 720, 1600
}

func (e *Engine) inputText(ctx context.Context, serial string, p map[string]string) error {
	if err := e.requireAdb(); err != nil {
		return err
	}
	text, ok := p["text"]
	if !ok || text == "" {
		return fmt.Errorf("param %q required", "text")
	}
	// 纯 ASCII 走标准 input text；含中文或非 ASCII 时通过系统剪贴板 + KEYCODE_PASTE (279) 粘贴输入。
	if isASCII(text) {
		escaped := strings.ReplaceAll(text, " ", "%s")
		_, err := e.adb.Shell(ctx, serial, "input text "+escaped)
		return err
	}
	safeQuote := strings.ReplaceAll(text, "'", "'\\''")
	cmd := fmt.Sprintf("cmd clipboard set-text '%s' >/dev/null 2>&1 && input keyevent 279 || am broadcast -a ADB_INPUT_TEXT --es msg '%s'", safeQuote, safeQuote)
	_, err := e.adb.Shell(ctx, serial, cmd)
	return err
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > unicode.MaxASCII {
			return false
		}
	}
	return true
}

func (e *Engine) pressKey(ctx context.Context, serial string, p map[string]string) error {
	if err := e.requireAdb(); err != nil {
		return err
	}
	k := strings.ToUpper(strings.TrimSpace(p["key"]))
	if k == "" {
		k = "BACK"
	}
	keyMap := map[string]string{
		"BACK":        "4",
		"HOME":        "3",
		"APP_SWITCH":  "187",
		"RECENTS":     "187",
		"ENTER":       "66",
		"POWER":       "26",
		"VOLUME_UP":   "24",
		"VOLUME_DOWN": "25",
		"DELETE":      "67",
	}
	code, ok := keyMap[k]
	if !ok {
		code = k
	}
	_, err := e.adb.Shell(ctx, serial, "input keyevent "+code)
	return err
}

func (e *Engine) launchApp(ctx context.Context, serial string, p map[string]string) error {
	if err := e.requireAdb(); err != nil {
		return err
	}
	pkg := strings.TrimSpace(p["pkg"])
	if pkg == "" {
		return fmt.Errorf("param %q required", "pkg")
	}
	out, err := e.adb.Shell(ctx, serial, fmt.Sprintf("monkey -p %s -c android.intent.category.LAUNCHER 1", pkg))
	if err != nil {
		return err
	}
	lower := strings.ToLower(out)
	if strings.Contains(lower, "no activities found") || strings.Contains(lower, "monkey aborted") || strings.Contains(lower, "error:") {
		return fmt.Errorf("启动应用失败（未找到包名 %q，请填写准确的 Android 包名而非中文名）", pkg)
	}
	return nil
}

func (e *Engine) stopApp(ctx context.Context, serial string, p map[string]string) error {
	if err := e.requireAdb(); err != nil {
		return err
	}
	pkg, ok := p["pkg"]
	if !ok || strings.TrimSpace(pkg) == "" {
		return fmt.Errorf("param %q required", "pkg")
	}
	_, err := e.adb.Shell(ctx, serial, "am force-stop "+strings.TrimSpace(pkg))
	return err
}

// installApk 安装应用。
func (e *Engine) installApk(ctx context.Context, serial string, p map[string]string) error {
	if err := e.requireAdb(); err != nil {
		return err
	}
	apkPath, ok := p["apkPath"]
	if !ok || strings.TrimSpace(apkPath) == "" {
		return fmt.Errorf("param %q required", "apkPath")
	}
	return e.adb.Install(ctx, serial, strings.TrimSpace(apkPath))
}

// screenshot 截图并保存到产物目录,返回产物路径。
func (e *Engine) screenshot(ctx context.Context, serial string, p map[string]string) (string, error) {
	if err := e.requireAdb(); err != nil {
		return "", err
	}
	sub := strings.TrimSpace(p["dir"])
	safe := sanitize(serial)
	name := fmt.Sprintf("%s_%d.png", safe, time.Now().UnixNano())
	if e.adb != nil {
		if err := e.adb.Screencap(ctx, serial, e.shotPath(sub, name)); err != nil {
			return "", err
		}
	}
	return e.shotPath(sub, name), nil
}

func (e *Engine) shotPath(sub, name string) string {
	dir := e.shotsDir
	if sub != "" {
		dir = dir + "/" + sanitize(sub)
	}
	return dir + "/" + name
}

func sanitize(s string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_")
	s = replacer.Replace(s)
	s = strings.ReplaceAll(s, "..", "_")
	return s
}

func (e *Engine) wait(p map[string]string) error {
	ms, err := intParam(p, "ms")
	if err != nil {
		return err
	}
	if ms < 0 {
		return fmt.Errorf("param %q must be non-negative", "ms")
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
	return nil
}

// ocrFind 截图后调用 OCR 识别目标文字,若找到且 click!="false" 则点击中心坐标。
func (e *Engine) ocrFind(ctx context.Context, serial string, p map[string]string) error {
	target, ok := p["text"]
	if !ok || strings.TrimSpace(target) == "" {
		return fmt.Errorf("param %q required", "text")
	}
	if e.ocr == nil {
		return fmt.Errorf("OCR not configured (接入本地 OCR 服务后可用)")
	}
	path, err := e.screenshot(ctx, serial, map[string]string{"dir": "ocr"})
	if err != nil {
		return err
	}
	x, y, found, err := e.ocr.Recognize(ctx, path, strings.TrimSpace(target))
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("text %q not found", target)
	}
	if strings.EqualFold(strings.TrimSpace(p["click"]), "true") && x > 0 && y > 0 {
		_, _ = e.adb.Shell(ctx, serial, fmt.Sprintf("input tap %d %d", x, y))
	}
	return nil
}

// --- 流程解析 ---

// ParseFlow 解析流程 JSON(步骤数组)为 Flow。空数组或非法 JSON 返回错误。
func ParseFlow(stepsJSON string) (Flow, error) {
	var steps []Step
	if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
		return Flow{}, fmt.Errorf("parse flow steps: %w", err)
	}
	return Flow{Steps: steps}, nil
}
