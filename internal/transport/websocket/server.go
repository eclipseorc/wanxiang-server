package websocket

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/eclipseorc/wanxiang-server/internal/storage/sqlite"
	"github.com/gorilla/websocket"
)

const (
	writeWait = 10 * time.Second
	pongWait  = 70 * time.Second
	pingEvery = 55 * time.Second
)

type client struct {
	conn *websocket.Conn
	mu   sync.Mutex
	kind string
	id   string
	done chan struct{}
	once sync.Once
}

func newClient(conn *websocket.Conn, kind, id string) *client {
	return &client{conn: conn, kind: kind, id: id, done: make(chan struct{})}
}
func (c *client) send(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return c.conn.WriteJSON(v)
}
func (c *client) close() {
	c.once.Do(func() { close(c.done); _ = c.conn.Close() })
}

type Hub struct {
	store *sqlite.Store
	mu sync.RWMutex
	devices map[string]*client
	admins map[*client]sqlite.User
	watchMu sync.Mutex
	viewer *client
	sessionID string
	deviceID string
}

func NewHub(store *sqlite.Store) *Hub {
	return &Hub{store: store, devices: make(map[string]*client), admins: make(map[*client]sqlite.User)}
}

type Handler struct{ hub *Hub }
func NewHandler(h *Hub) *Handler { return &Handler{hub: h} }
func (h *Handler) ServeMux() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/ws/device", h.device)
	m.HandleFunc("/ws/admin", h.admin)
	return m
}

var upgrader = websocket.Upgrader{
	ReadBufferSize: 4096,
	WriteBufferSize: 4096,
	CheckOrigin: originAllowed,
}

func originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" { return true }
	for _, allowed := range strings.Split(os.Getenv("WS_ALLOWED_ORIGINS"), ",") {
		if strings.TrimSpace(allowed) != "" && strings.TrimSpace(allowed) == origin { return true }
	}
	return false
}

func (h *Handler) upgrade(w http.ResponseWriter, r *http.Request, kind string) (*client, error) {
	raw := r.URL.Query().Get("ticket")
	if raw == "" { http.Error(w, "ticket required", http.StatusUnauthorized); return nil, sqlite.ErrNotFound }
	ticket, err := h.hub.store.ConsumeTicket(sqlite.Hash(raw))
	if err != nil || ticket.Type != kind { http.Error(w, "invalid or expired ticket", http.StatusUnauthorized); return nil, sqlite.ErrNotFound }
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil { return nil, err }
	conn.SetReadLimit(64 << 10)
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(pongWait)) })
	return newClient(conn, kind, ticket.SubjectID), nil
}

func (h *Handler) device(w http.ResponseWriter, r *http.Request) {
	c, err := h.upgrade(w, r, "device"); if err != nil { return }
	enabled, err := h.hub.store.DeviceEnabled(c.id)
	if err != nil || !enabled { _ = c.send(map[string]string{"type":"error", "message":"device disabled or unknown"}); c.close(); return }
	h.hub.mu.Lock(); old := h.hub.devices[c.id]; h.hub.devices[c.id] = c; h.hub.mu.Unlock()
	if old != nil { _ = old.send(map[string]string{"type":"replaced"}); old.close() }
	_ = h.hub.store.SetOnline(c.id, true)
	_ = c.send(map[string]any{"type":"joined", "deviceId":c.id})
	h.hub.broadcastDevice(c.id, "device-online", nil)
	go h.pingLoop(c)
	defer h.removeDevice(c)
	for { var msg map[string]any; if c.conn.ReadJSON(&msg) != nil { return }; h.handleDevice(c, msg) }
}

func (h *Handler) admin(w http.ResponseWriter, r *http.Request) {
	c, err := h.upgrade(w, r, "admin"); if err != nil { return }
	u, err := h.hub.store.UserByID(c.id); if err != nil { c.close(); return }
	h.hub.mu.Lock(); h.hub.admins[c] = u; h.hub.mu.Unlock()
	defer h.removeAdmin(c)
	h.sendSnapshot(c, u)
	go h.pingLoop(c)
	for { var msg map[string]any; if c.conn.ReadJSON(&msg) != nil { return }; h.handleAdmin(c, u, msg) }
}

