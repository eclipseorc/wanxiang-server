package scrcpy

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
)

// 项目根目录（相对测试运行目录两级向上：scrcpy -> internal -> 仓库根）
// 用于定位 tools/adb。
func projectRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// resolveAdbPath 应能在项目内定位到 adb 二进制。
func TestResolveAdbPath(t *testing.T) {
	root := projectRoot(t)
	path, err := resolveAdbPath(root)
	if err != nil {
		t.Fatalf("resolve adb path: %v", err)
	}
	want := "adb.exe"
	if runtime.GOOS != "windows" {
		want = "adb"
	}
	if filepath.Base(path) != want {
		t.Fatalf("resolved %s, want base %s", path, want)
	}
}

// 不存在的目录应报错。
func TestResolveAdbPathMissing(t *testing.T) {
	if _, err := resolveAdbPath(t.TempDir()); err == nil {
		t.Fatal("expected error for missing adb")
	}
}

// 冒烟：用真实 adb 二进制跑一次 version，确认封装能执行。
func TestADBVersion(t *testing.T) {
	adb, err := NewADB(projectRoot(t))
	if err != nil {
		t.Skipf("adb not available: %v", err)
	}
	version, err := adb.Version(context.Background())
	if err != nil {
		t.Fatalf("adb version: %v", err)
	}
	if version == "" {
		t.Fatal("empty adb version")
	}
}

// parseMdnsServices 应只提取 _adb-tls-connect._tcp 服务的 ip:port。
func TestParseMdnsServices(t *testing.T) {
	out := "List of discovered mdns services\n" +
		"adb-ZY22L4X5X7-qurAZn\t_adb-tls-connect._tcp\t192.168.41.177:35583\n" +
		"adb-ABC123-def\t_adb._tcp\t192.168.41.178:5555\n" // 传统 _adb._tcp，应被忽略
	got := parseMdnsServices(out)
	want := []string{"192.168.41.177:35583"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("parseMdnsServices = %v, want %v", got, want)
	}
}

// 空输出或仅表头应返回空列表。
func TestParseMdnsServicesEmpty(t *testing.T) {
	if got := parseMdnsServices("List of discovered mdns services\n"); len(got) != 0 {
		t.Fatalf("parseMdnsServices = %v, want empty", got)
	}
}

// parseUsbSerials 应只提取 USB 有线设备（无冒号 serial），忽略无线设备（ip:port），
// 并包含 device 与 unauthorized 状态，忽略 offline。
func TestParseUsbSerials(t *testing.T) {
	out := "List of devices attached\n" +
		"8DBY0X2K\tdevice\n" +           // USB device，应提取
		"LGM-V500N\tunauthorized\n" +    // 刚插线未授权，应提取
		"192.168.41.177:35583\tdevice\n" + // 无线设备，应忽略
		"offline-serial\toffline\n"       // offline，应忽略
	got := parseUsbSerials(out)
	want := []string{"8DBY0X2K", "LGM-V500N"}
	if len(got) != len(want) {
		t.Fatalf("parseUsbSerials = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parseUsbSerials = %v, want %v", got, want)
		}
	}
}
