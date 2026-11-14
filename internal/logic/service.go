package logic

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
	"golang.org/x/crypto/bcrypt"
)

type Service struct { Store *sqlite.Store }
func New(s *sqlite.Store) *Service { return &Service{Store:s} }
func (s *Service) EnsureAdmin(username,password string) error { if username==""||password=="" { return errors.New("ADMIN_USERNAME and ADMIN_PASSWORD are required") }; _,e:=s.Store.UserByName(username); if e==nil{return nil}; if !errors.Is(e,sqlite.ErrNotFound){return e}; hash,err:=bcrypt.GenerateFromPassword([]byte(password),bcrypt.DefaultCost);if err!=nil{return err};return s.Store.CreateUser(randomID(),username,string(hash),"admin") }
func (s *Service) Login(username,password string) (string,sqlite.User,error) { u,e:=s.Store.UserByName(username);if e!=nil{return "",u,e};if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash),[]byte(password))!=nil{return "",u,errors.New("invalid credentials")}; token,e:=randomSecret(32);if e!=nil{return "",u,e};e=s.Store.CreateSession(sqlite.Hash(token),u.ID,time.Now().Add(12*time.Hour));return token,u,e }
func (s *Service) Authenticate(token string) (sqlite.User,error) { return s.Store.ValidateSession(sqlite.Hash(token)) }
func (s *Service) CreateUser(username,password string) (string,error) { if len(password)<12{return "",errors.New("password must be at least 12 characters")};h,e:=bcrypt.GenerateFromPassword([]byte(password),bcrypt.DefaultCost);if e!=nil{return "",e};id:=randomID();return id,s.Store.CreateUser(id,username,string(h),"user") }
func randomID() string { x,_:=randomSecret(16); return x }
func randomSecret(n int) (string,error) { b:=make([]byte,n);if _,e:=rand.Read(b);e!=nil{return "",e};return hex.EncodeToString(b),nil }
func (s *Service) CreateDevice(name string) (id,token string,err error) { if name=="" {return "","",errors.New("name required")};id,err=randomSecret(16);if err!=nil{return};token,err=randomSecret(32);if err!=nil{return};err=s.Store.CreateDevice(id,name,sqlite.Hash(token));return }
func (s *Service) CreateTicket(typ,subject string) (string,error) { raw,e:=randomSecret(24);if e!=nil{return "",e};e=s.Store.NewTicket(sqlite.Hash(raw),typ,subject,time.Now().Add(60*time.Second));return raw,e }
