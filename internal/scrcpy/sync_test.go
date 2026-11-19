// sync_test.go 测试群控组的坐标归一化广播与生命周期。
package scrcpy

import (
	"encoding/binary"
	"net"
	"reflect"
	"testing"
)

// newTestServer 构造一个空 Server，供群控逻辑测试（不依赖 adb）。
func newTestServer() *Server {
	return NewServer(nil, ScrcpyConfig{})
}

// fakeControl 用 net.Pipe 模拟控制通道，返回写端和读端。
func fakeControl(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})
	// net.Pipe 写会阻塞直到对端读，测试里用 goroutine 消费。
	return server, client
}

// drain 在后台消费 pipe 的写入，避免写端阻塞，并把数据回传给通道。
func drain(t *testing.T, c net.Conn) <-chan []byte {
	t.Helper()
	out := make(chan []byte, 64)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				cp := make([]byte, n)
				copy(cp, buf[:n])
				out <- cp
			}
			if err != nil {
				close(out)
				return
			}
		}
	}()
	return out
}

// touchX 从 32 字节 touch 帧里解析出 X 坐标。
func touchX(b []byte) uint32 { return binary.BigEndian.Uint32(b[10:14]) }

// touchY 从 32 字节 touch 帧里解析出 Y 坐标。
func touchY(b []byte) uint32 { return binary.BigEndian.Uint32(b[14:18]) }

// 主控分辨率 1080x2400 下的点击 (540,1200)，在不同分辨率从机上应换算到
// 相同相对位置（各自屏幕中心）。
func TestSyncBroadcastTouchNormalizesCoordinates(t *testing.T) {
	s := newTestServer()

	// 主控：1080x2400，中心 (540,1200)
	masterCtl, masterRd := fakeControl(t)
	masterCh := drain(t, masterRd)
	s.registerSession("master", masterCtl, 1080, 2400)

	// 从机：720x1600，中心应为 (360,800)
	slaveCtl, slaveRd := fakeControl(t)
	slaveCh := drain(t, slaveRd)
	s.registerSession("slave", slaveCtl, 720, 1600)

	// 建组，master 为主控。
	s.mu.Lock()
	g := &syncGroup{id: "g1", master: "master", members: map[string]*deviceSession{
		"master": s.sessions["master"],
		"slave":  s.sessions["slave"],
	}}
	s.groups["g1"] = g
	s.inGroup["master"] = "g1"
	s.inGroup["slave"] = "g1"
	s.mu.Unlock()

	// 主控广播一次点击 (540,1200) —— 即各自屏幕中心。
	s.broadcastControl("master", []byte(`{"type":"touch","action":"down","x":540,"y":1200}`))

	// 主控帧应保持 (540,1200)。
	masterFrame := <-masterCh
	if gotX, gotY := touchX(masterFrame), touchY(masterFrame); gotX != 540 || gotY != 1200 {
		t.Fatalf("master coords = (%d,%d), want (540,1200)", gotX, gotY)
	}
	// 从机帧应换算为 (360,800)。
	slaveFrame := <-slaveCh
	if gotX, gotY := touchX(slaveFrame), touchY(slaveFrame); gotX != 360 || gotY != 800 {
		t.Fatalf("slave coords = (%d,%d), want (360,800)", gotX, gotY)
	}
}

// 边界点击（屏幕最右下角）在从机上应钳制到合法范围内，不越界。
func TestSyncBroadcastTouchClampsToBounds(t *testing.T) {
	s := newTestServer()

	masterCtl, masterRd := fakeControl(t)
	drain(t, masterRd)
	s.registerSession("master", masterCtl, 1000, 2000)

	slaveCtl, slaveRd := fakeControl(t)
	slaveCh := drain(t, slaveRd)
	s.registerSession("slave", slaveCtl, 500, 500)

	s.mu.Lock()
	s.groups["g1"] = &syncGroup{id: "g1", master: "master", members: map[string]*deviceSession{
		"master": s.sessions["master"],
		"slave":  s.sessions["slave"],
	}}
	s.inGroup["master"] = "g1"
	s.inGroup["slave"] = "g1"
	s.mu.Unlock()

	// 主控点击 (999,1999)，从机换算后不能超过 499。
	s.broadcastControl("master", []byte(`{"type":"touch","action":"down","x":999,"y":1999}`))
	slaveFrame := <-slaveCh
	if gotX, gotY := touchX(slaveFrame), touchY(slaveFrame); gotX >= 500 || gotY >= 500 {
		t.Fatalf("slave coords (%d,%d) out of bounds", gotX, gotY)
	}
}

// 无坐标事件（key）应原样广播给所有成员，且是完整的 keycode 帧。
func TestSyncBroadcastKeyEvent(t *testing.T) {
	s := newTestServer()

	ctl1, rd1 := fakeControl(t)
	ch1 := drain(t, rd1)
	s.registerSession("a", ctl1, 1080, 2400)

	ctl2, rd2 := fakeControl(t)
	ch2 := drain(t, rd2)
	s.registerSession("b", ctl2, 1080, 2400)

	s.mu.Lock()
	s.groups["g1"] = &syncGroup{id: "g1", master: "a", members: map[string]*deviceSession{
		"a": s.sessions["a"],
		"b": s.sessions["b"],
	}}
	s.inGroup["a"] = "g1"
	s.inGroup["b"] = "g1"
	s.mu.Unlock()

	s.broadcastControl("a", []byte(`{"type":"key","action":"down","key":4}`))

	for name, ch := range map[string]<-chan []byte{"a": ch1, "b": ch2} {
		frame := <-ch
		if frame[0] != msgInjectKeycode {
			t.Fatalf("device %s frame type = %d, want keycode", name, frame[0])
		}
	}
}

