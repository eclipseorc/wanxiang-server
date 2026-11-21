package sqlite

import (
	"database/sql"
	"errors"
	"time"
)

// AutomationFlow 是一条自动化流程定义。StepsJSON 是步骤数组的原始 JSON（表单编排
// 产物），由执行引擎解析；本层只负责持久化，不做结构校验。
type AutomationFlow struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	StepsJSON   string `json:"stepsJson"`
	Schedule    string `json:"schedule"`
	CreatedAt   int64  `json:"createdAt"`
	UpdatedAt   int64  `json:"updatedAt"`
}

// AutomationJob 是一次具体的自动化任务执行。
type AutomationJob struct {
	ID           string `json:"id"`
	FlowID       string `json:"flowId"`
	DeviceIDs    string `json:"deviceIds"`    // 逗号分隔的目标设备 ID
	Status       string `json:"status"`       // pending/running/success/failed/partial
	SnapshotJSON string `json:"snapshotJson"` // 触发时的步骤快照
	CreatedAt    int64  `json:"createdAt"`
	StartedAt    int64  `json:"startedAt"`
	FinishedAt   int64  `json:"finishedAt"`
}

// AutomationStepLog 是任务中单个步骤的执行日志。
type AutomationStepLog struct {
	ID             string `json:"id"`
	JobID          string `json:"jobId"`
	DeviceID       string `json:"deviceId"`
	StepIndex      int    `json:"stepIndex"`
	Action         string `json:"action"`
	ParamsJSON     string `json:"paramsJson"`
	Status         string `json:"status"` // success/failed
	Error          string `json:"error"`
	ScreenshotPath string `json:"screenshotPath"`
	CreatedAt      int64  `json:"createdAt"`
}

// --- 流程 CRUD ---

// CreateAutomationFlow 新建一条流程。
func (s *Store) CreateAutomationFlow(f AutomationFlow) error {
	now := time.Now().Unix()
	_, err := s.DB.Exec(
		`INSERT INTO automation_flows(id,name,description,steps_json,schedule,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,?)`,
		f.ID, f.Name, f.Description, f.StepsJSON, f.Schedule, now, now,
	)
	return err
}

