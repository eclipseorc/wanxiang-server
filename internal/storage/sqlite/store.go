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
}

type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
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
func (s *Store) UpsertDevice(id, model string) error {
	_, err := s.DB.Exec(
		`INSERT INTO devices(id,name,model,created_at) VALUES(?,'',?,?)
		 ON CONFLICT(id) DO UPDATE SET model=excluded.model`,
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

func (s *Store) ListDevices() ([]Device, error) {
	rows, err := s.DB.Query(
		`SELECT d.id,d.name,d.model,d.enabled,d.online,d.last_seen,d.battery,d.rtt_ms,d.status_json,
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
		var enabled, online int
		var groupIDs sql.NullString
		if err := rows.Scan(
			&device.ID, &device.Name, &device.Model, &enabled, &online, &device.LastSeen,
			&device.Battery, &device.RTTMS, &device.Status, &groupIDs,
		); err != nil {
			return nil, err
		}
		device.Enabled = enabled == 1
		device.Online = online == 1
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

func (s *Store) StartWatch(sessionID, deviceID, userID string) error {
	result, err := s.DB.Exec(
		`UPDATE watch_sessions SET session_id=?,device_id=?,user_id=?
		 WHERE id=1 AND session_id IS NULL
		 AND EXISTS(SELECT 1 FROM devices WHERE id=? AND online=1 AND enabled=1)`,
		sessionID, deviceID, userID, deviceID,
	)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return errors.New("device offline or another watch session active")
	}
	return nil
}

func (s *Store) EndWatch(sessionID string) error {
	_, err := s.DB.Exec(
		`UPDATE watch_sessions SET session_id=NULL,device_id=NULL,user_id=NULL WHERE id=1 AND session_id=?`,
		sessionID,
	)
	return err
}

func (s *Store) ResetWatch() error {
	_, err := s.DB.Exec(`UPDATE watch_sessions SET session_id=NULL,device_id=NULL,user_id=NULL WHERE id=1`)
	return err
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
