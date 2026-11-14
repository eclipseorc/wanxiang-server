package sqlite

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

var ErrNotFound = errors.New("not found")

type Store struct { DB *sql.DB }
type User struct { ID, Username, PasswordHash, Role string }
type Device struct { ID, Name, Status string; Enabled, Online bool; LastSeen int64; Battery int; RTTMS int64; GroupIDs []string }
type Ticket struct { Type, SubjectID string }
type ControlEvent struct { ID int64; Kind, DeviceID string }

func NewStore(db *sql.DB) *Store { return &Store{DB:db} }
func Hash(s string) string { h:=sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func (s *Store) CreateUser(id, username, passwordHash, role string) error { _,e:=s.DB.Exec(`INSERT INTO users(id,username,password_hash,role) VALUES(?,?,?,?)`,id,username,passwordHash,role); return e }
func (s *Store) UserByName(name string) (User,error) { var u User; e:=s.DB.QueryRow(`SELECT id,username,password_hash,role FROM users WHERE username=?`,name).Scan(&u.ID,&u.Username,&u.PasswordHash,&u.Role); if e==sql.ErrNoRows { e=ErrNotFound }; return u,e }
func (s *Store) UserByID(id string) (User,error) { var u User; e:=s.DB.QueryRow(`SELECT id,username,password_hash,role FROM users WHERE id=?`,id).Scan(&u.ID,&u.Username,&u.PasswordHash,&u.Role); if e==sql.ErrNoRows {e=ErrNotFound}; return u,e }
func (s *Store) CreateSession(tokenHash,userID string, expiry time.Time) error { _,e:=s.DB.Exec(`INSERT INTO sessions(token_hash,user_id,expires_at) VALUES(?,?,?)`,tokenHash,userID,expiry.Unix()); return e }
func (s *Store) ValidateSession(hash string) (User,error) { var u User; e:=s.DB.QueryRow(`SELECT u.id,u.username,u.password_hash,u.role FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.expires_at>?`,hash,time.Now().Unix()).Scan(&u.ID,&u.Username,&u.PasswordHash,&u.Role); if e==sql.ErrNoRows { e=ErrNotFound }; return u,e }
func (s *Store) CreateDevice(id,name,tokenHash string) error { _,e:=s.DB.Exec(`INSERT INTO devices(id,name,token_hash,created_at) VALUES(?,?,?,?)`,id,name,tokenHash,time.Now().Unix()); return e }
func (s *Store) DeviceAuth(id, tokenHash string) (bool,error) { var n int; e:=s.DB.QueryRow(`SELECT count(*) FROM devices WHERE id=? AND token_hash=? AND enabled=1`,id,tokenHash).Scan(&n); return n==1,e }
func (s *Store) DeviceEnabled(id string) (bool,error) { var n int; e:=s.DB.QueryRow(`SELECT enabled FROM devices WHERE id=?`,id).Scan(&n); return n==1,e }
func (s *Store) UpdateDeviceState(id string, online bool, battery int, rtt int64, status string) error { n:=0; if online {n=1}; _,e:=s.DB.Exec(`UPDATE devices SET online=?,last_seen=?,battery=?,rtt_ms=?,status_json=? WHERE id=?`,n,time.Now().Unix(),battery,rtt,status,id); return e }
func (s *Store) SetOnline(id string, online bool) error { n:=0; if online {n=1}; _,e:=s.DB.Exec(`UPDATE devices SET online=?,last_seen=? WHERE id=?`,n,time.Now().Unix(),id); return e }
func (s *Store) ListDevices() ([]Device,error) { rows,e:=s.DB.Query(`SELECT id,name,enabled,online,last_seen,battery,rtt_ms,status_json FROM devices ORDER BY name,id`); if e!=nil{return nil,e}; defer rows.Close(); out:=[]Device{}; for rows.Next(){var d Device; var en,on int; if e=rows.Scan(&d.ID,&d.Name,&en,&on,&d.LastSeen,&d.Battery,&d.RTTMS,&d.Status);e!=nil{return nil,e}; d.Enabled=en==1; d.Online=on==1; gs,er:=s.DeviceGroups(d.ID); if er!=nil{return nil,er}; d.GroupIDs=gs; out=append(out,d)}; return out,rows.Err() }
func (s *Store) CreateGroup(id,name string) error { _,e:=s.DB.Exec(`INSERT INTO groups(id,name,created_at) VALUES(?,?,?)`,id,name,time.Now().Unix()); return e }
func (s *Store) AddUserGroup(userID,groupID string) error { _,e:=s.DB.Exec(`INSERT OR IGNORE INTO group_users(group_id,user_id) VALUES(?,?)`,groupID,userID); return e }
func (s *Store) AddDeviceGroup(deviceID,groupID string) error { _,e:=s.DB.Exec(`INSERT OR IGNORE INTO device_groups(group_id,device_id) VALUES(?,?)`,groupID,deviceID); return e }
func (s *Store) SetGroupAccess(ug,dg string,view,manage bool) error { v,m:=0,0; if view {v=1}; if manage {m=1}; _,e:=s.DB.Exec(`INSERT INTO group_access(user_group_id,device_group_id,can_view,can_manage) VALUES(?,?,?,?) ON CONFLICT(user_group_id,device_group_id) DO UPDATE SET can_view=excluded.can_view,can_manage=excluded.can_manage`,ug,dg,v,m); return e }
func (s *Store) CanAccess(userID,deviceID string,manage bool) (bool,error) { u,e:=s.UserByID(userID); if e!=nil{return false,e}; if u.Role=="admin" {return true,nil}; col:="can_view"; if manage {col="can_manage"}; q:=`SELECT EXISTS(SELECT 1 FROM group_users ug JOIN group_access ga ON ga.user_group_id=ug.group_id JOIN device_groups dg ON dg.group_id=ga.device_group_id WHERE ug.user_id=? AND dg.device_id=? AND ga.`+col+`=1)`; var ok int; e=s.DB.QueryRow(q,userID,deviceID).Scan(&ok); return ok==1,e }
func (s *Store) NewTicket(hash,typ,subject string, expiry time.Time) error { _,e:=s.DB.Exec(`INSERT INTO ws_tickets(ticket_hash,subject_type,subject_id,expires_at) VALUES(?,?,?,?)`,hash,typ,subject,expiry.Unix()); return e }
func (s *Store) ConsumeTicket(hash string) (Ticket,error) { tx,e:=s.DB.Begin(); if e!=nil{return Ticket{},e}; defer tx.Rollback(); var t Ticket; e=tx.QueryRow(`SELECT subject_type,subject_id FROM ws_tickets WHERE ticket_hash=? AND expires_at>?`,hash,time.Now().Unix()).Scan(&t.Type,&t.SubjectID); if e!=nil {if e==sql.ErrNoRows{e=ErrNotFound}; return Ticket{},e}; if _,e=tx.Exec(`DELETE FROM ws_tickets WHERE ticket_hash=?`,hash); e!=nil{return Ticket{},e}; return t,tx.Commit() }
func (s *Store) Enqueue(kind,deviceID,actor string) error { tx,e:=s.DB.Begin(); if e!=nil{return e}; defer tx.Rollback(); if _,e=tx.Exec(`INSERT INTO control_events(kind,device_id,created_at) VALUES(?,?,?)`,kind,deviceID,time.Now().Unix()); e!=nil{return e}; if kind=="disconnect" { if _,e=tx.Exec(`UPDATE devices SET enabled=0 WHERE id=?`,deviceID);e!=nil{return e} }; if kind=="enable" { if _,e=tx.Exec(`UPDATE devices SET enabled=1 WHERE id=?`,deviceID);e!=nil{return e} }; if _,e=tx.Exec(`INSERT INTO audit_logs(actor_id,action,device_id,created_at) VALUES(?,?,?,?)`,actor,kind,deviceID,time.Now().Unix());e!=nil{return e}; return tx.Commit() }
func (s *Store) PendingEvents() ([]ControlEvent,error) { rows,e:=s.DB.Query(`SELECT id,kind,device_id FROM control_events WHERE processed=0 ORDER BY id LIMIT 100`); if e!=nil{return nil,e}; defer rows.Close(); out:=[]ControlEvent{}; for rows.Next(){var x ControlEvent;if e=rows.Scan(&x.ID,&x.Kind,&x.DeviceID);e!=nil{return nil,e};out=append(out,x)};return out,rows.Err() }
func (s *Store) MarkEvent(id int64) error { _,e:=s.DB.Exec(`UPDATE control_events SET processed=1 WHERE id=?`,id); return e }
func (s *Store) StartWatch(sessionID,deviceID,userID string) error { res,e:=s.DB.Exec(`UPDATE watch_sessions SET session_id=?,device_id=?,user_id=? WHERE id=1 AND session_id IS NULL AND EXISTS(SELECT 1 FROM devices WHERE id=? AND online=1 AND enabled=1)`,sessionID,deviceID,userID,deviceID); if e!=nil{return e}; n,_:=res.RowsAffected(); if n==0{return errors.New("device offline or another watch session active")}; return nil }
func (s *Store) EndWatch(sessionID string) error { _,e:=s.DB.Exec(`UPDATE watch_sessions SET session_id=NULL,device_id=NULL,user_id=NULL WHERE id=1 AND session_id=?`,sessionID); return e }
func (s *Store) CurrentWatch() (sessionID,deviceID,userID string) { _=s.DB.QueryRow(`SELECT COALESCE(session_id,''),COALESCE(device_id,''),COALESCE(user_id,'') FROM watch_sessions WHERE id=1`).Scan(&sessionID,&deviceID,&userID); return }
func (s *Store) DeviceGroups(deviceID string) ([]string,error) { rows,e:=s.DB.Query(`SELECT group_id FROM device_groups WHERE device_id=?`,deviceID); if e!=nil{return nil,e}; defer rows.Close(); a:=[]string{}; for rows.Next(){var x string;if e=rows.Scan(&x);e!=nil{return nil,e};a=append(a,x)};return a,rows.Err() }
