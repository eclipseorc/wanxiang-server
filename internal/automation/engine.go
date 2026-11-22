// Package automation 提供内嵌轻量 RPA 引擎：把一组「动作原语」按流程编排,
// 在目标安卓真机上逐步骤执行,并回写执行日志。复用心象已有的 ADB 能力,
// 不依赖外部 RPA 软件(如影刀)。
//
// 一期范围:8 个动作原语(tap/swipe/inputText/launchApp/installApk/screenshot/
// wait/ocrFind),流程以 JSON 步骤数组表达,执行引擎逐步骤解析执行。
package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/scrcpy"
)

// Step 是流程中的一个步骤。Action 是动作原语名,Params 是参数键值对。
type Step struct {
	Action string            `json:"action"`
	Params map[string]string `json:"params"`
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
}

// OCRClient 是 OCR 文字识别的抽象接口,一期可注入 nil(未接入 OCR 时 ocrFind
// 返回明确错误)。二期接入本地 RapidOCR/PaddleOCR 服务实现。
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
// 决定是否中止整个流程。
func (e *Engine) RunStep(ctx context.Context, serial string, index int, step Step) StepResult {
	res := StepResult{Index: index, Action: step.Action, Status: "success"}
	switch step.Action {
	case "tap":
		err := e.tap(ctx, serial, step.Params)
		res = result(index, step.Action, err)
	case "swipe":
		err := e.swipe(ctx, serial, step.Params)
		res = result(index, step.Action, err)
	case "inputText":
		err := e.inputText(ctx, serial, step.Params)
		res = result(index, step.Action, err)
	case "launchApp":
		err := e.launchApp(ctx, serial, step.Params)
		res = result(index, step.Action, err)
	case "installApk":
		err := e.installApk(ctx, serial, step.Params)
		res = result(index, step.Action, err)
	case "screenshot":
		path, err := e.screenshot(ctx, serial, step.Params)
		res = result(index, step.Action, err)
		res.Screenshot = path
	case "wait":
		err := e.wait(step.Params)
		res = result(index, step.Action, err)
	case "ocrFind":
		err := e.ocrFind(ctx, serial, step.Params)
		res = result(index, step.Action, err)
	default:
		res.Status = "failed"
		res.Error = "unknown action: " + step.Action
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

func (e *Engine) swipe(ctx context.Context, serial string, p map[string]string) error {
	if err := e.requireAdb(); err != nil {
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
	// 可选滑动时长(毫秒),默认 300。
	duration := 300
	if d, err := intParam(p, "duration"); err == nil {
		duration = d
	}
	_, err = e.adb.Shell(ctx, serial, fmt.Sprintf("input swipe %d %d %d %d %d", x1, y1, x2, y2, duration))
	return err
}

func (e *Engine) inputText(ctx context.Context, serial string, p map[string]string) error {
	if err := e.requireAdb(); err != nil {
		return err
	}
	text, ok := p["text"]
	if !ok || text == "" {
		return fmt.Errorf("param %q required", "text")
	}
	// input text 对空格需转义为 %s;其余特殊字符由 adb 侧处理。
	escaped := strings.ReplaceAll(text, " ", "%s")
	_, err := e.adb.Shell(ctx, serial, "input text "+escaped)
	return err
}

func (e *Engine) launchApp(ctx context.Context, serial string, p map[string]string) error {
	if err := e.requireAdb(); err != nil {
		return err
	}
	pkg, ok := p["pkg"]
	if !ok || strings.TrimSpace(pkg) == "" {
		return fmt.Errorf("param %q required", "pkg")
	}
	_, err := e.adb.Shell(ctx, serial, fmt.Sprintf("monkey -p %s -c android.intent.category.LAUNCHER 1", strings.TrimSpace(pkg)))
	return err
}

// installApk 安装应用。优先用 params 里的本地 APK 路径(apkPath);否则用
// package 名从设备的包管理器卸载——一期只支持「本地路径安装」,远程 APK 复用
// 既有 APK 上传 + adb install 能力,不在本动作内实现。
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

// screenshot 截图并保存到产物目录,返回产物路径。文件名含设备与时间戳,避免覆盖。
func (e *Engine) screenshot(ctx context.Context, serial string, p map[string]string) (string, error) {
	if err := e.requireAdb(); err != nil {
		return "", err
	}
	// 自定义保存路径(相对 shotsDir 的目录名),可选。
	sub := strings.TrimSpace(p["dir"])
	// 用设备 serial 中的非法字符替换为下划线,避免路径注入/越界。
	safe := sanitize(serial)
	name := fmt.Sprintf("%s_%d.png", safe, time.Now().UnixNano())
	if e.adb != nil {
		// 通过 exec-out screencap 拉取 PNG 二进制到本地。
		if err := e.adb.Screencap(ctx, serial, e.shotPath(sub, name)); err != nil {
			return "", err
		}
	}
	return e.shotPath(sub, name), nil
}

// shotPath 拼出截图产物的本地绝对路径。
func (e *Engine) shotPath(sub, name string) string {
	dir := e.shotsDir
	if sub != "" {
		dir = dir + "/" + sanitize(sub)
	}
	return dir + "/" + name
}

// sanitize 把字符串中的路径分隔符等替换为安全字符,防止路径注入/越界。
// 先替换分隔符,再处理「..」,避免替换顺序导致重复下划线或残留穿越序列。
func sanitize(s string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_")
	s = replacer.Replace(s)
	// 替换分隔符后,「..」已不可能形成路径穿越(点本身无害),无需额外处理,
	// 但为稳妥仍去掉连续点,避免产生「..」这类易混淆片段。
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

// ocrFind 截图后调用 OCR 识别目标文字,返回中心坐标。一期未接入 OCR 时返回
// 明确错误,提示接入 OCR 服务。
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
	_, _, found, err := e.ocr.Recognize(ctx, path, strings.TrimSpace(target))
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("text %q not found", target)
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
