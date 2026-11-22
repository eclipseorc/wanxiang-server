package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// DeviceModelScript 是「设备自动配对」的一条机型脚本：按厂商+型号匹配一台设备的
// 「动作序列」JSON，下发给设备端由通用引擎执行。MatchPatterns 是候选匹配串数组
// （如 ["motorola moto e14","moto e14"]），StepsJSON 是动作步骤数组的原始 JSON，
// 本层只负责持久化，不解析步骤结构。Version 用于设备端版本比较。
type DeviceModelScript struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	MatchPatterns string `json:"matchPatterns"`
	StepsJSON     string `json:"stepsJson"`
	Version       int    `json:"version"`
	UpdatedAt     int64  `json:"updatedAt"`
}

// DeviceModelOption 是「已上报型号」下拉选项：型号串 + 该型号的设备数。
type DeviceModelOption struct {
	Model       string `json:"model"`
	DeviceCount int    `json:"deviceCount"`
}

// ListDeviceModelScripts 返回全部机型脚本，按更新时间倒序。
func (s *Store) ListDeviceModelScripts() ([]DeviceModelScript, error) {
	rows, err := s.DB.Query(
		`SELECT id,name,match_patterns,steps_json,version,updated_at
		 FROM device_model_scripts ORDER BY updated_at DESC, id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	scripts := make([]DeviceModelScript, 0)
	for rows.Next() {
		var sc DeviceModelScript
		if err := rows.Scan(&sc.ID, &sc.Name, &sc.MatchPatterns, &sc.StepsJSON, &sc.Version, &sc.UpdatedAt); err != nil {
			return nil, err
		}
		scripts = append(scripts, sc)
	}
	return scripts, rows.Err()
}

// DeviceModelScriptByID 返回单条脚本；不存在时返回 ErrNotFound。
func (s *Store) DeviceModelScriptByID(id string) (DeviceModelScript, error) {
	var sc DeviceModelScript
	err := s.DB.QueryRow(
		`SELECT id,name,match_patterns,steps_json,version,updated_at
		 FROM device_model_scripts WHERE id=?`, id,
	).Scan(&sc.ID, &sc.Name, &sc.MatchPatterns, &sc.StepsJSON, &sc.Version, &sc.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return sc, ErrNotFound
	}
	return sc, err
}

// DeviceModelScriptByMatch 按设备的「厂商+型号」串匹配脚本。match_patterns 是
// 候选串数组，命中规则是「候选串是 model 的子串（忽略大小写）」——比精确等于更
// 宽松，能兼容同机型的不同写法（如 "moto e14" 命中 "motorola moto e14"）。
// 多个脚本命中时取 version 最高者；未命中返回 ErrNotFound。
func (s *Store) DeviceModelScriptByMatch(model string) (DeviceModelScript, error) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return DeviceModelScript{}, ErrNotFound
	}
	scripts, err := s.ListDeviceModelScripts()
	if err != nil {
		return DeviceModelScript{}, err
	}
	var best DeviceModelScript
	found := false
	for _, sc := range scripts {
		if !matchPatternsHit(sc.MatchPatterns, model) {
			continue
		}
		if !found || sc.Version > best.Version {
			best = sc
			found = true
		}
	}
	if !found {
		return DeviceModelScript{}, ErrNotFound
	}
	return best, nil
}

// matchPatternsHit 判断候选匹配串数组里是否有任一串是 model 的子串（忽略大小写）。
func matchPatternsHit(matchPatternsJSON, model string) bool {
	var patterns []string
	if err := json.Unmarshal([]byte(matchPatternsJSON), &patterns); err != nil {
		return false
	}
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" && strings.Contains(model, p) {
			return true
		}
	}
	return false
}

// UpsertDeviceModelScript 写入（新建或覆盖）一条机型脚本。覆盖时 version 自增 1，
// 确保设备端能用「版本比较」识别出新版并更新本地缓存。
func (s *Store) UpsertDeviceModelScript(sc DeviceModelScript) error {
	now := time.Now().Unix()
	// 先取现有版本，覆盖时自增；新建时从 0 起步。
	version := 0
	if existing, err := s.DeviceModelScriptByID(sc.ID); err == nil {
		version = existing.Version
	}
	if _, err := s.DB.Exec(
		`INSERT INTO device_model_scripts(id,name,match_patterns,steps_json,version,updated_at)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   name=excluded.name,
		   match_patterns=excluded.match_patterns,
		   steps_json=excluded.steps_json,
		   version=excluded.version,
		   updated_at=excluded.updated_at`,
		sc.ID, sc.Name, sc.MatchPatterns, sc.StepsJSON, version+1, now,
	); err != nil {
		return err
	}
	return nil
}

// DistinctDeviceModels 返回所有设备上报过的非空型号去重列表（含设备数），供
// 「机型脚本」编辑页做型号下拉选择。数据源是 devices.model，与脚本匹配用的是
// 同一个字段。按型号排序。
func (s *Store) DistinctDeviceModels() ([]DeviceModelOption, error) {
	rows, err := s.DB.Query(
		`SELECT model, COUNT(*) FROM devices WHERE model<>'' GROUP BY model ORDER BY model`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	options := make([]DeviceModelOption, 0)
	for rows.Next() {
		var o DeviceModelOption
		if err := rows.Scan(&o.Model, &o.DeviceCount); err != nil {
			return nil, err
		}
		options = append(options, o)
	}
	return options, rows.Err()
}

// DeleteDeviceModelScript 删除一条机型脚本；返回 ErrNotFound 表示不存在。
func (s *Store) DeleteDeviceModelScript(id string) error {
	result, err := s.DB.Exec(`DELETE FROM device_model_scripts WHERE id=?`, id)
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