// ListAutomationFlows 返回全部流程，按更新时间倒序。
func (s *Store) ListAutomationFlows() ([]AutomationFlow, error) {
	rows, err := s.DB.Query(
		`SELECT id,name,description,steps_json,schedule,created_at,updated_at
		 FROM automation_flows ORDER BY updated_at DESC, id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	flows := make([]AutomationFlow, 0)
	for rows.Next() {
		var f AutomationFlow
		if err := rows.Scan(&f.ID, &f.Name, &f.Description, &f.StepsJSON, &f.Schedule, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		flows = append(flows, f)
	}
	return flows, rows.Err()
}

// AutomationFlowByID 返回单条流程；不存在时返回 ErrNotFound。
func (s *Store) AutomationFlowByID(id string) (AutomationFlow, error) {
	var f AutomationFlow
	err := s.DB.QueryRow(
		`SELECT id,name,description,steps_json,schedule,created_at,updated_at
		 FROM automation_flows WHERE id=?`, id,
	).Scan(&f.ID, &f.Name, &f.Description, &f.StepsJSON, &f.Schedule, &f.CreatedAt, &f.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	return f, err
}

// UpdateAutomationFlow 更新流程的名称、描述、步骤与调度。返回 ErrNotFound 表示不存在。
func (s *Store) UpdateAutomationFlow(f AutomationFlow) error {
	result, err := s.DB.Exec(
		`UPDATE automation_flows SET name=?,description=?,steps_json=?,schedule=?,updated_at=?
		 WHERE id=?`,
		f.Name, f.Description, f.StepsJSON, f.Schedule, time.Now().Unix(), f.ID,
	)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAutomationFlow 删除流程；其任务与步骤日志由外键 ON DELETE CASCADE 一并清理。
func (s *Store) DeleteAutomationFlow(id string) error {
	result, err := s.DB.Exec(`DELETE FROM automation_flows WHERE id=?`, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

// --- 任务 ---

// CreateAutomationJob 新建一条任务（初始状态 pending）。
func (s *Store) CreateAutomationJob(j AutomationJob) error {
	_, err := s.DB.Exec(
		`INSERT INTO automation_jobs(id,flow_id,device_ids,status,snapshot_json,created_at,started_at,finished_at)
		 VALUES(?,?,?,?,?,?,0,0)`,
		j.ID, j.FlowID, j.DeviceIDs, j.Status, j.SnapshotJSON, time.Now().Unix(),
	)
	return err
}

// ListAutomationJobs 返回任务列表（按创建时间倒序），可选按 flowID 过滤。
func (s *Store) ListAutomationJobs(flowID string) ([]AutomationJob, error) {
	query := `SELECT id,flow_id,device_ids,status,snapshot_json,created_at,started_at,finished_at
	          FROM automation_jobs`
	args := make([]any, 0)
	if flowID != "" {
		query += ` WHERE flow_id=?`
		args = append(args, flowID)
	}
	query += ` ORDER BY created_at DESC, id`

	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	jobs := make([]AutomationJob, 0)
	for rows.Next() {
		var j AutomationJob
		if err := rows.Scan(&j.ID, &j.FlowID, &j.DeviceIDs, &j.Status, &j.SnapshotJSON, &j.CreatedAt, &j.StartedAt, &j.FinishedAt); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// AutomationJobByID 返回单条任务；不存在时返回 ErrNotFound。
func (s *Store) AutomationJobByID(id string) (AutomationJob, error) {
	var j AutomationJob
	err := s.DB.QueryRow(
		`SELECT id,flow_id,device_ids,status,snapshot_json,created_at,started_at,finished_at
		 FROM automation_jobs WHERE id=?`, id,
	).Scan(&j.ID, &j.FlowID, &j.DeviceIDs, &j.Status, &j.SnapshotJSON, &j.CreatedAt, &j.StartedAt, &j.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	return j, err
}

// UpdateAutomationJobStatus 更新任务状态；running 时写入开始时间，终态写入结束时间。
func (s *Store) UpdateAutomationJobStatus(id, status string) error {
	now := time.Now().Unix()
	_, err := s.DB.Exec(
		`UPDATE automation_jobs SET status=?,
		   started_at=CASE WHEN ?='running' THEN ? ELSE started_at END,
		   finished_at=CASE WHEN ? IN ('success','failed','partial') THEN ? ELSE finished_at END
		 WHERE id=?`,
		status, status, now, status, now, id,
	)
	return err
}

// --- 步骤日志 ---

// InsertAutomationStepLog 写入一条步骤执行日志。
func (s *Store) InsertAutomationStepLog(l AutomationStepLog) error {
	_, err := s.DB.Exec(
		`INSERT INTO automation_step_logs(id,job_id,device_id,step_index,action,params_json,status,error,screenshot_path,created_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		l.ID, l.JobID, l.DeviceID, l.StepIndex, l.Action, l.ParamsJSON, l.Status, l.Error, l.ScreenshotPath, time.Now().Unix(),
	)
	return err
}

// ListAutomationStepLogs 返回某任务的全部步骤日志，按步骤序号、设备排序。
func (s *Store) ListAutomationStepLogs(jobID string) ([]AutomationStepLog, error) {
	rows, err := s.DB.Query(
		`SELECT id,job_id,device_id,step_index,action,params_json,status,error,screenshot_path,created_at
		 FROM automation_step_logs WHERE job_id=? ORDER BY step_index,device_id,id`, jobID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	logs := make([]AutomationStepLog, 0)
	for rows.Next() {
		var l AutomationStepLog
		if err := rows.Scan(&l.ID, &l.JobID, &l.DeviceID, &l.StepIndex, &l.Action, &l.ParamsJSON, &l.Status, &l.Error, &l.ScreenshotPath, &l.CreatedAt); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}
