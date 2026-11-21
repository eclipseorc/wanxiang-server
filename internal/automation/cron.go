package automation

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cronSchedule 是一个极简的 cron 表达式解析器,只支持标准五字段(分 时 日 月 周),
// 精度到分钟。相比引入 robfig/cron,这里零依赖、覆盖自动化场景的绝大多数需求
// (如「每天 9 点」「每小时第 0 分」「工作日 9 点」)。
//
// 语法:
//   - `*`      任意值
//   - `N`      精确值(分/时 0-59、日 1-31、月 1-12、周 0-6,0 与 7 均表示周日)
//   - `N,M`    枚举
//   - `N-M`    闭区间
//   - `*/N`    步长(从最小值起每 N)
type cronSchedule struct {
	minute, hour, dom, month, dow map[int]bool
}

// parseCron 解析 cron 表达式,返回 schedule 或错误。字段顺序:分 时 日 月 周。
func parseCron(expr string) (*cronSchedule, error) {
	expr = strings.TrimSpace(expr)
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron 需 5 个字段(分 时 日 月 周),got %d", len(fields))
	}
	s := &cronSchedule{}
	var err error
	if s.minute, err = parseField(fields[0], 0, 59); err != nil {
		return nil, fmt.Errorf("分字段: %w", err)
	}
	if s.hour, err = parseField(fields[1], 0, 23); err != nil {
		return nil, fmt.Errorf("时字段: %w", err)
	}
	if s.dom, err = parseField(fields[2], 1, 31); err != nil {
		return nil, fmt.Errorf("日字段: %w", err)
	}
	if s.month, err = parseField(fields[3], 1, 12); err != nil {
		return nil, fmt.Errorf("月字段: %w", err)
	}
	if s.dow, err = parseField(fields[4], 0, 7); err != nil { // 周 0-7,7 与 0 均表周日,由 normalize 归一
		return nil, fmt.Errorf("周字段: %w", err)
	}
	return s, nil
}

// matches 判断给定时间是否命中该 cron 表达式。
func (s *cronSchedule) matches(t time.Time) bool {
	// 周日:time.Weekday() 返回 0,与 cron 的 0 一致;cron 的 7 在 parseField 已归一为 0。
	dow := int(t.Weekday())
	return s.minute[t.Minute()] && s.hour[t.Hour()] &&
		s.dom[t.Day()] && s.month[int(t.Month())] && s.dow[dow]
}

// parseField 解析单个 cron 字段为命中值集合。
func parseField(field string, min, max int) (map[int]bool, error) {
	set := map[int]bool{}
	if field == "" {
		return nil, fmt.Errorf("空字段")
	}
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("空枚举项")
		}
		if err := addPart(set, part, min, max); err != nil {
			return nil, err
		}
	}
	return set, nil
}

// addPart 处理单个字段片段:`*`、`N`、`N-M`、`*/N`。
func addPart(set map[int]bool, part string, min, max int) error {
	// 步长 */N 或 N-M/N
	if strings.Contains(part, "/") {
		seg := strings.SplitN(part, "/", 2)
		step, err := strconv.Atoi(strings.TrimSpace(seg[1]))
		if err != nil || step <= 0 {
			return fmt.Errorf("非法步长 %q", seg[1])
		}
		lo, hi := min, max
		base := strings.TrimSpace(seg[0])
		if base != "*" {
			// 形如 N-M/N
			if !strings.Contains(base, "-") {
				return fmt.Errorf("步长基数需为 * 或 N-M: %q", part)
			}
			rng := strings.SplitN(base, "-", 2)
			lo, err = strconv.Atoi(strings.TrimSpace(rng[0]))
			if err != nil {
				return fmt.Errorf("非法区间起点 %q", rng[0])
			}
			hi, err = strconv.Atoi(strings.TrimSpace(rng[1]))
			if err != nil {
				return fmt.Errorf("非法区间终点 %q", rng[1])
			}
		}
		if lo < min || hi > max || lo > hi {
			return fmt.Errorf("区间 [%d,%d] 超出 [%d,%d]", lo, hi, min, max)
		}
		for v := lo; v <= hi; v += step {
			set[normalize(v)] = true
		}
		return nil
	}

	// 区间 N-M
	if strings.Contains(part, "-") {
		rng := strings.SplitN(part, "-", 2)
		lo, err := strconv.Atoi(strings.TrimSpace(rng[0]))
		if err != nil {
			return fmt.Errorf("非法区间起点 %q", rng[0])
		}
		hi, err := strconv.Atoi(strings.TrimSpace(rng[1]))
		if err != nil {
			return fmt.Errorf("非法区间终点 %q", rng[1])
		}
		if lo < min || hi > max || lo > hi {
			return fmt.Errorf("区间 [%d,%d] 超出 [%d,%d]", lo, hi, min, max)
		}
		for v := lo; v <= hi; v++ {
			set[normalize(v)] = true
		}
		return nil
	}

	// 通配 *
	if part == "*" {
		for v := min; v <= max; v++ {
			set[normalize(v)] = true
		}
		return nil
	}

	// 单值 N
	n, err := strconv.Atoi(part)
	if err != nil {
		return fmt.Errorf("非法值 %q", part)
	}
	if n < min || n > max {
		return fmt.Errorf("值 %d 超出 [%d,%d]", n, min, max)
	}
	set[normalize(n)] = true
	return nil
}

// normalize 把周日双表示(0 与 7)统一为 0。其余值原样返回。
func normalize(v int) int {
	if v == 7 {
		return 0
	}
	return v
}
