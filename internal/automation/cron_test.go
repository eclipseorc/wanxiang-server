package automation

import (
	"testing"
	"time"
)

// TestParseCronValid 验证合法 cron 表达式的解析与命中。
func TestParseCronValid(t *testing.T) {
	cases := []struct {
		expr   string
		at     time.Time
		match  bool
	}{
		{"0 9 * * *", time.Date(2026, 1, 1, 9, 0, 0, 0, time.Local), true},
		{"0 9 * * *", time.Date(2026, 1, 1, 9, 1, 0, 0, time.Local), false},
		{"0 9 * * *", time.Date(2026, 1, 1, 10, 0, 0, 0, time.Local), false},
		{"*/15 * * * *", time.Date(2026, 1, 1, 9, 0, 0, 0, time.Local), true},
		{"*/15 * * * *", time.Date(2026, 1, 1, 9, 15, 0, 0, time.Local), true},
		{"*/15 * * * *", time.Date(2026, 1, 1, 9, 45, 0, 0, time.Local), true},
		{"*/15 * * * *", time.Date(2026, 1, 1, 9, 10, 0, 0, time.Local), false},
		{"30 8-10 * * *", time.Date(2026, 1, 1, 8, 30, 0, 0, time.Local), true},
		{"30 8-10 * * *", time.Date(2026, 1, 1, 10, 30, 0, 0, time.Local), true},
		{"30 8-10 * * *", time.Date(2026, 1, 1, 11, 30, 0, 0, time.Local), false},
		{"0 9 * * 1", time.Date(2026, 1, 5, 9, 0, 0, 0, time.Local), true},  // 周一
		{"0 9 * * 1", time.Date(2026, 1, 6, 9, 0, 0, 0, time.Local), false}, // 周二
		{"0 9 1,15 * *", time.Date(2026, 1, 1, 9, 0, 0, 0, time.Local), true},
		{"0 9 1,15 * *", time.Date(2026, 1, 15, 9, 0, 0, 0, time.Local), true},
		{"0 9 1,15 * *", time.Date(2026, 1, 2, 9, 0, 0, 0, time.Local), false},
	}
	for _, c := range cases {
		s, err := parseCron(c.expr)
		if err != nil {
			t.Fatalf("parseCron(%q): %v", c.expr, err)
		}
		if got := s.matches(c.at); got != c.match {
			t.Errorf("cron %q at %v: got %v want %v", c.expr, c.at.Format("15:04"), got, c.match)
		}
	}
}

// TestParseCronInvalid 验证非法表达式被拒绝。
func TestParseCronInvalid(t *testing.T) {
	bad := []string{
		"", "0 9 * *", "0 9 * * * *", "abc 9 * * *", "61 9 * * *",
		"0 24 * * *", "0 9 32 * *", "0 9 * 13 *", "0 9 * * 8",
		"*/0 * * * *",
	}
	for _, expr := range bad {
		if _, err := parseCron(expr); err == nil {
			t.Errorf("parseCron(%q) should fail", expr)
		}
	}
}

// TestParseCronSunday 验证周日双表示(0 与 7)等价。
func TestParseCronSunday(t *testing.T) {
	s0, _ := parseCron("0 9 * * 0")
	s7, _ := parseCron("0 9 * * 7")
	sun := time.Date(2026, 1, 4, 9, 0, 0, 0, time.Local) // 2026-01-04 是周日
	if !s0.matches(sun) || !s7.matches(sun) {
		t.Fatalf("周日双表示应同时命中")
	}
}