func (h *Handler) pingLoop(c *client) {
	t := time.NewTicker(pingEvery); defer t.Stop()
	for { select { case <-c.done: return; case <-t.C:
		c.mu.Lock(); _ = c.conn.SetWriteDeadline(time.Now().Add(writeWait)); err := c.conn.WriteMessage(websocket.PingMessage, nil); c.mu.Unlock()
		if err != nil { c.close(); return }
	} }
}

func (h *Handler) removeDevice(c *client) {
	h.hub.mu.Lock(); current := h.hub.devices[c.id] == c; if current { delete(h.hub.devices, c.id) }; h.hub.mu.Unlock()
	if current { _ = h.hub.store.SetOnline(c.id, false); h.hub.broadcastDevice(c.id, "device-offline", nil); h.hub.endWatchForDevice(c.id) }
	c.close()
}
func (h *Handler) removeAdmin(c *client) {
	h.hub.mu.Lock(); delete(h.hub.admins, c); h.hub.mu.Unlock()
	h.hub.watchMu.Lock(); sid, viewer := h.hub.sessionID, h.hub.viewer; h.hub.watchMu.Unlock()
	if viewer == c && sid != "" { h.hub.endWatch(sid) }
	c.close()
}

func (h *Handler) handleDevice(c *client, msg map[string]any) {
	switch msg["type"] {
	case "join": _ = c.send(map[string]string{"type":"joined", "deviceId":c.id})
	case "out": _ = c.send(map[string]string{"type":"out-ack"}); c.close()
	case "heartbeat":
		battery := intValue(msg["battery"]); if battery < 0 || battery > 100 { battery = 0 }
		rtt := int64(intValue(msg["rttMs"])); status, _ := msg["status"].(string)
		_ = h.hub.store.UpdateDeviceState(c.id, battery, rtt, status)
		_ = c.send(map[string]any{"type":"heartbeat-ack", "sentAt":msg["sentAt"]})
		h.hub.broadcastDevice(c.id, "device-state", map[string]any{"battery":battery,"rttMs":rtt,"status":status,"lastSeen":time.Now().Unix()})
	case "status": h.hub.broadcastDevice(c.id, "device-state", msg["data"])
	case "command-result": h.hub.broadcastDevice(c.id, "command-result", msg)
	case "watch-ready", "watch-denied", "watch-stopped", "offer", "answer", "candidate":
		h.hub.watchMu.Lock(); viewer, sid, did := h.hub.viewer, h.hub.sessionID, h.hub.deviceID; h.hub.watchMu.Unlock()
		if viewer == nil || did != c.id || sid == "" || msg["sessionId"] != sid { return }
		_ = viewer.send(msg)
		if msg["type"] == "watch-denied" || msg["type"] == "watch-stopped" { h.hub.endWatch(sid) }
	}
}

func (h *Handler) sendSnapshot(c *client, u sqlite.User) {
	devices, err := h.hub.store.ListDevices(); if err != nil { return }
	visible := make([]sqlite.Device, 0)
	for _, d := range devices { allowed, _ := h.hub.store.CanAccess(u.ID, d.ID, false); if allowed { visible = append(visible, d) } }
	_ = c.send(map[string]any{"type":"device-list", "devices":visible})
}