// sync-create 应把成员登记进组；一台设备不能同时在两个组。
func TestSyncCreateGroupLifecycle(t *testing.T) {
	s := newTestServer()

	ctlA, rdA := fakeControl(t)
	drain(t, rdA)
	s.registerSession("a", ctlA, 1080, 2400)
	ctlB, rdB := fakeControl(t)
	drain(t, rdB)
	s.registerSession("b", ctlB, 720, 1600)

	// 建组 g1：a 为主控。
	if resp := s.syncCreate("g1", []string{"a", "b"}, "a"); resp["type"] != "sync-created" {
		t.Fatalf("syncCreate g1 resp = %v, want sync-created", resp)
	}
	if s.groups["g1"] == nil {
		t.Fatal("group g1 not created")
	}
	if s.groups["g1"].master != "a" {
		t.Fatalf("master = %q, want a", s.groups["g1"].master)
	}
	if s.inGroup["b"] != "g1" {
		t.Fatalf("inGroup[b] = %q, want g1", s.inGroup["b"])
	}

	// 一台设备不能同时在两个组：a 已在 g1，再建含 a 的 g3 会自动解散旧组 g1
	// （清理僵尸组），然后成功建 g3，避免「一键镜像」因残留组永久失败。
	if resp := s.syncCreate("g3", []string{"a"}, "a"); resp["type"] != "sync-created" {
		t.Fatalf("syncCreate g3 should auto-dissolve stale g1 and succeed, got %v", resp)
	}
	if s.groups["g1"] != nil {
		t.Fatal("g1 should be auto-dissolved")
	}
	if _, ok := s.inGroup["b"]; ok {
		t.Fatal("b should be removed from inGroup after g1 auto-dissolve")
	}

	// 解散 g3 后，a 应从 inGroup 移除。
	if resp := s.syncDestroy("g3"); resp["type"] != "sync-destroyed" {
		t.Fatalf("syncDestroy g3 resp = %v", resp)
	}
	if s.groups["g3"] != nil {
		t.Fatal("g3 should be destroyed")
	}
	if _, ok := s.inGroup["a"]; ok {
		t.Fatal("a should be removed from inGroup after destroy")
	}

	// 缺省主控：解散后重建，不指定 master，应取第一个成员 a。
	if resp := s.syncCreate("g2", []string{"a", "b"}, ""); resp["type"] != "sync-created" {
		t.Fatalf("syncCreate g2 resp = %v, want sync-created", resp)
	}
	if s.groups["g2"] == nil || s.groups["g2"].master != "a" {
		t.Fatalf("g2 master should default to first member a")
	}
}

// unregisterSession 应把设备从群控组摘除，组变空时解散。
func TestUnregisterSessionRemovesFromGroup(t *testing.T) {
	s := newTestServer()

	ctlA, rdA := fakeControl(t)
	drain(t, rdA)
	s.registerSession("a", ctlA, 1080, 2400)

	s.mu.Lock()
	s.groups["g1"] = &syncGroup{id: "g1", master: "a", members: map[string]*deviceSession{
		"a": s.sessions["a"],
	}}
	s.inGroup["a"] = "g1"
	s.mu.Unlock()

	s.unregisterSession("a")

	if len(s.sessions) != 0 {
		t.Fatal("session should be removed")
	}
	if _, ok := s.groups["g1"]; ok {
		t.Fatal("empty group should be destroyed")
	}
	if _, ok := s.inGroup["a"]; ok {
		t.Fatal("a should be removed from inGroup")
	}
}

// parseOnlineSerials 应只提取 device 状态的 serial，忽略 offline/unauthorized/表头。
func TestParseOnlineSerials(t *testing.T) {
	out := "List of devices attached\n" +
		"192.168.1.100:35583\tdevice\n" +
		"192.168.1.101:40000\toffline\n" +
		"192.168.1.102:40001\tdevice\n" +
		"emulator-5554\tdevice\n" +
		"192.168.1.103:40002\tunauthorized\n"

	got := parseOnlineSerials(out)
	want := []string{
		"192.168.1.100:35583",
		"192.168.1.102:40001",
		"emulator-5554",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseOnlineSerials = %v, want %v", got, want)
	}
}

// 空输出（无设备）应返回空列表，不 panic。
func TestParseOnlineSerialsEmpty(t *testing.T) {
	if got := parseOnlineSerials("List of devices attached\n\n"); len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

// missingSerials 应返回不在在线集合里的 serial，保持顺序。
func TestMissingSerials(t *testing.T) {
	online := []string{"a:1", "b:2", "c:3"}
	serials := []string{"a:1", "x:9", "c:3", "y:8"}
	got := missingSerials(serials, online)
	want := []string{"x:9", "y:8"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missingSerials = %v, want %v", got, want)
	}
}

// 全部在线时应返回空。
func TestMissingSerialsAllOnline(t *testing.T) {
	online := []string{"a:1", "b:2"}
	if got := missingSerials([]string{"a:1", "b:2"}, online); len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}
