package sqlite

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ErrNotFound is returned when a requested database record does not exist.
var ErrNotFound = errors.New("not found")

// Store groups database operations used by the application.
type Store struct {
	DB *sql.DB
}

type User struct {
	ID           string
	Username     string
	PasswordHash string
	Role         string
	CreatedAt    string
}

// GroupAccess 是一条「用户组 → 设备组」的授权关系，用于权限管理列表展示。
type GroupAccess struct {
	UserGroupID     string `json:"userGroupId"`
	UserGroupName   string `json:"userGroupName"`
	DeviceGroupID   string `json:"deviceGroupId"`
	DeviceGroupName string `json:"deviceGroupName"`
	CanView         bool   `json:"canView"`
	CanManage       bool   `json:"canManage"`
}

type Device struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Model    string   `json:"model"`
	Status   string   `json:"status"`
	Enabled  bool     `json:"enabled"`
	Online   bool     `json:"online"`
	LastSeen int64    `json:"lastSeen"`
	Battery  int      `json:"battery"`
	RTTMS    int64    `json:"rttMs"`
	GroupIDs []string `json:"groupIds"`
	// AdbIP 是设备最近一次连接成功的局域网 IP，用于拼出 adb serial（ip:port）。
	// 空串表示尚未连接过 adb。
	AdbIP string `json:"adbIp"`
	// AdbPort 是设备最近一次连接成功的无线调试连接端口，与 AdbIP 一起拼出
	// 完整 adb serial（ip:port）。无线调试端口是动态分配的，不能假设固定 5555。
	// 0 表示端口未知（尚未连接或旧数据未回填）。
	AdbPort int `json:"adbPort"`
	// AdbOnline 是设备最近一次 adb 连接状态（落库持久化）。连接成功 true，
	// 断开/未授权/未连接 false。用于在 ADBMonitor 尚未扫描到内存态时，
	// 给前端一个可靠的兜底判断，避免「未授权」设备被误判为可操作。
	AdbOnline bool `json:"adbOnline"`
	// AdbMode 记录当前/最近一次 ADB 连接的模式："auto"（安卓端自动化上报 adb-ready）
	// 或 "manual"（管理后台通过无线调试模式或传统模式手动连接）。
	AdbMode string `json:"adbMode"`
}

type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// SyncGroup 是持久化的「固定镜像组」：预存主控与成员设备，一键镜像时复用。
// 成员用 device.id 绑定（而非 adb serial），IP 变化不影响组的复用。
type SyncGroup struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	MasterID  string   `json:"masterId"`
	DeviceIDs []string `json:"deviceIds"`
}

type Ticket struct {
	Type      string
	SubjectID string
}

// NewStore creates a Store backed by db.
func NewStore(db *sql.DB) *Store {
	return &Store{DB: db}
}

// Hash returns the SHA-256 hex digest used to store bearer tokens safely.
func Hash(value string) string {
	return hexEncode(sha256Sum([]byte(value)))
}

func (s *Store) CreateUser(id, username, passwordHash, role string) error {
	// users.created_at 是 TEXT 列，必须写入与 SQLite CURRENT_TIMESTAMP
	// 相同格式的时间字符串；写入 Unix 整数会被 TEXT affinity 转成数字字符串。
	_, err := s.DB.Exec(
		`INSERT INTO users(id,username,password_hash,role,created_at) VALUES(?,?,?,?,?)`,
		id, username, passwordHash, role, timeNowStamp(),
	)
	return err
}

func (s *Store) UserByName(username string) (User, error) {
	var user User
	err := s.DB.QueryRow(
		`SELECT id,username,password_hash,role FROM users WHERE username=?`, username,
	).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Role)
	return user, notFound(err)
}

func (s *Store) UserByID(id string) (User, error) {
	var user User
	err := s.DB.QueryRow(
		`SELECT id,username,password_hash,role FROM users WHERE id=?`, id,
	).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Role)
	return user, notFound(err)
}

func (s *Store) CreateSession(tokenHash, userID string, expiry time.Time) error {
	_, err := s.DB.Exec(
		`INSERT INTO sessions(token_hash,user_id,expires_at) VALUES(?,?,?)`,
		tokenHash, userID, expiry.Unix(),
	)
	return err
}

func (s *Store) ValidateSession(tokenHash string) (User, error) {
	var user User
	err := s.DB.QueryRow(
		`SELECT u.id,u.username,u.password_hash,u.role
		 FROM sessions s JOIN users u ON u.id=s.user_id
		 WHERE s.token_hash=? AND s.expires_at>?`,
		tokenHash, time.Now().Unix(),
	).Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Role)
	return user, notFound(err)
}

