package sqlite

import (
	"database/sql"
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
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users(id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('admin','user')), created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS sessions(token_hash TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, expires_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS devices(id TEXT PRIMARY KEY, name TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE, enabled INTEGER NOT NULL DEFAULT 1, online INTEGER NOT NULL DEFAULT 0, last_seen INTEGER NOT NULL DEFAULT 0, battery INTEGER NOT NULL DEFAULT 0, rtt_ms INTEGER NOT NULL DEFAULT 0, status_json TEXT NOT NULL DEFAULT '{}', created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS groups(id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS group_users(group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, PRIMARY KEY(group_id,user_id))`,
		`CREATE TABLE IF NOT EXISTS device_groups(group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE, device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE, PRIMARY KEY(group_id,device_id))`,
		`CREATE TABLE IF NOT EXISTS group_access(user_group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE, device_group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE, can_view INTEGER NOT NULL DEFAULT 0, can_manage INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(user_group_id,device_group_id))`,
		`CREATE TABLE IF NOT EXISTS ws_tickets(ticket_hash TEXT PRIMARY KEY, subject_type TEXT NOT NULL, subject_id TEXT NOT NULL, expires_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS control_events(id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, device_id TEXT NOT NULL, actor_id TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, processed INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS watch_sessions(id INTEGER PRIMARY KEY CHECK(id=1), session_id TEXT, device_id TEXT, user_id TEXT)`,
		`INSERT OR IGNORE INTO watch_sessions(id,session_id,device_id,user_id) VALUES(1,NULL,NULL,NULL)`,
		`CREATE TABLE IF NOT EXISTS audit_logs(id INTEGER PRIMARY KEY AUTOINCREMENT, actor_id TEXT, action TEXT NOT NULL, device_id TEXT, detail TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON sessions(expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_devices_online ON devices(online)`,
		`CREATE INDEX IF NOT EXISTS idx_control_pending ON control_events(processed,id)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}
	// Upgrade databases created by the first prototype, whose control_events
	// table did not yet store the actor that initiated an operation.
	var hasActor int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('control_events') WHERE name='actor_id'`).Scan(&hasActor); err != nil {
		return err
	}
	if hasActor == 0 {
		if _, err := db.Exec(`ALTER TABLE control_events ADD COLUMN actor_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}
