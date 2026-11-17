package scrcpy

import (
	"testing"
)

// 同一 serial 同一时刻只能有一个观看会话。
func TestTryAcquireSinglePerSerial(t *testing.T) {
	s := NewServer(nil, ScrcpyConfig{})
	if !s.tryAcquire("192.168.1.10:35583") {
		t.Fatal("first acquire should succeed")
	}
	if s.tryAcquire("192.168.1.10:35583") {
		t.Fatal("second acquire for same serial should fail")
	}
	s.release("192.168.1.10:35583")
	if !s.tryAcquire("192.168.1.10:35583") {
		t.Fatal("acquire after release should succeed")
	}
}

// 不同 serial 互不影响。
func TestTryAcquireDifferentSerials(t *testing.T) {
	s := NewServer(nil, ScrcpyConfig{})
	if !s.tryAcquire("a:1") {
		t.Fatal("acquire a:1 should succeed")
	}
	if !s.tryAcquire("b:2") {
		t.Fatal("acquire b:2 should succeed")
	}
}

// scrcpy 启动命令应包含关键参数。
func TestBuildScrcpyCommand(t *testing.T) {
	cfg := ScrcpyConfig{
		ServerRemotePath: "/data/local/tmp/scrcpy-server",
		MaxSize:          1024, BitRate: 2000000, MaxFPS: 30,
	}
	cmd := cfg.buildScrcpyCommand()
	for _, want := range []string{
		"CLASSPATH=/data/local/tmp/scrcpy-server",
		"com.genymobile.scrcpy.Server",
		"tunnel_forward=true",
		"max_size=1024",
		"video_bit_rate=2000000",
		"max_fps=30",
		"send_frame_meta=true",
	} {
		if !contains(cmd, want) {
			t.Fatalf("command missing %q: %s", want, cmd)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
