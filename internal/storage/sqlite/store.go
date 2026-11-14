package sqlite

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func Open(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// SQLite is a single-writer database. A single connection per process avoids
	// surprising lock contention; busy_timeout lets the two local services wait.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
		if _, err = db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if err = db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func InitSchema(db *sql.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('admin','user')), created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sessions (token_hash TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, expires_at INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON sessions(expires_at)`,
		`CREATE TABLE IF NOT EXISTS devices (id TEXT PRIMARY KEY, name TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE, enabled INTEGER NOT NULL DEFAULT 1, online INTEGER NOT NULL DEFAULT 0, last_seen INTEGER NOT NULL DEFAULT 0, battery INTEGER NOT NULL DEFAULT 0, rtt_ms INTEGER NOT NULL DEFAULT 0, status_json TEXT NOT NULL DEFAULT '{}', created_at INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_devices_online ON devices(online)`,
		`CREATE TABLE IF NOT EXISTS groups (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS group_users (group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, PRIMARY KEY(group_id,user_id))`,
		`CREATE TABLE IF NOT EXISTS device_groups (group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE, device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE, PRIMARY KEY(group_id,device_id))`,
		`CREATE TABLE IF NOT EXISTS group_access (user_group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE, device_group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE, can_view INTEGER NOT NULL DEFAULT 0, can_manage INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(user_group_id,device_group_id))`,
		`CREATE TABLE IF NOT EXISTS ws_tickets (ticket_hash TEXT PRIMARY KEY, subject_type TEXT NOT NULL, subject_id TEXT NOT NULL, expires_at INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_ws_tickets_expiry ON ws_tickets(expires_at)`,
		`CREATE TABLE IF NOT EXISTS control_events (id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, device_id TEXT NOT NULL, actor_id TEXT NOT NULL, created_at INTEGER NOT NULL, processed INTEGER NOT NULL DEFAULT 0)`,
		`CREATE INDEX IF NOT EXISTS idx_control_pending ON control_events(processed,id)`,
		`CREATE TABLE IF NOT EXISTS watch_sessions (id INTEGER PRIMARY KEY CHECK(id=1), session_id TEXT, device_id TEXT, user_id TEXT)`,
		`INSERT OR IGNORE INTO watch_sessions(id,session_id,device_id,user_id) VALUES(1,NULL,NULL,NULL)`,
		`CREATE TABLE IF NOT EXISTS audit_logs (id INTEGER PRIMARY KEY AUTOINCREMENT, actor_id TEXT, action TEXT NOT NULL, device_id TEXT, detail TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL)`,
	}
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

var ErrNotFound = errors.New("not found")

type Store struct{ DB *sql.DB }
type User struct{ ID, Username, PasswordHash, Role string }
type Device struct {
	ID, Name, Status string
	Enabled, Online  bool
	LastSeen         int64
	Battery          int
	RTTMS            int64
	GroupIDs         []string
}
type Group struct{ ID, Name string }
type Ticket struct{ Type, SubjectID string }
type ControlEvent struct {
	ID                  int64
	Kind, DeviceID, ActorID string
}

func NewStore(db *sql.DB) *Store { return &Store{DB: db} }
func Hash(s string) string { sum := sha256Sum([]byte(s)); return hexEncode(sum) }

func (s *Store) CreateUser(id, username, passwordHash, role string) error {
	_, err := s.DB.Exec(`INSERT INTO users(id,username,password_hash,role,created_at) VALUES(?,?,?,?,?)`, id, username, passwordHash, role, timeNowUnix())
	return err
}
func (s *Store) UserByName(name string) (User, error) {
	var u User
	err := s.DB.QueryRow(`SELECT id,username,password_hash,role FROM users WHERE username=?`, name).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role)
	return u, notFound(err)
}
func (s *Store) UserByID(id string) (User, error) {
	var u User
	err := s.DB.QueryRow(`SELECT id,username,password_hash,role FROM users WHERE id=?`, id).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role)
	return u, notFound(err)
}
func (s *Store) CreateSession(tokenHash, userID string, expiry time.Time) error {
	_, err := s.DB.Exec(`INSERT INTO sessions(token_hash,user_id,expires_at) VALUES(?,?,?)`, tokenHash, userID, expiry.Unix())
	return err
}
func (s *Store) ValidateSession(hash string) (User, error) {
	var u User
	err := s.DB.QueryRow(`SELECT u.id,u.username,u.password_hash,u.role FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.expires_at>?`, hash, time.Now().Unix()).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role)
	return u, notFound(err)
}
func (s *Store) DeleteSession(hash string) error { _, err := s.DB.Exec(`DELETE FROM sessions WHERE token_hash=?`, hash); return err }
func (s *Store) CreateDevice(id, name, tokenHash string) error {
	_, err := s.DB.Exec(`INSERT INTO devices(id,name,token_hash,created_at) VALUES(?,?,?,?)`, id, name, tokenHash, time.Now().Unix())
	return err
}
func (s *Store) DeviceAuth(id, tokenHash string) (bool, error) {
	var n int
	err := s.DB.QueryRow(`SELECT count(*) FROM devices WHERE id=? AND token_hash=? AND enabled=1`, id, tokenHash).Scan(&n)
	return n == 1, err
}
func (s *Store) DeviceEnabled(id string) (bool, error) {
	var enabled int
	err := s.DB.QueryRow(`SELECT enabled FROM devices WHERE id=?`, id).Scan(&enabled)
	if err == sql.ErrNoRows { return false, ErrNotFound }
	return enabled == 1, err
}
func (s *Store) UpdateDeviceState(id string, battery int, rtt int64, status string) error {
	_, err := s.DB.Exec(`UPDATE devices SET online=1,last_seen=?,battery=?,rtt_ms=?,status_json=? WHERE id=?`, time.Now().Unix(), battery, rtt, status, id)
	return err
}
func (s *Store) SetOnline(id string, online bool) error {
	n := 0
	if online { n = 1 }
	_, err := s.DB.Exec(`UPDATE devices SET online=?,last_seen=? WHERE id=?`, n, time.Now().Unix(), id)
	return err
}
func (s *Store) ListDevices() ([]Device, error) {
	rows, err := s.DB.Query(`SELECT d.id,d.name,d.enabled,d.online,d.last_seen,d.battery,d.rtt_ms,d.status_json,group_concat(DISTINCT dg.group_id) FROM devices d LEFT JOIN device_groups dg ON dg.device_id=d.id GROUP BY d.id ORDER BY d.name,d.id`)
	if err != nil { return nil, err }
	defer rows.Close()
	out := make([]Device, 0)
	for rows.Next() {
		var d Device
		var enabled, online int
		var groups sql.NullString
		if err = rows.Scan(&d.ID, &d.Name, &enabled, &online, &d.LastSeen, &d.Battery, &d.RTTMS, &d.Status, &groups); err != nil { return nil, err }
		d.Enabled, d.Online = enabled == 1, online == 1
		if groups.Valid && groups.String != "" { d.GroupIDs = strings.Split(groups.String, ",") } else { d.GroupIDs = []string{} }
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *Store) CreateGroup(id, name string) error {
	_, err := s.DB.Exec(`INSERT INTO groups(id,name,created_at) VALUES(?,?,?)`, id, name, time.Now().Unix())
	return err
}
func (s *Store) ListGroups() ([]Group, error) {
	rows, err := s.DB.Query(`SELECT id,name FROM groups ORDER BY name,id`)
	if err != nil { return nil, err }
	defer rows.Close()
	out := make([]Group, 0)
	for rows.Next() { var g Group; if err = rows.Scan(&g.ID, &g.Name); err != nil { return nil, err }; out = append(out, g) }
	return out, rows.Err()
}
func (s *Store) AddUserGroup(userID, groupID string) error {
	_, err := s.DB.Exec(`INSERT OR IGNORE INTO group_users(group_id,user_id) VALUES(?,?)`, groupID, userID)
	return err
}
func (s *Store) AddDeviceGroup(deviceID, groupID string) error {
	_, err := s.DB.Exec(`INSERT OR IGNORE INTO device_groups(group_id,device_id) VALUES(?,?)`, groupID, deviceID)
	return err
}
func (s *Store) SetGroupAccess(userGroup, deviceGroup string, canView, canManage bool) error {
	if canManage { canView = true }
	view, manage := 0, 0
	if canView { view = 1 }; if canManage { manage = 1 }
	_, err := s.DB.Exec(`INSERT INTO group_access(user_group_id,device_group_id,can_view,can_manage) VALUES(?,?,?,?) ON CONFLICT(user_group_id,device_group_id) DO UPDATE SET can_view=excluded.can_view,can_manage=excluded.can_manage`, userGroup, deviceGroup, view, manage)
	return err
}
func (s *Store) CanAccess(userID, deviceID string, manage bool) (bool, error) {
	u, err := s.UserByID(userID)
	if err != nil { return false, err }
	if u.Role == "admin" { return true, nil }
	column := "can_view"
	if manage { column = "can_manage" }
	query := `SELECT EXISTS(SELECT 1 FROM group_users ug JOIN group_access ga ON ga.user_group_id=ug.group_id JOIN device_groups dg ON dg.group_id=ga.device_group_id WHERE ug.user_id=? AND dg.device_id=? AND ga.` + column + `=1)`
	var allowed int
	err = s.DB.QueryRow(query, userID, deviceID).Scan(&allowed)
	return allowed == 1, err
}
func (s *Store) NewTicket(hash, typ, subject string, expiry time.Time) error {
	_, err := s.DB.Exec(`INSERT INTO ws_tickets(ticket_hash,subject_type,subject_id,expires_at) VALUES(?,?,?,?)`, hash, typ, subject, expiry.Unix())
	return err
}
func (s *Store) ConsumeTicket(hash string) (Ticket, error) {
	tx, err := s.DB.Begin()
	if err != nil { return Ticket{}, err }
	defer tx.Rollback()
	var ticket Ticket
	err = tx.QueryRow(`SELECT subject_type,subject_id FROM ws_tickets WHERE ticket_hash=? AND expires_at>?`, hash, time.Now().Unix()).Scan(&ticket.Type, &ticket.SubjectID)
	if err != nil { return Ticket{}, notFound(err) }
	if _, err = tx.Exec(`DELETE FROM ws_tickets WHERE ticket_hash=?`, hash); err != nil { return Ticket{}, err }
	return ticket, tx.Commit()
}
func (s *Store) Enqueue(kind, deviceID, actor string) error {
	tx, err := s.DB.Begin()
	if err != nil { return err }
	defer tx.Rollback()
	if kind == "disconnect" {
		res, e := tx.Exec(`UPDATE devices SET enabled=0,online=0 WHERE id=?`, deviceID); if e != nil { return e }; n, _ := res.RowsAffected(); if n == 0 { return ErrNotFound }
	} else if kind == "enable" {
		res, e := tx.Exec(`UPDATE devices SET enabled=1 WHERE id=?`, deviceID); if e != nil { return e }; n, _ := res.RowsAffected(); if n == 0 { return ErrNotFound }
	} else { return errors.New("unsupported control event") }
	if _, err = tx.Exec(`INSERT INTO control_events(kind,device_id,actor_id,created_at) VALUES(?,?,?,?)`, kind, deviceID, actor, time.Now().Unix()); err != nil { return err }
	if _, err = tx.Exec(`INSERT INTO audit_logs(actor_id,action,device_id,created_at) VALUES(?,?,?,?)`, actor, kind, deviceID, time.Now().Unix()); err != nil { return err }
	return tx.Commit()
}
func (s *Store) PendingEvents() ([]ControlEvent, error) {
	rows, err := s.DB.Query(`SELECT id,kind,device_id,actor_id FROM control_events WHERE processed=0 ORDER BY id LIMIT 100`)
	if err != nil { return nil, err }; defer rows.Close()
	out := make([]ControlEvent, 0)
	for rows.Next() { var ev ControlEvent; if err = rows.Scan(&ev.ID,&ev.Kind,&ev.DeviceID,&ev.ActorID); err != nil { return nil, err }; out=append(out,ev) }
	return out, rows.Err()
}
func (s *Store) MarkEvent(id int64) error { _, err := s.DB.Exec(`UPDATE control_events SET processed=1 WHERE id=?`, id); return err }
func (s *Store) StartWatch(sessionID, deviceID, userID string) error {
	res, err := s.DB.Exec(`UPDATE watch_sessions SET session_id=?,device_id=?,user_id=? WHERE id=1 AND session_id IS NULL AND EXISTS(SELECT 1 FROM devices WHERE id=? AND online=1 AND enabled=1)`, sessionID,deviceID,userID,deviceID)
	if err != nil { return err }; n,_:=res.RowsAffected(); if n==0 { return errors.New("device offline or another watch session active") }; return nil
}
func (s *Store) EndWatch(sessionID string) error { _, err := s.DB.Exec(`UPDATE watch_sessions SET session_id=NULL,device_id=NULL,user_id=NULL WHERE id=1 AND session_id=?`, sessionID); return err }
func (s *Store) ResetWatch() error { _, err := s.DB.Exec(`UPDATE watch_sessions SET session_id=NULL,device_id=NULL,user_id=NULL WHERE id=1`); return err }
func (s *Store) Audit(actor, action, deviceID, detail string) error { _, err := s.DB.Exec(`INSERT INTO audit_logs(actor_id,action,device_id,detail,created_at) VALUES(?,?,?,?,?)`, actor,action,deviceID,detail,time.Now().Unix()); return err }

func notFound(err error) error { if errors.Is(err, sql.ErrNoRows) { return ErrNotFound }; return err }
