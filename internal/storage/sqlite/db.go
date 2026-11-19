// Package sqlite 提供 SQLite 数据库连接、表结构初始化、迁移和数据读写操作。
// Open 创建数据库连接；InitSchema 准备表结构；Store 实现业务所需的数据访问方法。
package sqlite

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // 注册纯 Go 的 SQLite 驱动，避免依赖 CGO。
)

// Open 打开（必要时创建）SQLite 数据库并统一连接行为。
// 限制为单连接可避免多进程/多协程写入时的锁竞争被放大；WAL 让读不阻塞写，
// busy_timeout 给跨进程写入留出 5 秒等待窗口，外键约束保证分组关系级联清理。
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
	// busy_timeout 必须先设置：后面的 journal_mode 需要短暂独占锁，
	// 两个服务进程同时启动时可能立刻返回 SQLITE_BUSY。
	for _, pragma := range []string{"PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
		if _, err = db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	// journal_mode=WAL 的锁不受 busy_timeout 完全保护，做有限重试；
	// 若始终失败，由 OpenReady 关闭连接后整体重开。
	for attempt := 0; attempt < 3; attempt++ {
		if _, err = db.Exec("PRAGMA journal_mode=WAL"); err == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// OpenReady 打开数据库并完成表结构初始化，供两个服务进程启动时使用。
// 两个进程同时启动一个全新的 SQLite 文件时，第二个进程可能在创建
// WAL/SHM 共享文件的瞬间拿到 SQLITE_BUSY 或 SQLITE_READONLY；每次失败
// 都关闭连接整体重开，避免连接停留在错误状态。
func OpenReady(path string) (*sql.DB, error) {
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		db, err := Open(path)
		if err == nil {
			if err = InitSchema(db); err == nil {
				return db, nil
			}
			_ = db.Close()
		}
		lastErr = err
		time.Sleep(300 * time.Millisecond)
	}
	return nil, fmt.Errorf("initialize sqlite database after retries: %w", lastErr)
}

// InitSchema 幂等地建立全部表与索引，并对早期原型创建的数据库做增量迁移。
func InitSchema(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users(id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('admin','user')), created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS sessions(token_hash TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, expires_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS devices(id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1, online INTEGER NOT NULL DEFAULT 0, last_seen INTEGER NOT NULL DEFAULT 0, battery INTEGER NOT NULL DEFAULT 0, rtt_ms INTEGER NOT NULL DEFAULT 0, status_json TEXT NOT NULL DEFAULT '{}', adb_ip TEXT NOT NULL DEFAULT '', adb_port INTEGER NOT NULL DEFAULT 0, adb_online INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS groups(id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS group_users(group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, PRIMARY KEY(group_id,user_id))`,
		`CREATE TABLE IF NOT EXISTS device_groups(group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE, device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE, PRIMARY KEY(group_id,device_id))`,
		`CREATE TABLE IF NOT EXISTS group_access(user_group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE, device_group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE, can_view INTEGER NOT NULL DEFAULT 0, can_manage INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(user_group_id,device_group_id))`,
		`CREATE TABLE IF NOT EXISTS ws_tickets(ticket_hash TEXT PRIMARY KEY, subject_type TEXT NOT NULL, subject_id TEXT NOT NULL, expires_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sync_groups(id TEXT PRIMARY KEY, name TEXT NOT NULL, master_id TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sync_group_members(sync_group_id TEXT NOT NULL REFERENCES sync_groups(id) ON DELETE CASCADE, device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE, sort_order INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(sync_group_id,device_id))`,
		`CREATE TABLE IF NOT EXISTS audit_logs(id INTEGER PRIMARY KEY AUTOINCREMENT, actor_id TEXT, action TEXT NOT NULL, device_id TEXT, detail TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON sessions(expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_devices_online ON devices(online)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}

	// 旧库迁移：早期原型的 devices 表带 token_hash 列（服务端下发设备令牌），
	// 现改为设备自注册（deviceId + model）。SQLite 的 DROP COLUMN 需要较新版本，
	// 这里用「检测后重建表」的方式迁移，避免依赖版本特性。
	if err := migrateDevicesTable(db); err != nil {
		return err
	}

	// 增量迁移：为旧库补齐 adb_ip / adb_online 两列（adb 连接状态持久化）。
	// 用 additive ALTER TABLE ADD COLUMN，检测列不存在才加，幂等。
	if err := addColumnIfMissing(db, "devices", "adb_ip", `TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := addColumnIfMissing(db, "devices", "adb_online", `INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	// 增量迁移：为旧库补齐 adb_port 列（无线调试连接端口，拼真实 serial 用）。
	// 无线调试端口是动态分配的，不能假设固定 5555，因此单独存一列。
	if err := addColumnIfMissing(db, "devices", "adb_port", `INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	return nil
}

// addColumnIfMissing 检测表是否已有指定列，缺失时用 ALTER TABLE ADD COLUMN 补齐。
// SQLite 不支持 IF NOT EXISTS 加列，需先查 pragma_table_info。
func addColumnIfMissing(db *sql.DB, table, column, def string) error {
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, column,
	).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	_, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, def))
	return err
}

// migrateDevicesTable 把旧版 devices 表迁移到无 token_hash 的形态：
// 若存在 token_hash 列则重建表并补齐 model 列。只读旧数据，不丢设备/分组关系。
func migrateDevicesTable(db *sql.DB) error {
	var hasToken int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('devices') WHERE name='token_hash'`).Scan(&hasToken); err != nil {
		return err
	}
	if hasToken == 0 {
		return nil
	}

	// 新表结构：去掉 token_hash，新增 model 列（默认空串）。
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS devices_new(
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			online INTEGER NOT NULL DEFAULT 0,
			last_seen INTEGER NOT NULL DEFAULT 0,
			battery INTEGER NOT NULL DEFAULT 0,
			rtt_ms INTEGER NOT NULL DEFAULT 0,
			status_json TEXT NOT NULL DEFAULT '{}',
			created_at INTEGER NOT NULL
		)`)
	if err != nil {
		return err
	}
	if _, err := db.Exec(`
		INSERT OR IGNORE INTO devices_new(id,name,model,enabled,online,last_seen,battery,rtt_ms,status_json,created_at)
		SELECT id,name,'',enabled,online,last_seen,battery,rtt_ms,status_json,created_at FROM devices`); err != nil {
		return err
	}
	if _, err := db.Exec(`DROP TABLE devices`); err != nil {
		return err
	}
	if _, err := db.Exec(`ALTER TABLE devices_new RENAME TO devices`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_devices_online ON devices(online)`); err != nil {
		return err
	}
	return nil
}
