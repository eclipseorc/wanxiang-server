package automation

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/eclipseorc/wanxiang-server/internal/scrcpy"
)

// FastUITreeProvider 是由 WebSocket Hub 注入的极速无障碍控件树抓取函数（~50ms）。
type FastUITreeProvider func(ctx context.Context, deviceID string) ([]byte, error)

var (
	fastUITreeMu       sync.RWMutex
	fastUITreeProvider FastUITreeProvider
)

// RegisterFastUITreeProvider 注册 WebSocket 极速控件树提供者（通常由 app.RunWebsocket 启动时注入）。
func RegisterFastUITreeProvider(fn FastUITreeProvider) {
	fastUITreeMu.Lock()
	fastUITreeProvider = fn
	fastUITreeMu.Unlock()
}

// DumpUIHierarchyForDevice 优先通过 WebSocket 无障碍通道 50ms 级秒抓控件树；
// 若设备未开无障碍或未连 WS，自动无缝回退到传统 ADB `uiautomator dump`。
func DumpUIHierarchyForDevice(ctx context.Context, adb *scrcpy.ADB, deviceID, serial string) ([]UINode, error) {
	if strings.TrimSpace(deviceID) != "" {
		fastUITreeMu.RLock()
		fn := fastUITreeProvider
		fastUITreeMu.RUnlock()
		if fn != nil {
			if raw, err := fn(ctx, deviceID); err == nil && len(raw) > 2 {
				var nodes []UINode
				if err := json.Unmarshal(raw, &nodes); err == nil && len(nodes) > 0 {
					return nodes, nil
				}
			}
		}
	}
	return DumpUIHierarchy(ctx, adb, serial)
}

// UINode 表示手机屏幕上无障碍/UIAutomator 控件树中的一个可见控件节点。
// 既供后端 clickElement / waitForElement / ifElementExists 查找定位，
// 也通过 /api/admin/automation/ui-tree 接口返回给前端 Scrcpy 画面做悬停高亮拾取。
type UINode struct {
	Text        string `json:"text"`
	ResourceID  string `json:"resId"`
	ContentDesc string `json:"desc"`
	ClassName   string `json:"className"`
	Package     string `json:"pkg"`
	Clickable   bool   `json:"clickable"`
	Checkable   bool   `json:"checkable"`
	Checked     bool   `json:"checked"`
	Enabled     bool   `json:"enabled"`
	Bounds      [4]int `json:"bounds"` // [left, top, right, bottom]
	CenterX     int    `json:"centerX"`
	CenterY     int    `json:"centerY"`
}

var boundsPattern = regexp.MustCompile(`\[(\d+),(\d+)\]\[(\d+),(\d+)\]`)

// DumpUIHierarchy 从目标设备抓取当前屏幕的 UI 控件树列表。
// 优先使用 `uiautomator dump /dev/tty` 直接输出到标准输出（免写文件、更快）；
// 若部分 ROM 不支持 /dev/tty，则自动回退到 `/data/local/tmp/wx_uidump.xml`。
func DumpUIHierarchy(ctx context.Context, adb *scrcpy.ADB, serial string) ([]UINode, error) {
	if adb == nil {
		return nil, fmt.Errorf("adb not available")
	}
	out, err := adb.Shell(ctx, serial, "uiautomator dump /dev/tty")
	if err != nil || !strings.Contains(out, "<hierarchy") {
		out, err = adb.Shell(ctx, serial, "uiautomator dump /data/local/tmp/wx_uidump.xml >/dev/null 2>&1 && cat /data/local/tmp/wx_uidump.xml")
		if err != nil {
			return nil, fmt.Errorf("dump ui hierarchy failed: %w", err)
		}
	}
	idx := strings.Index(out, "<hierarchy")
	if idx < 0 {
		return nil, fmt.Errorf("ui hierarchy xml not found in dump output")
	}
	endIdx := strings.LastIndex(out, "</hierarchy>")
	xmlContent := out[idx:]
	if endIdx > idx {
		xmlContent = out[idx : endIdx+len("</hierarchy>")]
	}
	return ParseUIHierarchyXML(xmlContent)
}

