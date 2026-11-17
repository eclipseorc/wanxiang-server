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
