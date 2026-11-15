package logic

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
	"golang.org/x/crypto/bcrypt"
)

// Service 是应用层业务入口，负责管理用户、设备和临时票据的创建与校验。
type Service struct { Store *sqlite.Store }

// New 创建一个业务服务实例，实际数据访问依然由 sqlite.Store 负责。
func New(s *sqlite.Store) *Service { return &Service{Store:s} }

// EnsureAdmin 确保系统中至少存在一个管理员账号；如果不存在，按配置创建默认管理员。
func (s *Service) EnsureAdmin(username,password string) error {
	if username==""||password=="" { return errors.New("ADMIN_USERNAME and ADMIN_PASSWORD are required") }
	_,e:=s.Store.UserByName(username)
	if e==nil { return nil }
	passwordHash,err:=bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err!=nil { return err }
	return s.Store.CreateUser(username, string(passwordHash), "admin")
}

// Login 根据用户名和密码校验登录；成功后返回新的 session token 和用户对象。
func (s *Service) Login(username,password string) (string,sqlite.User,error) {
	u,e:=s.Store.UserByName(username)
	if e!=nil { return "",u,e }
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return "",u,errors.New("invalid username or password")
	}
	token,err:=randomSecret(32)
	if err!=nil { return "",u,err }
	if err:=s.Store.CreateSession(sqlite.Hash(token), u.ID, time.Now().Add(7*24*time.Hour)); err!=nil { return "",u,err }
	return token,u,nil
}

// Authenticate 通过会话 token 解析当前登录用户，供 HTTP 路由中间件使用。
func (s *Service) Authenticate(token string) (sqlite.User,error) {
	return s.Store.ValidateSession(sqlite.Hash(token))
}

// CreateUser 为普通用户创建账号，密码长度必须满足业务安全要求。
func (s *Service) CreateUser(username,password string) (string,error) {
	if len(password)<12 { return "",errors.New("password must be at least 12 characters") }
	h,err:=bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err!=nil { return "",err }
	if err:=s.Store.CreateUser(username, string(h), "user"); err!=nil { return "",err }
	return "created",nil
}

// randomID 生成一个随机的业务 ID，通常用于临时对象或前端展示 ID。
func randomID() string { x,_:=randomSecret(16); return x }

// randomSecret 生成指定长度的随机十六进制字符串。
func randomSecret(n int) (string,error) {
	b:=make([]byte,n)
	if _,e:=rand.Read(b); e!=nil { return "",e }
	return hex.EncodeToString(b),nil
}

// CreateDevice 创建设备并返回设备 ID 与初始化 token；token 只在设备第一次注册时返回。
func (s *Service) CreateDevice(name string) (id,token string,err error) {
	if name=="" { return "","",errors.New("name required") }
	id,err=randomSecret(16)
	if err!=nil { return }
	token,err=randomSecret(32)
	if err!=nil { return }
	if err=s.Store.CreateDevice(id, name, sqlite.Hash(token)); err!=nil { return }
	return id, token, nil
}

// CreateTicket 为设备或管理员创建一次性 WebSocket 连接票据，通常有效期为 60 秒。
func (s *Service) CreateTicket(typ,subject string) (string,error) {
	raw,e:=randomSecret(24)
	if e!=nil { return "",e }
	e=s.Store.NewTicket(sqlite.Hash(raw), typ, subject, time.Now().Add(60*time.Second))
	if e!=nil { return "",e }
	return raw,nil
}