// ParseUIHierarchyXML 解析 uiautomator dump 产出的 XML 字符串为扁平控件列表。
func ParseUIHierarchyXML(xmlContent string) ([]UINode, error) {
	decoder := xml.NewDecoder(strings.NewReader(xmlContent))
	nodes := make([]UINode, 0, 64)
	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nodes, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "node" {
			continue
		}
		var n UINode
		n.Enabled = true
		var boundsStr string
		for _, attr := range se.Attr {
			switch attr.Name.Local {
			case "text":
				n.Text = attr.Value
			case "resource-id":
				n.ResourceID = attr.Value
			case "content-desc":
				n.ContentDesc = attr.Value
			case "class":
				n.ClassName = attr.Value
			case "package":
				n.Package = attr.Value
			case "clickable":
				n.Clickable = attr.Value == "true"
			case "checkable":
				n.Checkable = attr.Value == "true"
			case "checked":
				n.Checked = attr.Value == "true"
			case "enabled":
				n.Enabled = attr.Value == "true"
			case "bounds":
				boundsStr = attr.Value
			}
		}
		if b, ok := parseBounds(boundsStr); ok {
			// 过滤宽高为 0 的不可见节点
			if b[2] > b[0] && b[3] > b[1] {
				n.Bounds = b
				n.CenterX = (b[0] + b[2]) / 2
				n.CenterY = (b[1] + b[3]) / 2
				nodes = append(nodes, n)
			}
		}
	}
	return nodes, nil
}

func parseBounds(s string) ([4]int, bool) {
	var b [4]int
	m := boundsPattern.FindStringSubmatch(s)
	if len(m) != 5 {
		return b, false
	}
	for i := 0; i < 4; i++ {
		v, err := strconv.Atoi(m[i+1])
		if err != nil {
			return b, false
		}
		b[i] = v
	}
	return b, true
}

// FindBestNode 在控件列表中按匹配方式（by: text / id / desc / class / any）查找最优节点。
// 评分策略：完全相等 > 包含匹配；可点击控件优先；面积更小（更具体的子控件）优先。
func FindBestNode(nodes []UINode, by, target string) (UINode, bool) {
	by = strings.ToLower(strings.TrimSpace(by))
	if by == "" {
		by = "any"
	}
	nt := normalizeQuotes(strings.TrimSpace(target))
	if nt == "" {
		return UINode{}, false
	}
	ntLower := strings.ToLower(nt)

	var best UINode
	bestScore := -1
	for _, n := range nodes {
		score := matchNodeScore(n, by, ntLower)
		if score <= 0 {
			continue
		}
		if n.Clickable {
			score += 50
		}
		// 面积越小说明越贴近具体按钮而非全屏父容器
		area := (n.Bounds[2] - n.Bounds[0]) * (n.Bounds[3] - n.Bounds[1])
		if area > 0 && area < 200000 {
			score += 20
		}
		if score > bestScore {
			bestScore = score
			best = n
		}
	}
	return best, bestScore > 0
}

func matchNodeScore(n UINode, by, targetLower string) int {
	scoreField := func(val string) int {
		v := strings.ToLower(strings.TrimSpace(normalizeQuotes(val)))
		if v == "" {
			return 0
		}
		if v == targetLower {
			return 1000
		}
		if strings.Contains(v, targetLower) {
			return 200 - minInt(len(v), 100)
		}
		return 0
	}

	switch by {
	case "text":
		return maxInt(scoreField(n.Text), scoreField(n.ContentDesc))
	case "id":
		return scoreField(n.ResourceID)
	case "desc":
		return scoreField(n.ContentDesc)
	case "class":
		return scoreField(n.ClassName)
	default: // "any"
		return maxInt(
			maxInt(scoreField(n.Text), scoreField(n.ContentDesc)),
			scoreField(n.ResourceID),
		)
	}
}

func normalizeQuotes(s string) string {
	r := strings.NewReplacer(
		"\u201c", `"`, "\u201d", `"`, "「", `"`, "」", `"`,
		"\u2018", `'`, "\u2019", `'`, "\u3000", " ",
	)
	return r.Replace(s)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
