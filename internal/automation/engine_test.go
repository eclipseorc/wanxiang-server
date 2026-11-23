package automation

import (
	"strings"
	"testing"
)

func TestParseFlow(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		wantN   int
		wantErr bool
	}{
		{"empty array", `[]`, 0, false},
		{"single step", `[{"action":"tap","params":{"x":"1","y":"2"}}]`, 1, false},
		{"multiple steps", `[{"action":"wait","params":{"ms":"100"}},{"action":"launchApp","params":{"pkg":"com.x"}}]`, 2, false},
		{"invalid json", `not-json`, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			flow, err := ParseFlow(c.json)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(flow.Steps) != c.wantN {
				t.Fatalf("got %d steps, want %d", len(flow.Steps), c.wantN)
			}
		})
	}
}

func TestSplitDevices(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a,b,c", []string{"a", "b", "c"}},
		{"a,b,a", []string{"a", "b"}},                  // 去重
		{"a，b，c", []string{"a", "b", "c"}},            // 中文逗号
		{"a, ,b", []string{"a", "b"}},                  // 空项
	}
	for _, c := range cases {
		got := splitDevices(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("splitDevices(%q) = %v, want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("splitDevices(%q) = %v, want %v", c.in, got, c.want)
			}
		}
	}
}

func TestResult(t *testing.T) {
	r := result(0, "tap", nil)
	if r.Status != "success" || r.Error != "" {
		t.Fatalf("success result wrong: %+v", r)
	}
	r = result(1, "tap", errTest)
	if r.Status != "failed" || r.Error != "boom" {
		t.Fatalf("failed result wrong: %+v", r)
	}
}

var errTest = &testErr{"boom"}

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }

func TestIntParam(t *testing.T) {
	if _, err := intParam(map[string]string{"x": "10"}, "x"); err != nil {
		t.Fatalf("intParam valid: %v", err)
	}
	if _, err := intParam(map[string]string{}, "x"); err == nil {
		t.Fatalf("intParam missing should error")
	}
	if _, err := intParam(map[string]string{"x": "abc"}, "x"); err == nil {
		t.Fatalf("intParam invalid should error")
	}
}

func TestSanitize(t *testing.T) {
	if got := sanitize("192.168.1.1:5555"); got != "192.168.1.1_5555" {
		t.Fatalf("sanitize got %q", got)
	}
	// 路径穿越输入:结果必须不含分隔符与「..」,具体替换为下划线即可。
	got := sanitize("../etc/passwd")
	if containsAny(got, "/", "\\", "..") {
		t.Fatalf("sanitize left traversal characters: %q", got)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestParseUIHierarchyAndFindBestNode(t *testing.T) {
	xmlSample := `<?xml version="1.0" encoding="UTF-8"?>
<hierarchy rotation="0">
  <node index="0" text="要允许通过此网络进行无线调试吗？" resource-id="android:id/alertTitle" class="android.widget.TextView" clickable="false" bounds="[80,500][640,580]" />
  <node index="1" text="允许" resource-id="android:id/button1" class="android.widget.Button" clickable="true" bounds="[500,820][620,900]" />
</hierarchy>`

	nodes, err := ParseUIHierarchyXML(xmlSample)
	if err != nil {
		t.Fatalf("ParseUIHierarchyXML failed: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("got %d nodes, want 2", len(nodes))
	}

	// FindBestNode 应该优先命中完全相等且 clickable 的「允许」按钮，而不是包含「允许」的标题
	best, ok := FindBestNode(nodes, "text", "允许")
	if !ok {
		t.Fatalf("expected FindBestNode to match 允许")
	}
	if best.ResourceID != "android:id/button1" || best.CenterX != 560 || best.CenterY != 860 {
		t.Fatalf("FindBestNode picked wrong node: %+v", best)
	}
}
