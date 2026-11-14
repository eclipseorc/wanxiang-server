package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/eclipseorc/wanxiang-server/internal/logic"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

type Router struct { svc *logic.Service }
func New(s *logic.Service) *Router { return &Router{svc:s} }
func (r *Router) Handler() http.Handler { m:=http.NewServeMux();m.HandleFunc("GET /api/health",r.health);m.HandleFunc("POST /api/admin/login",r.login);m.Handle("/api/admin/",r.auth(http.HandlerFunc(r.admin)));m.Handle("/api/device/",http.HandlerFunc(r.device));return m }
func writeJSON(w http.ResponseWriter,status int,v any){w.Header().Set("Content-Type","application/json");w.WriteHeader(status);_ = json.NewEncoder(w).Encode(v)}
func fail(w http.ResponseWriter,status int,msg string){writeJSON(w,status,map[string]string{"error":msg})}
func decode(r *http.Request,v any) error {d:=json.NewDecoder(http.MaxBytesReader(nil,r.Body,1<<20));d.DisallowUnknownFields();return d.Decode(v)}
func (r *Router) health(w http.ResponseWriter,_ *http.Request){writeJSON(w,200,map[string]any{"ok":true,"service":"http-api"})}
func bearer(req *http.Request) string { h:=req.Header.Get("Authorization");if !strings.HasPrefix(h,"Bearer "){return ""};return strings.TrimSpace(strings.TrimPrefix(h,"Bearer ")) }
func (r *Router) auth(next http.Handler) http.Handler {return http.HandlerFunc(func(w http.ResponseWriter,q *http.Request){u,e:=r.svc.Authenticate(bearer(q));if e!=nil{fail(w,401,"unauthorized");return};ctx:=contextWithUser(q,u);next.ServeHTTP(w,ctx)})}
func (r *Router) login(w http.ResponseWriter,q *http.Request){var x struct{Username string `json:"username"`;Password string `json:"password"`};if decode(q,&x)!=nil{fail(w,400,"invalid JSON");return};t,u,e:=r.svc.Login(x.Username,x.Password);if e!=nil{fail(w,401,"invalid username or password");return};writeJSON(w,200,map[string]string{"token":t,"userId":u.ID,"role":u.Role})}
func (r *Router) admin(w http.ResponseWriter,q *http.Request){u:=userFromContext(q);path:=strings.TrimPrefix(q.URL.Path,"/api/admin/");switch path {
case "ws-ticket": if q.Method!="POST"{fail(w,405,"POST required");return};t,e:=r.svc.CreateTicket("admin",u.ID);if e!=nil{fail(w,500,"ticket error");return};writeJSON(w,200,map[string]string{"ticket":t})
case "devices": if q.Method=="GET"{r.listDevices(w,u);return};if q.Method=="POST"{if u.Role!="admin"{fail(w,403,"admin only");return};var x struct{Name string `json:"name"`};if decode(q,&x)!=nil{fail(w,400,"invalid JSON");return};id,t,e:=r.svc.CreateDevice(x.Name);if e!=nil{fail(w,400,e.Error());return};writeJSON(w,201,map[string]string{"deviceId":id,"deviceToken":t});return};fail(w,405,"method not allowed")
case "users":if q.Method!="POST"||u.Role!="admin"{fail(w,403,"admin only");return};var x struct{Username string `json:"username"`;Password string `json:"password"`};if decode(q,&x)!=nil{fail(w,400,"invalid JSON");return};id,e:=r.svc.CreateUser(x.Username,x.Password);if e!=nil{fail(w,400,e.Error());return};writeJSON(w,201,map[string]string{"userId":id})
case "groups":if q.Method!="POST"||u.Role!="admin"{fail(w,403,"admin only");return};var x struct{Name string `json:"name"`};if decode(q,&x)!=nil||strings.TrimSpace(x.Name)==""{fail(w,400,"name required");return};id:=newID();if e:=r.svc.Store.CreateGroup(id,x.Name);e!=nil{fail(w,409,"could not create group");return};writeJSON(w,201,map[string]string{"groupId":id})
case "groups/users":if q.Method!="POST"||u.Role!="admin"{fail(w,403,"admin only");return};var x struct{UserID string `json:"userId"`;GroupID string `json:"groupId"`};if decode(q,&x)!=nil{fail(w,400,"invalid JSON");return};if e:=r.svc.Store.AddUserGroup(x.UserID,x.GroupID);e!=nil{fail(w,400,"invalid user or group");return};writeJSON(w,200,map[string]bool{"ok":true})
case "groups/devices":if q.Method!="POST"||u.Role!="admin"{fail(w,403,"admin only");return};var x struct{DeviceID string `json:"deviceId"`;GroupID string `json:"groupId"`};if decode(q,&x)!=nil{fail(w,400,"invalid JSON");return};if e:=r.svc.Store.AddDeviceGroup(x.DeviceID,x.GroupID);e!=nil{fail(w,400,"invalid device or group");return};writeJSON(w,200,map[string]bool{"ok":true})
case "group-access":if q.Method!="POST"||u.Role!="admin"{fail(w,403,"admin only");return};var x struct{UserGroupID string `json:"userGroupId"`;DeviceGroupID string `json:"deviceGroupId"`;CanView bool `json:"canView"`;CanManage bool `json:"canManage"`};if decode(q,&x)!=nil{fail(w,400,"invalid JSON");return};if e:=r.svc.Store.SetGroupAccess(x.UserGroupID,x.DeviceGroupID,x.CanView,x.CanManage);e!=nil{fail(w,400,"invalid group access");return};writeJSON(w,200,map[string]bool{"ok":true})
case "devices/disconnect","devices/enable":if q.Method!="POST"{fail(w,405,"POST required");return};var x struct{DeviceID string `json:"deviceId"`};if decode(q,&x)!=nil||x.DeviceID==""{fail(w,400,"deviceId required");return};ok,e:=r.svc.Store.CanAccess(u.ID,x.DeviceID,true);if e!=nil||!ok{fail(w,403,"forbidden");return};kind:="disconnect";if path=="devices/enable"{kind="enable"};if e=r.svc.Store.Enqueue(kind,x.DeviceID,u.ID);e!=nil{fail(w,500,"operation failed");return};writeJSON(w,202,map[string]string{"status":"queued"})
default: http.NotFound(w,q)} }
func (r *Router) listDevices(w http.ResponseWriter,u sqlite.User){ds,e:=r.svc.Store.ListDevices();if e!=nil{fail(w,500,"database error");return};out:=make([]sqlite.Device,0,len(ds));for _,d:=range ds{ok,er:=r.svc.Store.CanAccess(u.ID,d.ID,false);if er==nil&&ok{d.Status="";out=append(out,d)}};writeJSON(w,200,out)}
func (r *Router) device(w http.ResponseWriter,q *http.Request){if q.URL.Path!="/api/device/ws-ticket"||q.Method!="POST"{http.NotFound(w,q);return};var x struct{DeviceID string `json:"deviceId"`};if decode(q,&x)!=nil||x.DeviceID==""{fail(w,400,"deviceId required");return};ok,e:=r.svc.Store.DeviceAuth(x.DeviceID,sqlite.Hash(bearer(q)));if e!=nil||!ok{fail(w,401,"invalid device credentials");return};t,e:=r.svc.CreateTicket("device",x.DeviceID);if e!=nil{fail(w,500,"ticket error");return};writeJSON(w,200,map[string]string{"ticket":t})}
func newID()string{x,e:=randomID();if e!=nil{return ""};return x}
var errContext=errors.New("missing user context")
