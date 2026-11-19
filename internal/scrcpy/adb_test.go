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

// parseLsOutput 应正确解析目录与文件，跳过 . / .. / total，并识别图片扩展名。
func TestParseLsOutput(t *testing.T) {
	out := "total 16\n" +
		"drwxr-xr-x 2 root root  4096 2024-01-01 12:00 Camera\n" +
		"-rw-r--r-- 1 root root  2413 2024-01-01 12:00 IMG_001.jpg\n" +
		"-rw-r--r-- 1 root root 12000 2024-01-01 12:00 report.txt\n" +
		"drwxr-xr-x 2 root root  4096 2024-01-01 12:00 .\n" +
		"drwxr-xr-x 2 root root  4096 2024-01-01 12:00 ..\n" +
		"-rw-r--r-- 1 root root   512 2024-01-01 12:00 my note with space.png\n"
	got := parseLsOutput(out)
	if len(got) != 4 {
		t.Fatalf("parseLsOutput len = %d, want 4", len(got))
	}
	if got[0].Name != "Camera" || !got[0].IsDir {
		t.Fatalf("got[0] = %+v, want dir Camera", got[0])
	}
	if got[1].Name != "IMG_001.jpg" || got[1].IsDir || got[1].Size != 2413 || !got[1].IsImage {
		t.Fatalf("got[1] = %+v, want file IMG_001.jpg size 2413 image", got[1])
	}
	if got[2].Name != "report.txt" || got[2].IsImage {
		t.Fatalf("got[2] = %+v, want non-image report.txt", got[2])
	}
	// 含空格的文件名应完整保留，且识别为图片。
	if got[3].Name != "my note with space.png" || !got[3].IsImage {
		t.Fatalf("got[3] = %+v, want name with space + image", got[3])
	}
}

// parseLsOutput 对空输出或仅 total 行应返回空列表。
func TestParseLsOutputEmpty(t *testing.T) {
	if got := parseLsOutput("total 0\n"); len(got) != 0 {
		t.Fatalf("parseLsOutput = %v, want empty", got)
	}
	if got := parseLsOutput(""); len(got) != 0 {
		t.Fatalf("parseLsOutput = %v, want empty", got)
	}
}

// parseUsbSerials 应只提取 USB 有线设备（无冒号 serial），忽略无线设备（ip:port），
// 并包含 device 与 unauthorized 状态，忽略 offline。
func TestParseUsbSerials(t *testing.T) {
	out := "List of devices attached\n" +
		"8DBY0X2K\tdevice\n" + // USB device，应提取
		"LGM-V500N\tunauthorized\n" + // 刚插线未授权，应提取
		"192.168.41.177:35583\tdevice\n" + // 无线设备，应忽略
		"offline-serial\toffline\n" // offline，应忽略
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
