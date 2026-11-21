package automation

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

// Scheduler 周期性检查带 schedule 的流程,到点自动创建任务并交给 Executor 执行。
// 它采用「每分钟 tick + 最近一次触发时间记忆」的朴素实现:对每条带 cron 的流程,
// 记录上一次触发到的时间点,若当前分钟命中且尚未在本分钟触发过,则触发。
//
// 这是进程内单实例调度器(admin-server 进程),不支持多副本去重——本系统为单实例
// 部署,满足需求;若未来横向扩容,需引入分布式锁/DB 级任务去重。
type Scheduler struct {
	store    *sqlite.Store
	executor *Executor
	// lastTrigger 记录 flowID -> 上次触发的「分钟时间戳」(Unix 分钟),避免重复触发。
	lastTrigger map[string]int64
	stop        chan struct{}
	done        chan struct{}
}

// NewScheduler 构造调度器。不自动启动,需调用 Start。
func NewScheduler(store *sqlite.Store, executor *Executor) *Scheduler {
	return &Scheduler{
		store:       store,
		executor:    executor,
		lastTrigger: map[string]int64{},
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
}

// Start 启动调度循环。每 30 秒 tick 一次,到分钟边界时检查各流程是否到点。
func (sc *Scheduler) Start() {
	go sc.loop()
	log.Printf("automation scheduler started")
}

// Stop 优雅停止调度循环(等待当前 tick 结束)。
func (sc *Scheduler) Stop() {
	close(sc.stop)
	<-sc.done
}

func (sc *Scheduler) loop() {
	defer close(sc.done)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	// 记录上一次处理的「分钟」,避免同一分钟被 30s tick 触发两次。
	var lastMinute string
	for {
		select {
		case <-sc.stop:
			return
		case now := <-ticker.C:
			minute := now.Format("2006-01-02 15:04")
			if minute == lastMinute {
				continue
			}
			lastMinute = minute
			sc.tick(now)
		}
	}
}

// tick 检查所有带 schedule 的流程,对到点且未触发的流程执行一次。
func (sc *Scheduler) tick(now time.Time) {
	flows, err := sc.store.ListAutomationFlows()
	if err != nil {
		log.Printf("automation scheduler list flows: %v", err)
		return
	}
	for _, f := range flows {
		schedule := strings.TrimSpace(f.Schedule)
		if schedule == "" {
			continue
		}
		cs, err := parseCron(schedule)
		if err != nil {
			// 非法 cron 表达式只告警一次,不阻塞其他流程。
			log.Printf("automation scheduler flow=%s invalid cron %q: %v", f.ID, schedule, err)
			continue
		}
		if !cs.matches(now) {
			continue
		}
		minuteTS := now.Truncate(time.Minute).Unix()
		if sc.lastTrigger[f.ID] == minuteTS {
			continue
		}
		sc.lastTrigger[f.ID] = minuteTS
		go sc.trigger(f)
	}
}

// trigger 为一条流程创建任务并异步执行。目标设备取流程的 DeviceIDs;为空时
// 取全部「业务在线且 ADB 在线」的设备。
func (sc *Scheduler) trigger(f sqlite.AutomationFlow) {
	deviceIDs := splitDevices(f.DeviceIDs)
	if len(deviceIDs) == 0 {
		online, err := sc.store.ListOnlineAdbDevices()
		if err != nil {
			log.Printf("automation scheduler flow=%s list online devices: %v", f.ID, err)
			return
		}
		deviceIDs = online
	}
	if len(deviceIDs) == 0 {
		log.Printf("automation scheduler flow=%s: no target devices, skip", f.ID)
		return
	}

	job := sqlite.AutomationJob{
		ID:           mustID(),
		FlowID:       f.ID,
		DeviceIDs:    strings.Join(deviceIDs, ","),
		Status:       "pending",
		SnapshotJSON: f.StepsJSON,
	}
	if err := sc.store.CreateAutomationJob(job); err != nil {
		log.Printf("automation scheduler flow=%s create job: %v", f.ID, err)
		return
	}
	log.Printf("automation scheduler flow=%s triggered job=%s devices=%d", f.ID, job.ID, len(deviceIDs))

	go func() {
		if err := sc.executor.Run(context.Background(), job.ID); err != nil {
			_ = sc.store.UpdateAutomationJobStatus(job.ID, "failed")
			log.Printf("automation scheduler job=%s run: %v", job.ID, err)
		}
	}()
}