func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE token_hash=?`, tokenHash)
	return err
}

// PurgeExpired 删除已过期的会话与一次性票据。
// 业务层在启动、登录和签发票据时惰性调用，避免这两张表无限增长。
// audit_logs 刻意不自动删除：审计记录需要长期保留，清理应由备份/归档策略负责。
func (s *Store) PurgeExpired() error {
	now := time.Now().Unix()
	for _, query := range []string{
		`DELETE FROM sessions WHERE expires_at<=?`,
		`DELETE FROM ws_tickets WHERE expires_at<=?`,
	} {
		if _, err := s.DB.Exec(query, now); err != nil {
			return err
		}
	}
	return nil
}

// UpsertDevice 登记设备：设备首次上报时插入，重复上报时更新型号。
// 设备 ID 由 App 本地生成，服务端只负责持久化，不再下发任何设备令牌。
// 传入空型号（如 adb-ready 先行登记）时保留库中已有型号，避免把型号清空。
func (s *Store) UpsertDevice(id, model string) error {
	_, err := s.DB.Exec(
		`INSERT INTO devices(id,name,model,created_at) VALUES(?,'',?,?)
		 ON CONFLICT(id) DO UPDATE SET model=CASE WHEN excluded.model<>'' THEN excluded.model ELSE devices.model END`,
		id, model, time.Now().Unix(),
	)
	return err
}

// SetEnabled 由转发进程直接写入设备的启用/停用状态。
func (s *Store) SetEnabled(id string, enabled bool) error {
	value := 0
	if enabled {
		value = 1
	}
	result, err := s.DB.Exec(`UPDATE devices SET enabled=? WHERE id=?`, value, id)
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

func (s *Store) DeviceEnabled(id string) (bool, error) {
	var enabled int
	err := s.DB.QueryRow(`SELECT enabled FROM devices WHERE id=?`, id).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	return enabled == 1, err
}

// DeviceByID 返回单台设备记录（含 adb_ip），用于按 ID 取设备的连接信息。
func (s *Store) DeviceByID(id string) (Device, error) {
	var device Device
	var enabled, online, adbOnline int
	var groupIDs sql.NullString
	err := s.DB.QueryRow(
		`SELECT d.id,d.name,d.model,d.enabled,d.online,d.last_seen,d.battery,d.rtt_ms,d.status_json,d.adb_ip,d.adb_port,d.adb_online,d.adb_mode,
		        group_concat(DISTINCT dg.group_id)
		 FROM devices d LEFT JOIN device_groups dg ON dg.device_id=d.id
		 WHERE d.id=? GROUP BY d.id`,
		id,
	).Scan(
		&device.ID, &device.Name, &device.Model, &enabled, &online, &device.LastSeen,
		&device.Battery, &device.RTTMS, &device.Status, &device.AdbIP, &device.AdbPort, &adbOnline, &device.AdbMode, &groupIDs,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return device, ErrNotFound
	}
	device.Enabled = enabled == 1
	device.Online = online == 1
	device.AdbOnline = adbOnline == 1
	device.GroupIDs = []string{}
	if groupIDs.Valid && groupIDs.String != "" {
		device.GroupIDs = strings.Split(groupIDs.String, ",")
	}
	return device, err
}

func (s *Store) UpdateDeviceState(id string, battery int, rtt int64, status string) error {
	_, err := s.DB.Exec(
		`UPDATE devices SET online=1,last_seen=?,battery=?,rtt_ms=?,status_json=? WHERE id=?`,
		time.Now().Unix(), battery, rtt, status, id,
	)
	return err
}

func (s *Store) SetOnline(id string, online bool) error {
	value := 0
	if online {
		value = 1
	}
	_, err := s.DB.Exec(
		`UPDATE devices SET online=?,last_seen=? WHERE id=?`, value, time.Now().Unix(), id,
	)
	return err
}

// ResetAllOnline 在 WebSocket 服务启动时将所有设备的在线与 ADB 在线标记归零，
// 防止上次进程异常退出或重启后，数据库中残留幽灵「在线 / ADB 在线」状态。
func (s *Store) ResetAllOnline() error {
	_, err := s.DB.Exec(`UPDATE devices SET online=0, adb_online=0`)
	return err
}

// SetDeviceAdb 记录设备最近一次 adb 连接的 IP、端口（拼 serial 用）与在线状态。
// 连接成功时传 ip/port 与 online=true；断开时 ip/port 保留（供自动重连）、online=false。
func (s *Store) SetDeviceAdb(id, ip string, port int, online bool) error {
	value := 0
	if online {
		value = 1
	}
	_, err := s.DB.Exec(
		`UPDATE devices SET adb_ip=?,adb_port=?,adb_online=? WHERE id=?`, ip, port, value, id,
	)
	return err
}

// SetDeviceAdbWithMode 记录设备最近一次 adb 连接的 IP、端口、在线状态以及连接模式（"auto" 或 "manual"）。
func (s *Store) SetDeviceAdbWithMode(id, ip string, port int, online bool, mode string) error {
	value := 0
	if online {
		value = 1
	}
	_, err := s.DB.Exec(
		`UPDATE devices SET adb_ip=?,adb_port=?,adb_online=?,adb_mode=? WHERE id=?`, ip, port, value, mode, id,
	)
	return err
}

func (s *Store) ListDevices() ([]Device, error) {
	rows, err := s.DB.Query(
		`SELECT d.id,d.name,d.model,d.enabled,d.online,d.last_seen,d.battery,d.rtt_ms,d.status_json,d.adb_ip,d.adb_port,d.adb_online,d.adb_mode,
		        group_concat(DISTINCT dg.group_id)
		 FROM devices d LEFT JOIN device_groups dg ON dg.device_id=d.id
		 GROUP BY d.id ORDER BY d.name,d.id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	devices := make([]Device, 0)
	for rows.Next() {
		var device Device
		var enabled, online, adbOnline int
		var groupIDs sql.NullString
		if err := rows.Scan(
			&device.ID, &device.Name, &device.Model, &enabled, &online, &device.LastSeen,
			&device.Battery, &device.RTTMS, &device.Status, &device.AdbIP, &device.AdbPort, &adbOnline, &device.AdbMode, &groupIDs,
		); err != nil {
			return nil, err
		}
		device.Enabled = enabled == 1
		device.Online = online == 1
		device.AdbOnline = adbOnline == 1
		device.GroupIDs = []string{}
		if groupIDs.Valid && groupIDs.String != "" {
			device.GroupIDs = strings.Split(groupIDs.String, ",")
		}
		devices = append(devices, device)
	}
	return devices, rows.Err()
}

