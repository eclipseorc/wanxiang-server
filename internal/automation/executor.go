package automation

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/eclipseorc/wanxiang-server/internal/scrcpy"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

// Executor 负责把一条任务(job)下发到多台设备并发执行,并回写步骤日志与状态。
// 它依赖 Store 做持久化,依赖 Engine 执行动作原语。
type Executor struct {
	store  *sqlite.Store
	engine *Engine
	adb    *scrcpy.ADB
}

// NewExecutor 构造执行器。maxConcurrency 为批量设备并发上限(0 表示不限,建议设上限)。
func NewExecutor(store *sqlite.Store, engine *Engine, adb *scrcpy.ADB) *Executor {
	return &Executor{store: store, engine: engine, adb: adb}
}

// Run 同步执行一条任务:按 deviceIDs 逐台(并发)跑完整个流程,写步骤日志,
// 最后按「全部成功/全部失败/部分成功」更新任务状态。
//
// 任务状态流转:调用方先以 pending 建 job,再调用 Run;Run 内先置 running,
// 结束时置终态。deviceIDs 为空或全部失败时仍返回 nil(状态已落库),真正的
// 底层错误才返回 error。
func (x *Executor) Run(ctx context.Context, jobID string) error {
	job, err := x.store.AutomationJobByID(jobID)
	if err != nil {
		return fmt.Errorf("load job: %w", err)
	}

	flow, err := ParseFlow(job.SnapshotJSON)
	if err != nil {
		return err
	}
	deviceIDs := splitDevices(job.DeviceIDs)
	if len(deviceIDs) == 0 || len(flow.Steps) == 0 {
		_ = x.store.UpdateAutomationJobStatus(jobID, "success")
		return nil
	}

	if err := x.store.UpdateAutomationJobStatus(jobID, "running"); err != nil {
		return err
	}

	// 逐设备并发执行,用 WaitGroup 收集结果,信号量限制并发上限。
	results := make([]bool, len(deviceIDs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8) // 并发上限 8,避免 adb 被海量设备打爆

	for i, deviceID := range deviceIDs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, deviceID string) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = x.runOnDevice(ctx, jobID, deviceID, flow)
		}(i, deviceID)
	}
	wg.Wait()

	// 汇总状态:全部成功 / 全部失败 / 部分成功。
	okCount, failCount := 0, 0
	for _, ok := range results {
		if ok {
			okCount++
		} else {
			failCount++
		}
	}
	status := "success"
	switch {
	case okCount == len(results):
		status = "success"
	case failCount == len(results):
		status = "failed"
	default:
		status = "partial"
	}
	if err := x.store.UpdateAutomationJobStatus(jobID, status); err != nil {
		return err
	}
	log.Printf("automation job %s finished status=%s ok=%d failed=%d", jobID, status, okCount, failCount)
	return nil
}

// runOnDevice 在单台设备上跑完整个流程,返回该设备是否全部步骤成功。
func (x *Executor) runOnDevice(ctx context.Context, jobID, deviceID string, flow Flow) bool {
	serial, err := x.resolveSerial(ctx, deviceID)
	if err != nil {
		log.Printf("automation resolve serial device=%s: %v", deviceID, err)
		return false
	}

	allOK := true
	for i, step := range flow.Steps {
		res := x.engine.RunStep(ctx, serial, i, step)
		paramsJSON, _ := json.Marshal(step.Params)
		logEntry := sqlite.AutomationStepLog{
			ID:             mustID(),
			JobID:          jobID,
			DeviceID:       deviceID,
			StepIndex:      i,
			Action:         step.Action,
			ParamsJSON:     string(paramsJSON),
			Status:         res.Status,
			Error:          res.Error,
			ScreenshotPath: res.Screenshot,
		}
		if err := x.store.InsertAutomationStepLog(logEntry); err != nil {
			log.Printf("automation write step log: %v", err)
		}
		if res.Status != "success" {
			allOK = false
			// 单设备上,某步失败即中止后续步骤(避免在错误界面继续操作)。
			break
		}
	}
	return allOK
}

// resolveSerial 把设备 ID 解析为 adb serial。设备记录里存有 adb_ip/adb_port,
// 拼成 ip:port;否则退回用 deviceID 本身(可能是已直接以 serial 形式传入)。
func (x *Executor) resolveSerial(ctx context.Context, deviceID string) (string, error) {
	if x.adb == nil {
		return "", fmt.Errorf("adb not available")
	}
	dev, err := x.store.DeviceByID(deviceID)
	if err == nil && dev.AdbIP != "" && dev.AdbPort > 0 {
		return fmt.Sprintf("%s:%d", dev.AdbIP, dev.AdbPort), nil
	}
	// 兜底:deviceID 可能直接就是 serial(ip:port)。
	if dev.ID == "" {
		return deviceID, nil
	}
	return deviceID, nil
}

// splitDevices 把逗号分隔的设备 ID 字符串拆成去空、去重的切片。
func splitDevices(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range splitComma(s) {
		part = strings.TrimSpace(part)
		if part != "" && !seen[part] {
			seen[part] = true
			out = append(out, part)
		}
	}
	return out
}

// splitComma 按逗号拆分(兼容中英文逗号)。
func splitComma(s string) []string {
	var parts []string
	cur := ""
	for _, r := range s {
		if r == ',' || r == '，' {
			parts = append(parts, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		parts = append(parts, cur)
	}
	return parts
}

// mustID 生成日志主键;失败时用时间戳兜底(仅日志用途,可容忍)。
func mustID() string {
	// 复用 scrcpy 侧无随机 ID 工具,这里内联一个简单的。
	return randHex(16)
}
