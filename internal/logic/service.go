// Package logic 实现管理 API 的应用层业务规则：账号、密码、设备与一次性票据。
package logic

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
	"golang.org/x/crypto/bcrypt"
)

const (
	// bcrypt 只使用密码的前 72 字节，更长的输入没有额外安全收益，反而会直接报错。
	maxPasswordBytes = 72
	minPasswordBytes = 12
	maxUsernameBytes = 128

	// 会话与一次性票据的有效期；过期记录会在登录和签发票据时节流清理。
	sessionLifetime = 12 * time.Hour
	ticketLifetime  = 60 * time.Second
	// 清理间隔：ticket 签发是高频路径，每次都执行 DELETE 会与转发进程的
	// 400ms 轮询争抢 SQLite 写锁，在高并发下放大成数秒级阻塞。
	purgeInterval = 5 * time.Minute
)

// InputError 表示可以安全返回给调用方的输入校验错误。
// 其它错误一律视为内部错误，底层细节只写日志，不能出现在 HTTP 响应里。
type InputError struct{ Message string }

func (e *InputError) Error() string { return e.Message }

func invalid(format string, args ...any) error {
	return &InputError{Message: fmt.Sprintf(format, args...)}
}

// Service 是应用层业务入口，负责管理用户、设备和临时票据的创建与校验。
type Service struct {
	Store *sqlite.Store

	purgeMu   sync.Mutex
	lastPurge time.Time
}

// New 创建一个业务服务实例，实际数据访问依然由 sqlite.Store 负责。
func New(s *sqlite.Store) *Service { return &Service{Store: s} }

// EnsureAdmin 确保系统中存在配置的管理员账号，仅在账号不存在时创建。
// 已有账号的密码不会随 .env 变化自动更新，避免配置误改导致账号被接管。
func (s *Service) EnsureAdmin(username, password string) error {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return errors.New("ADMIN_USERNAME and ADMIN_PASSWORD are required")
	}

	existing, err := s.Store.UserByName(username)
	switch {
	case err == nil:
		// 同名账号若不是管理员，说明配置被普通用户占用；静默跳过会让系统没有管理员。
		if existing.Role != "admin" {
			return fmt.Errorf("ADMIN_USERNAME %q is already used by a non-admin account (role=%q)", username, existing.Role)
		}
		return nil
	case !errors.Is(err, sqlite.ErrNotFound):
		return fmt.Errorf("look up administrator: %w", err)
	}

	if err := validatePassword(password); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash administrator password: %w", err)
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	return s.Store.CreateUser(id, username, string(hash), "admin")
}

// maybePurgeExpired 按固定间隔惰性清理过期凭据，避免高频签发路径反复写库。
// 首次调用会立即执行（lastPurge 为零值），之后每 5 分钟最多一次。
func (s *Service) maybePurgeExpired() {
	s.purgeMu.Lock()
	defer s.purgeMu.Unlock()
	if time.Since(s.lastPurge) < purgeInterval {
		return
	}
	_ = s.Store.PurgeExpired()
	s.lastPurge = time.Now()
}

// Login 校验用户名和密码，成功后签发新的会话 token 并清理过期凭据。
func (s *Service) Login(username, password string) (string, sqlite.User, error) {
	s.maybePurgeExpired()

	u, err := s.Store.UserByName(strings.TrimSpace(username))
	if err != nil {
		// 用户名不存在时也执行一次等成本的 bcrypt 校验，抹平登录耗时差，
		// 避免通过响应时间枚举账号。
		_ = bcrypt.CompareHashAndPassword(dummyPasswordHash(), []byte(password))
		return "", u, err
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return "", u, errors.New("invalid username or password")
	}

	token, err := randomSecret(32)
	if err != nil {
		return "", u, err
	}
	if err := s.Store.CreateSession(sqlite.Hash(token), u.ID, time.Now().Add(sessionLifetime)); err != nil {
		return "", u, err
	}
	return token, u, nil
}

// Authenticate 通过会话 token 解析当前登录用户，供 HTTP 路由中间件使用。
func (s *Service) Authenticate(token string) (sqlite.User, error) {
	if token == "" {
		return sqlite.User{}, sqlite.ErrNotFound
	}
	return s.Store.ValidateSession(sqlite.Hash(token))
}

// CreateUser 创建普通用户，返回新用户的真实 ID 供后续分组授权使用。
func (s *Service) CreateUser(username, password string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", invalid("username required")
	}
	if len(username) > maxUsernameBytes {
		return "", invalid("username is too long")
	}
	if err := validatePassword(password); err != nil {
		return "", err
	}
	// 提前给出可读的重名提示；数据库 UNIQUE 约束仍是最终防线。
	if _, err := s.Store.UserByName(username); err == nil {
		return "", invalid("username already exists")
	} else if !errors.Is(err, sqlite.ErrNotFound) {
		return "", fmt.Errorf("check existing username: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	if err := s.Store.CreateUser(id, username, string(hash), "user"); err != nil {
		return "", err
	}
	return id, nil
}

// RegisterDevice 登记一台设备：设备 ID 由 App 本地生成并上报，型号可选。
// 首次上报即入库，重复上报只更新型号，不做任何设备令牌下发。
func (s *Service) RegisterDevice(deviceID, model string) error {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return invalid("deviceId required")
	}
	if len(deviceID) > 128 {
		return invalid("deviceId is too long")
	}
	return s.Store.UpsertDevice(deviceID, strings.TrimSpace(model))
}

// CreateTicket 为设备或管理员签发一次性 WebSocket 连接票据，有效期 60 秒。
func (s *Service) CreateTicket(typ, subject string) (string, error) {
	s.maybePurgeExpired()
	raw, err := randomSecret(24)
	if err != nil {
		return "", err
	}
	if err := s.Store.NewTicket(sqlite.Hash(raw), typ, subject, time.Now().Add(ticketLifetime)); err != nil {
		return "", err
	}
	return raw, nil
}

// validatePassword 统一密码策略。长度按字节计算，避免 bcrypt 截断或返回 ErrPasswordTooLong。
func validatePassword(password string) error {
	if len(password) < minPasswordBytes {
		return invalid("password must be at least %d characters", minPasswordBytes)
	}
	if len(password) > maxPasswordBytes {
		return invalid("password must be at most %d bytes", maxPasswordBytes)
	}
	return nil
}

// randomID 生成随机的业务 ID，用于用户、设备等主键。
func randomID() (string, error) { return randomSecret(16) }

var (
	dummyOnce sync.Once
	dummyHash []byte
)

// dummyPasswordHash 懒生成一个固定成本的 bcrypt 哈希，仅用于账号枚举防护。
// 它永远不匹配任何真实密码，只是让「用户不存在」与「密码错误」耗时接近。
func dummyPasswordHash() []byte {
	dummyOnce.Do(func() {
		hash, err := bcrypt.GenerateFromPassword([]byte("timing-guard-placeholder"), bcrypt.DefaultCost)
		if err != nil {
			hash = []byte("not-a-valid-bcrypt-hash")
		}
		dummyHash = hash
	})
	return dummyHash
}

// randomSecret 生成指定长度的随机十六进制字符串。
func randomSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return hex.EncodeToString(b), nil
}
