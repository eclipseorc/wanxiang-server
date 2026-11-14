package websocket

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
	"github.com/gorilla/websocket"
)

type client struct { conn *websocket.Conn; mu sync.Mutex; kind,id string }
func(c *client) send(v any)error{c.mu.Lock();defer c.mu.Unlock();_ = c.conn.SetWriteDeadline(time.Now().Add(10*time.Second));return c.conn.WriteJSON(v)}
type Hub struct { store *sqlite.Store; mu sync.RWMutex; devices map[string]*client; admins map[*client]sqlite.User; watchMu sync.Mutex; viewer *client; sessionID,deviceID string }
func NewHub(s *sqlite.Store)*Hub{return &Hub{store:s,devices:map[string]*client{},admins:map[*client]sqlite.User{}}}
var upgrader=websocket.Upgrader{ReadBufferSize:4096,WriteBufferSize:4096,CheckOrigin:func(r *http.Request)bool{return r.Header.Get("Origin")==""}}
type Handler struct{ hub *Hub }
func NewHandler(h *Hub)*Handler{return &Handler{hub:h}}
func(h *Handler) ServeMux()http.Handler{m:=http.NewServeMux();m.HandleFunc("/ws/device",h.device);m.HandleFunc("/ws/admin",h.admin);return m}
func(h *Handler) upgrade(w http.ResponseWriter,r *http.Request,typ string)(*client,error){ticket:=r.URL.Query().Get("ticket");if ticket==""{http.Error(w,"ticket required",401);return nil,sqlite.ErrNotFound};t,e:=h.hub.store.ConsumeTicket(sqlite.Hash(ticket));if e!=nil||t.Type!=typ{http.Error(w,"invalid or expired ticket",401);return nil,sqlite.ErrNotFound};c,e:=upgrader.Upgrade(w,r,nil);if e!=nil{return nil,e};return &client{conn:c,kind:typ,id:t.SubjectID},nil}
func(h *Handler) device(w http.ResponseWriter,r *http.Request){c,e:=h.upgrade(w,r,"device");if e!=nil{return};enabled,e:=h.hub.store.DeviceEnabled(c.id);if e!=nil||!enabled{c.send(map[string]string{"type":"error","message":"device disabled"});c.conn.Close();return};h.hub.mu.Lock();old:=h.hub.devices[c.id];h.hub.devices[c.id]=c;h.hub.mu.Unlock();if old!=nil{_ = old.send(map[string]string{"type":"replaced"});_ = old.conn.Close()};_ = h.hub.store.SetOnline(c.id,true);h.hub.broadcastDevice(c.id,"device-online",nil);defer h.hub.removeDevice(c);for{var m map[string]any;if c.conn.ReadJSON(&m)!=nil{return};h.handleDevice(c,m)}}
func(h *Handler) removeDevice(c *client){h.hub.mu.Lock();if h.hub.devices[c.id]==c{delete(h.hub.devices,c.id);h.hub.mu.Unlock();_ = h.hub.store.SetOnline(c.id,false);h.hub.broadcastDevice(c.id,"device-offline",nil);h.hub.endWatchForDevice(c.id)}else{h.hub.mu.Unlock()};_ = c.conn.Close()}
func(h *Handler) handleDevice(c *client,m map[string]any){switch m["type"]{case "heartbeat":battery:=intValue(m["battery"]);rtt:=int64(intValue(m["rttMs"]));status,_:=m["status"].(string);_ = h.hub.store.UpdateDeviceState(c.id,true,battery,rtt,status);_ = c.send(map[string]any{"type":"heartbeat-ack","sentAt":m["sentAt"]});h.hub.broadcastDevice(c.id,"device-state",map[string]any{"battery":battery,"rttMs":rtt,"status":status});case "status":h.hub.broadcastDevice(c.id,"device-state",m["data"]);case "watch-ready","watch-denied","offer","answer","candidate","watch-stopped":h.hub.watchMu.Lock();v:=h.hub.viewer;sid:=h.hub.sessionID;did:=h.hub.deviceID;h.hub.watchMu.Unlock();if v!=nil&&did==c.id{m["sessionId"]=sid;_ = v.send(m);if m["type"]=="watch-denied"||m["type"]=="watch-stopped"{h.hub.endWatch(sid)}}}}
func(h *Handler) admin(w http.ResponseWriter,r *http.Request){c,e:=h.upgrade(w,r,"admin");if e!=nil{return};u,e:=h.hub.store.UserByID(c.id);if e!=nil{c.conn.Close();return};h.hub.mu.Lock();h.hub.admins[c]=u;h.hub.mu.Unlock();defer h.hub.removeAdmin(c);h.sendSnapshot(c,u);for{var m map[string]any;if c.conn.ReadJSON(&m)!=nil{return};h.handleAdmin(c,u,m)}}
func(h *Handler) removeAdmin(c *client){h.hub.mu.Lock();delete(h.hub.admins,c);h.hub.mu.Unlock();h.hub.watchMu.Lock();sid:=h.hub.sessionID;v:=h.hub.viewer;h.hub.watchMu.Unlock();if v==c&&sid!=""{h.hub.endWatch(sid)};_ = c.conn.Close()}
func(h *Handler) sendSnapshot(c *client,u sqlite.User){ds,e:=h.hub.store.ListDevices();if e!=nil{return};out:=[]sqlite.Device{};for _,d:=range ds{ok,_:=h.hub.store.CanAccess(u.ID,d.ID,false);if ok{d.Status="";out=append(out,d)}};_ = c.send(map[string]any{"type":"device-list","devices":out})}
func(h *Handler) handleAdmin(c *client,u sqlite.User,m map[string]any){switch m["type"]{case "ping":_ = c.send(map[string]any{"type":"pong","sentAt":m["sentAt"]});case "watch":id,_:=m["deviceId"].(string);ok,e:=h.hub.store.CanAccess(u.ID,id,false);if e!=nil||!ok{_ = c.send(map[string]string{"type":"error","message":"not authorized"});return};h.hub.mu.RLock();d:=h.hub.devices[id];h.hub.mu.RUnlock();if d==nil{_ = c.send(map[string]string{"type":"error","message":"device offline"});return};sid:=newSessionID();if e=h.hub.store.StartWatch(sid,id,u.ID);e!=nil{_ = c.send(map[string]string{"type":"error","message":e.Error()});return};h.hub.watchMu.Lock();if h.hub.viewer!=nil{h.hub.watchMu.Unlock();_ = h.hub.store.EndWatch(sid);_ = c.send(map[string]string{"type":"error","message":"watch already active"});return};h.hub.viewer=c;h.hub.sessionID=sid;h.hub.deviceID=id;h.hub.watchMu.Unlock();_ = d.send(map[string]any{"type":"watch-request","sessionId":sid,"requestedBy":u.ID});_ = c.send(map[string]string{"type":"watch-started","sessionId":sid,"deviceId":id});case "watch-stop":h.hub.watchMu.Lock();sid:=h.hub.sessionID;h.hub.watchMu.Unlock();h.hub.endWatch(sid);case "offer","answer","candidate":h.hub.watchMu.Lock();did:=h.hub.deviceID;sid:=h.hub.sessionID;viewer:=h.hub.viewer;h.hub.watchMu.Unlock();if viewer!=c||sid==""{return};h.hub.mu.RLock();d:=h.hub.devices[did];h.hub.mu.RUnlock();if d!=nil{m["sessionId"]=sid;_ = d.send(m)}}}
func(h *Hub) endWatch(sid string){if sid==""{return};h.watchMu.Lock();if h.sessionID!=sid{h.watchMu.Unlock();return};v,did:=h.viewer,h.deviceID;h.viewer=nil;h.deviceID="";h.sessionID="";h.watchMu.Unlock();_ = h.store.EndWatch(sid);if v!=nil{_ = v.send(map[string]string{"type":"watch-ended","sessionId":sid})};h.mu.RLock();d:=h.devices[did];h.mu.RUnlock();if d!=nil{_ = d.send(map[string]string{"type":"watch-stop","sessionId":sid})}}
func(h *Hub) endWatchForDevice(id string){h.watchMu.Lock();sid:=h.sessionID;did:=h.deviceID;h.watchMu.Unlock();if id==did{h.endWatch(sid)}}
func(h *Hub) broadcastDevice(id,typ string,data any){event:=map[string]any{"type":typ,"deviceId":id,"data":data};h.mu.RLock();targets:=make([]struct{c *client;u sqlite.User},0,len(h.admins));for c,u:=range h.admins{targets=append(targets,struct{c *client;u sqlite.User}{c,u})};h.mu.RUnlock();for _,x:=range targets{ok,_:=h.store.CanAccess(x.u.ID,id,false);if ok{_ = x.c.send(event)}}}
func(h *Hub) RunControlPoller(stop <-chan struct{}){ticker:=time.NewTicker(500*time.Millisecond);defer ticker.Stop();for{select{case<-stop:return;case<-ticker.C:events,e:=h.store.PendingEvents();if e!=nil{log.Printf("control poll: %v",e);continue};for _,ev:=range events{h.applyEvent(ev);_ = h.store.MarkEvent(ev.ID)}}}}
func(h *Hub) applyEvent(e sqlite.ControlEvent){h.mu.RLock();d:=h.devices[e.DeviceID];h.mu.RUnlock();if d!=nil&&e.Kind=="disconnect"{_ = d.send(map[string]string{"type":"disconnect","reason":"admin"});_ = d.conn.Close()};if e.Kind=="disconnect"{h.endWatchForDevice(e.DeviceID)}}
func intValue(v any)int{switch x:=v.(type){case float64:return int(x);case int:return x;case int64:return int(x)};return 0}
func newSessionID()string{return time.Now().UTC().Format("20060102T150405.000000000")}
var _ = json.Valid