func (s *Store) CreateGroup(id, name string) error {
	_, err := s.DB.Exec(
		`INSERT INTO groups(id,name,created_at) VALUES(?,?,?)`, id, name, time.Now().Unix(),
	)
	return err
}

func (s *Store) ListGroups() ([]Group, error) {
	rows, err := s.DB.Query(`SELECT id,name FROM groups ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	groups := make([]Group, 0)
	for rows.Next() {
		var group Group
		if err := rows.Scan(&group.ID, &group.Name); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

// DeleteGroup 删除分组；group_users / device_groups / group_access 由外键
// ON DELETE CASCADE 一并清理。返回 ErrNotFound 表示分组不存在。
func (s *Store) DeleteGroup(id string) error {
	result, err := s.DB.Exec(`DELETE FROM groups WHERE id=?`, id)
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

// DeleteDevice 删除单台设备记录；device_groups / sync_group_members 由外键级联清理。
func (s *Store) DeleteDevice(id string) error {
	result, err := s.DB.Exec(`DELETE FROM devices WHERE id=?`, id)
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

// DeleteOfflineDevices 批量清理所有处于离线状态（online=0）的历史僵尸设备记录。
func (s *Store) DeleteOfflineDevices() (int64, error) {
	result, err := s.DB.Exec(`DELETE FROM devices WHERE online=0`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ListUsers 返回全部用户（含角色与创建时间），按用户名排序。
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.DB.Query(
		`SELECT id,username,password_hash,role,created_at FROM users ORDER BY username,id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := make([]User, 0)
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// DeleteUser 删除用户；其会话与分组关系由外键级联清理。返回 ErrNotFound 表示不存在。
func (s *Store) DeleteUser(id string) error {
	result, err := s.DB.Exec(`DELETE FROM users WHERE id=?`, id)
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

// ListGroupAccess 返回全部「用户组 → 设备组」授权关系，join 组名供展示。
func (s *Store) ListGroupAccess() ([]GroupAccess, error) {
	rows, err := s.DB.Query(
		`SELECT ga.user_group_id, COALESCE(ug.name,''), ga.device_group_id, COALESCE(dg.name,''), ga.can_view, ga.can_manage
		 FROM group_access ga
		 LEFT JOIN groups ug ON ug.id=ga.user_group_id
		 LEFT JOIN groups dg ON dg.id=ga.device_group_id
		 ORDER BY ug.name,dg.name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]GroupAccess, 0)
	for rows.Next() {
		var a GroupAccess
		var canView, canManage int
		if err := rows.Scan(&a.UserGroupID, &a.UserGroupName, &a.DeviceGroupID, &a.DeviceGroupName, &canView, &canManage); err != nil {
			return nil, err
		}
		a.CanView = canView == 1
		a.CanManage = canManage == 1
		list = append(list, a)
	}
	return list, rows.Err()
}

// DeleteGroupAccess 删除一条授权关系（按用户组+设备组主键）。返回 ErrNotFound 表示不存在。
func (s *Store) DeleteGroupAccess(userGroupID, deviceGroupID string) error {
	result, err := s.DB.Exec(
		`DELETE FROM group_access WHERE user_group_id=? AND device_group_id=?`,
		userGroupID, deviceGroupID,
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

func (s *Store) AddUserGroup(userID, groupID string) error {
	_, err := s.DB.Exec(
		`INSERT OR IGNORE INTO group_users(group_id,user_id) VALUES(?,?)`, groupID, userID,
	)
	return err
}

func (s *Store) AddDeviceGroup(deviceID, groupID string) error {
	_, err := s.DB.Exec(
		`INSERT OR IGNORE INTO device_groups(group_id,device_id) VALUES(?,?)`, groupID, deviceID,
	)
	return err
}

// ListSyncGroups 返回全部固定镜像组及其成员（按 sort_order 排序）。
func (s *Store) ListSyncGroups() ([]SyncGroup, error) {
	rows, err := s.DB.Query(
		`SELECT g.id,g.name,g.master_id,m.device_id
		 FROM sync_groups g
		 LEFT JOIN sync_group_members m ON m.sync_group_id=g.id
		 ORDER BY g.name,g.id,m.sort_order,m.device_id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	groups := make([]SyncGroup, 0)
	byID := make(map[string]*SyncGroup)
	order := make([]string, 0)
	for rows.Next() {
		var id, name, masterID string
		var deviceID sql.NullString
		if err := rows.Scan(&id, &name, &masterID, &deviceID); err != nil {
			return nil, err
		}
		g, ok := byID[id]
		if !ok {
			g = &SyncGroup{ID: id, Name: name, MasterID: masterID, DeviceIDs: []string{}}
			byID[id] = g
			order = append(order, id)
		}
		if deviceID.Valid && deviceID.String != "" {
			g.DeviceIDs = append(g.DeviceIDs, deviceID.String)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range order {
		groups = append(groups, *byID[id])
	}
	return groups, nil
}

// SyncGroupByID 返回单个固定镜像组及其成员。
func (s *Store) SyncGroupByID(id string) (SyncGroup, error) {
	var g SyncGroup
	g.DeviceIDs = []string{}
	var masterID string
	err := s.DB.QueryRow(`SELECT id,name,master_id FROM sync_groups WHERE id=?`, id).
		Scan(&g.ID, &g.Name, &masterID)
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNotFound
	}
	if err != nil {
		return g, err
	}
	g.MasterID = masterID

	rows, err := s.DB.Query(
		`SELECT device_id FROM sync_group_members WHERE sync_group_id=? ORDER BY sort_order,device_id`, id,
	)
	if err != nil {
		return g, err
	}
	defer rows.Close()
	for rows.Next() {
		var deviceID string
		if err := rows.Scan(&deviceID); err != nil {
			return g, err
		}
		g.DeviceIDs = append(g.DeviceIDs, deviceID)
	}
	return g, rows.Err()
}

// CreateSyncGroup 创建固定镜像组：先写主表，再批量写成员。
// masterID 必须落在 memberDeviceIDs 内（由调用方保证，这里仅做空值兜底）。
func (s *Store) CreateSyncGroup(id, name, masterID string, memberDeviceIDs []string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	if _, err := tx.Exec(
		`INSERT INTO sync_groups(id,name,master_id,created_at,updated_at) VALUES(?,?,?,?,?)`,
		id, name, masterID, now, now,
	); err != nil {
		return err
	}
	for i, deviceID := range memberDeviceIDs {
		if _, err := tx.Exec(
			`INSERT INTO sync_group_members(sync_group_id,device_id,sort_order) VALUES(?,?,?)`,
			id, deviceID, i,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateSyncGroup 更新固定镜像组的名称、主控与成员（整组覆盖）。
// 若 masterID 为空，则回退为成员列表第一个。
func (s *Store) UpdateSyncGroup(id, name, masterID string, memberDeviceIDs []string) error {
	if masterID == "" && len(memberDeviceIDs) > 0 {
		masterID = memberDeviceIDs[0]
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`UPDATE sync_groups SET name=?,master_id=?,updated_at=? WHERE id=?`,
		name, masterID, time.Now().Unix(), id,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sync_group_members WHERE sync_group_id=?`, id); err != nil {
		return err
	}
	for i, deviceID := range memberDeviceIDs {
		if _, err := tx.Exec(
			`INSERT INTO sync_group_members(sync_group_id,device_id,sort_order) VALUES(?,?,?)`,
			id, deviceID, i,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteSyncGroup 删除固定镜像组；成员由外键 ON DELETE CASCADE 一并清理。
func (s *Store) DeleteSyncGroup(id string) error {
	result, err := s.DB.Exec(`DELETE FROM sync_groups WHERE id=?`, id)
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

func (s *Store) SetGroupAccess(userGroup, deviceGroup string, canView, canManage bool) error {
	if canManage {
		canView = true
	}
	view, manage := 0, 0
	if canView {
		view = 1
	}
	if canManage {
		manage = 1
	}
	_, err := s.DB.Exec(
		`INSERT INTO group_access(user_group_id,device_group_id,can_view,can_manage)
		 VALUES(?,?,?,?)
		 ON CONFLICT(user_group_id,device_group_id) DO UPDATE SET
		 can_view=excluded.can_view,can_manage=excluded.can_manage`,
		userGroup, deviceGroup, view, manage,
	)
	return err
}

func (s *Store) CanAccess(userID, deviceID string, manage bool) (bool, error) {
	user, err := s.UserByID(userID)
	if err != nil {
		return false, err
	}
	if user.Role == "admin" {
		return true, nil
	}

	// 使用两条固定 SQL 而不是拼接列名，彻底消除动态 SQL 注入面。
	const viewQuery = `SELECT EXISTS(
		SELECT 1
		FROM group_users ug
		JOIN group_access ga ON ga.user_group_id=ug.group_id
		JOIN device_groups dg ON dg.group_id=ga.device_group_id
		WHERE ug.user_id=? AND dg.device_id=? AND ga.can_view=1
	)`
	const manageQuery = `SELECT EXISTS(
		SELECT 1
		FROM group_users ug
		JOIN group_access ga ON ga.user_group_id=ug.group_id
		JOIN device_groups dg ON dg.group_id=ga.device_group_id
		WHERE ug.user_id=? AND dg.device_id=? AND ga.can_manage=1
	)`
	query := viewQuery
	if manage {
		query = manageQuery
	}
	var allowed int
	err = s.DB.QueryRow(query, userID, deviceID).Scan(&allowed)
	return allowed == 1, err
}

func (s *Store) NewTicket(tokenHash, kind, subjectID string, expiry time.Time) error {
	_, err := s.DB.Exec(
		`INSERT INTO ws_tickets(ticket_hash,subject_type,subject_id,expires_at) VALUES(?,?,?,?)`,
		tokenHash, kind, subjectID, expiry.Unix(),
	)
	return err
}

func (s *Store) ConsumeTicket(tokenHash string) (Ticket, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return Ticket{}, err
	}
	defer tx.Rollback()

	var ticket Ticket
	err = tx.QueryRow(
		`SELECT subject_type,subject_id FROM ws_tickets WHERE ticket_hash=? AND expires_at>?`,
		tokenHash, time.Now().Unix(),
	).Scan(&ticket.Type, &ticket.SubjectID)
	if err != nil {
		return Ticket{}, notFound(err)
	}
	if _, err := tx.Exec(`DELETE FROM ws_tickets WHERE ticket_hash=?`, tokenHash); err != nil {
		return Ticket{}, err
	}
	return ticket, tx.Commit()
}

func (s *Store) Audit(actorID, action, deviceID, detail string) error {
	_, err := s.DB.Exec(
		`INSERT INTO audit_logs(actor_id,action,device_id,detail,created_at) VALUES(?,?,?,?,?)`,
		actorID, action, deviceID, detail, time.Now().Unix(),
	)
	return err
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
