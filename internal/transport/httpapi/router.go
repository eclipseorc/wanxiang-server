package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/eclipseorc/wanxiang-server/internal/logic"
	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
)

type Router struct { svc *logic.Service }
func New(s *logic.Service) *Router { return &Router{svc:s} }
func (r *Router) Handler() http.Handler {
	m:=http.NewServeMux()
	m.HandleFunc("GET /api/health",r.health)
	m.HandleFunc("POST /api/admin/login",r.login)
	m.Handle("/api/admin/",r.auth(http.HandlerFunc(r.admin)))
	m.HandleFunc("POST /api/device/ws-ticket",r.deviceTicket)
	return m
}
func writeJSON(w http.ResponseWriter,status int,v any){w.Header().Set("Content-Type","application/json");w.WriteHeader(status);_ = json.NewEncoder(w).Encode(v)}
func fail(w http.ResponseWriter,status int,msg string){writeJSON(w,status,map[string]string{"error":msg})}
func decode(w http.ResponseWriter,r *http.Request,v any)error{d:=json.NewDecoder(http.MaxBytesReader(w,r.Body,1<<20));d.DisallowUnknownFields();return d.Decode(v)}
func (r *Router) health(w http.ResponseWriter,_ *http.Request){writeJSON(w,200,map[string]any{"ok":true,"service":"http-api"})}
func bearer(req *http.Request)string{h:=req.Header.Get("Authorization");if !strings.HasPrefix(h,"Bearer "){return ""};return strings.TrimSpace(strings.TrimPrefix(h,"Bearer "))}
func (r *Router) auth(next http.Handler)http.Handler{return http.HandlerFunc(func(w http.ResponseWriter,q *http.Request){u,e:=r.svc.Authenticate(bearer(q));if e!=nil{fail(w,401,"unauthorized");return};next.ServeHTTP(w,contextWithUser(q,u))})}
func (r *Router) login(w http.ResponseWriter,q *http.Request){var x struct{Username string `json:"username"`;Password string `json:"password"`};if decode(w,q,&x)!=nil{fail(w,400,"invalid JSON");return};token,u,e:=r.svc.Login(x.Username,x.Password);if e!=nil{fail(w,401,"invalid username or password");return};writeJSON(w,200,map[string]string{"token":token,"userId":u.ID,"role":u.Role})}
func (r *Router) admin(w http.ResponseWriter,q *http.Request){u:=userFromContext(q);path:=strings.TrimPrefix(q.URL.Path,"/api/admin/")
	switch path {
	case "logout": if q.Method!="POST"{fail(w,405,"POST required");return};_ = r.svc.Store.DeleteSession(sqlite.Hash(bearer(q)));writeJSON(w,200,map[string]bool{"ok":true})
	case "ws-ticket": if q.Method!="POST"{fail(w,405,"POST required");return};t,e:=r.svc.CreateTicket("admin",u.ID);if e!=nil{fail(w,500,"could not issue websocket ticket");return};writeJSON(w,200,map[string]string{"ticket":t})
	case "devices": if q.Method=="GET"{r.listDevices(w,u);return};if q.Method!="POST"{fail(w,405,"GET or POST required");return};if u.Role!="admin"{fail(w,403,"admin only");return};var x struct{Name string `json:"name"`};if decode(w,q,&x)!=nil{fail(w,400,"invalid JSON");return};id,token,e:=r.svc.CreateDevice(x.Name);if e!=nil{fail(w,400,e.Error());return};writeJSON(w,201,map[string]string{"deviceId":id,"deviceToken":token})
	case "users": if q.Method!="POST"||u.Role!="admin"{fail(w,403,"admin only");return};var x struct{Username string `json:"username"`;Password string `json:"password"`};if decode(w,q,&x)!=nil{fail(w,400,"invalid JSON");return};id,e:=r.svc.CreateUser(x.Username,x.Password);if e!=nil{fail(w,400,e.Error());return};writeJSON(w,201,map[string]string{"userId":id})
	case "groups": if q.Method=="GET"{groups,e:=r.svc.Store.ListGroups();if e!=nil{fail(w,500,"database error");return};writeJSON(w,200,groups);return};if q.Method!="POST"||u.Role!="admin"{fail(w,403,"admin only");return};var x struct{Name string `json:"name"`};if decode(w,q,&x)!=nil||strings.TrimSpace(x.Name)==""{fail(w,400,"name required");return};id,e:=randomID();if e!=nil{fail(w,500,"id generation failed");return};if e=r.svc.Store.CreateGroup(id,strings.TrimSpace(x.Name));e!=nil{fail(w,409,"group already exists or could not be created");return};writeJSON(w,201,map[string]string{"groupId":id})
	case "groups/users": if q.Method!="POST"||u.Role!="admin"{fail(w,403,"admin only");return};var x struct{UserID string `json:"userId"`;GroupID string `json:"groupId"`};if decode(w,q,&x)!=nil{fail(w,400,"invalid JSON");return};if e:=r.svc.Store.AddUserGroup(x.UserID,x.GroupID);e!=nil{fail(w,400,"invalid user or group");return};writeJSON(w,200,map[string]bool{"ok":true})
	case "groups/devices": if q.Method!="POST"||u.Role!="admin"{fail(w,403,"admin only");return};var x struct{DeviceID string `json:"deviceId"`;GroupID string `json:"groupId"`};if decode(w,q,&x)!=nil{fail(w,400,"invalid JSON");return};if e:=r.svc.Store.AddDeviceGroup(x.DeviceID,x.GroupID);e!=nil{fail(w,400,"invalid device or group");return};writeJSON(w,200,map[string]bool{"ok":true})
	case "group-access": if q.Method!="POST"||u.Role!="admin"{fail(w,403,"admin only");return};var x struct{UserGroupID string `json:"userGroupId"`;DeviceGroupID string `json:"deviceGroupId"`;CanView bool `json:"canView"`;CanManage bool `json:"canManage"`};if decode(w,q,&x)!=nil{fail(w,400,"invalid JSON");return};if e:=r.svc.Store.SetGroupAccess(x.UserGroupID,x.DeviceGroupID,x.CanView,x.CanManage);e!=nil{fail(w,400,"invalid group access");return};writeJSON(w,200,map[string]bool{"ok":true})
	case "devices/disconnect","devices/enable": if q.Method!="POST"{fail(w,405,"POST required");return};var x struct{DeviceID string `json:"deviceId"`};if decode(w,q,&x)!=nil||x.DeviceID==""{fail(w,400,"deviceId required");return};ok,e:=r.svc.Store.CanAccess(u.ID,x.DeviceID,true);if e!=nil||!ok{fail(w,403,"forbidden");return};kind:="disconnect";if path=="devices/enable"{kind="enable"};if e=r.svc.Store.Enqueue(kind,x.DeviceID,u.ID);e!=nil{if e==sqlite.ErrNotFound{fail(w,404,"device not found")}else{fail(w,500,"operation failed")};return};writeJSON(w,202,map[string]string{"status":"queued"})
	default:http.NotFound(w,q)
	}
}
func (r *Router) listDevices(w http.ResponseWriter,u sqlite.User){devices,e:=r.svc.Store.ListDevices();if e!=nil{fail(w,500,"database error");return};out:=make([]sqlite.Device,0,len(devices));for _,d:=range devices{ok,err:=r.svc.Store.CanAccess(u.ID,d.ID,false);if err==nil&&ok{out=append(out,d)}};writeJSON(w,200,out)}
func (r *Router) deviceTicket(w http.ResponseWriter,q *http.Request){var x struct{DeviceID string `json:"deviceId"`};if decode(w,q,&x)!=nil||x.DeviceID==""{fail(w,400,"deviceId required");return};ok,e:=r.svc.Store.DeviceAuth(x.DeviceID,sqlite.Hash(bearer(q)));if e!=nil||!ok{fail(w,401,"invalid device credentials");return};ticket,e:=r.svc.CreateTicket("device",x.DeviceID);if e!=nil{fail(w,500,"could not issue websocket ticket");return};writeJSON(w,200,map[string]string{"ticket":ticket})}