func (h *Handler) handleAdmin(c *client, u sqlite.User, msg map[string]any) {
	switch msg["type"] {
	case "ping": _ = c.send(map[string]any{"type":"pong", "sentAt":msg["sentAt"]})
	case "watch":
		deviceID, _ := msg["deviceId"].(string)
		allowed, err := h.hub.store.CanAccess(u.ID, deviceID, false); if err != nil || !allowed { _ = c.send(map[string]string{"type":"error","message":"not authorized"}); return }
		h.hub.mu.RLock(); device := h.hub.devices[deviceID]; h.hub.mu.RUnlock(); if device == nil { _ = c.send(map[string]string{"type":"error","message":"device offline"}); return }
		sid := newSessionID(); if err = h.hub.store.StartWatch(sid, deviceID, u.ID); err != nil { _ = c.send(map[string]string{"type":"error","message":err.Error()}); return }
		h.hub.watchMu.Lock(); if h.hub.viewer != nil { h.hub.watchMu.Unlock(); _ = h.hub.store.EndWatch(sid); _ = c.send(map[string]string{"type":"error","message":"watch already active"}); return }
		h.hub.viewer, h.hub.sessionID, h.hub.deviceID = c, sid, deviceID; h.hub.watchMu.Unlock()
		_ = h.hub.store.Audit(u.ID, "watch-request", deviceID, sid)
		_ = device.send(map[string]any{"type":"watch-request","sessionId":sid,"requestedBy":u.ID})
		_ = c.send(map[string]string{"type":"watch-started","sessionId":sid,"deviceId":deviceID})
	case "watch-stop":
		h.hub.watchMu.Lock(); sid, viewer := h.hub.sessionID, h.hub.viewer; h.hub.watchMu.Unlock(); if viewer == c { h.hub.endWatch(sid) }
	case "offer", "answer", "candidate":
		h.hub.watchMu.Lock(); deviceID, sid, viewer := h.hub.deviceID, h.hub.sessionID, h.hub.viewer; h.hub.watchMu.Unlock()
		if viewer != c || sid == "" || msg["sessionId"] != sid { return }
		h.hub.mu.RLock(); device := h.hub.devices[deviceID]; h.hub.mu.RUnlock(); if device != nil { _ = device.send(msg) }
	}
}

func (h *Hub) endWatch(sid string) {
	if sid == "" { return }
	h.watchMu.Lock(); if h.sessionID != sid { h.watchMu.Unlock(); return }
	viewer, deviceID := h.viewer, h.deviceID; h.viewer, h.deviceID, h.sessionID = nil, "", ""; h.watchMu.Unlock()
	_ = h.store.EndWatch(sid)
	if viewer != nil { _ = viewer.send(map[string]string{"type":"watch-ended","sessionId":sid}) }
	h.mu.RLock(); device := h.devices[deviceID]; h.mu.RUnlock(); if device != nil { _ = device.send(map[string]string{"type":"watch-stop","sessionId":sid}) }
	_ = h.store.Audit("system", "watch-ended", deviceID, sid)
}
func (h *Hub) endWatchForDevice(id string) { h.watchMu.Lock(); sid, did := h.sessionID, h.deviceID; h.watchMu.Unlock(); if id == did { h.endWatch(sid) } }
func (h *Hub) broadcastDevice(id, typ string, data any) {
	event := map[string]any{"type":typ,"deviceId":id,"data":data}
	h.hubAdminSnapshot(func(c *client,u sqlite.User){ allowed,_:=h.store.CanAccess(u.ID,id,false); if allowed { _ = c.send(event) } })
}
func (h *Hub) hubAdminSnapshot(fn func(*client,sqlite.User)) { h.mu.RLock(); list:=make([]struct{c *client;u sqlite.User},0,len(h.admins));for c,u:=range h.admins{list=append(list,struct{c *client;u sqlite.User}{c,u})};h.mu.RUnlock();for _,x:=range list{fn(x.c,x.u)} }
func (h *Hub) RunControlPoller(stop <-chan struct{}) {
	t:=time.NewTicker(400*time.Millisecond);defer t.Stop()
	for { select { case <-stop:return; case <-t.C:
		events,err:=h.store.PendingEvents();if err!=nil{log.Printf("control event poll failed: %v",err);continue}
		for _,event:=range events{h.applyEvent(event);_ = h.store.MarkEvent(event.ID)}
	} }
}
func (h *Hub) applyEvent(e sqlite.ControlEvent) {
	h.mu.RLock();device:=h.devices[e.DeviceID];h.mu.RUnlock()
	if e.Kind=="disconnect" { if device!=nil{_ = device.send(map[string]string{"type":"disconnect","reason":"admin"});device.close()};h.endWatchForDevice(e.DeviceID) }
	h.broadcastDevice(e.DeviceID,"device-admin-state",map[string]string{"state":e.Kind})
}
func intValue(v any) int { switch n:=v.(type){case float64:return int(n);case int:return n;case int64:return int(n)};return 0 }
func newSessionID() string { b:=make([]byte,16);if _,err:=rand.Read(b);err!=nil{return time.Now().UTC().Format("20060102T150405.000000000")};return hex.EncodeToString(b) }
