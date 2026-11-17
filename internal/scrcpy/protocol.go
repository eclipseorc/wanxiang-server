// Package scrcpy 把 scrcpy-server 的视频流与控制协议封装成可复用的 Go 组件。
//
// scrcpy 的通信基于两条 TCP 连接（由 adb forward 转发设备上的 localabstract:scrcpy）：
//   - 视频连接：设备先发 codec 头（h264/h265 + 宽高），随后是连续的 H.264/H.265 帧，
//     每帧前有 12 字节的 FrameMeta（8 字节 pts + 4 字节帧长度）。
//   - 控制连接：客户端按 scrcpy 二进制协议写入控制事件（触摸、按键、文本、滚动等），
//     全部使用大端序。
//
// 本文件只做协议编解码，不涉及 adb、连接或会话管理，方便独立单元测试。
package scrcpy

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// 控制事件类型，与 scrcpy-server 的 ControlMessage 枚举保持一致。
const (
	msgInjectKeycode    byte = 0
	msgInjectText       byte = 1
	msgInjectTouchEvent byte = 2
	msgInjectScroll     byte = 3
	msgBackOrScreenOn   byte = 4
)

// 触摸动作：按下、抬起、移动。
const (
	actionDown int = 0
	actionUp   int = 1
	actionMove int = 2
)

// pointerIDMouse 是 scrcpy 保留的“虚拟指针”标识，表示事件来自鼠标/远程指针，
// 而非设备上的真实多指触摸。
const pointerIDMouse uint64 = 0xFFFFFFFFFFFFFFFF

// 屏幕尺寸上限，防止异常参数构造出超大坐标。
const (
	maxScreenSize uint32 = 1 << 16 // 65536
	maxTextBytes  int    = 300
)

// ErrInvalidControl 表示控制事件参数非法，编码无法继续。
var ErrInvalidControl = errors.New("invalid control event")

// TouchAction 描述一次触摸事件的完整信息。坐标与屏幕尺寸都使用设备真实分辨率，
// 由 scrcpy-server 负责按窗口缩放换算。
type TouchAction struct {
	Action  int    // actionDown / actionUp / actionMove
	X       uint32 // 相对屏幕的横坐标
	Y       uint32 // 相对屏幕的纵坐标
	Width   uint32 // 设备屏幕宽
	Height  uint32 // 设备屏幕高
	Pointer uint64 // 指针 ID，鼠标场景用 pointerIDMouse
}

// encodeTouch 把触摸事件编码成 32 字节的 scrcpy 控制帧。
func encodeTouch(a TouchAction) ([]byte, error) {
	if a.Width == 0 || a.Height == 0 || a.Width > maxScreenSize || a.Height > maxScreenSize {
		return nil, fmt.Errorf("%w: invalid screen size %dx%d", ErrInvalidControl, a.Width, a.Height)
	}
	if a.X >= a.Width || a.Y >= a.Height {
		return nil, fmt.Errorf("%w: coordinate (%d,%d) out of %dx%d", ErrInvalidControl, a.X, a.Y, a.Width, a.Height)
	}
	switch a.Action {
	case actionDown, actionUp, actionMove:
	default:
		return nil, fmt.Errorf("%w: invalid action %d", ErrInvalidControl, a.Action)
	}

	buf := make([]byte, 32)
	buf[0] = msgInjectTouchEvent
	buf[1] = byte(a.Action)
	binary.BigEndian.PutUint64(buf[2:10], a.Pointer)
	binary.BigEndian.PutUint32(buf[10:14], a.X)
	binary.BigEndian.PutUint32(buf[14:18], a.Y)
	binary.BigEndian.PutUint16(buf[18:20], uint16(a.Width))
	binary.BigEndian.PutUint16(buf[20:22], uint16(a.Height))

	// action_button 与 buttons 是 4 字节 u32（大端），不能用单字节赋值，
	// 否则会因字节序错位被设备解析成错误的按钮状态（导致点击被识别成长按）。
	// 按钮位掩码：PRIMARY=1（左键），SECONDARY=2（右键）。
	switch a.Action {
	case actionDown:
		binary.BigEndian.PutUint16(buf[22:24], 0xFFFF) // pressure 最大
		binary.BigEndian.PutUint32(buf[24:28], 1)      // action_button = PRIMARY
		binary.BigEndian.PutUint32(buf[28:32], 1)      // buttons = PRIMARY
	case actionUp:
		binary.BigEndian.PutUint16(buf[22:24], 0)      // pressure 归零
		binary.BigEndian.PutUint32(buf[24:28], 1)      // action_button = 释放的是 PRIMARY
		binary.BigEndian.PutUint32(buf[28:32], 0)      // buttons = 无按下
	default: // move
		binary.BigEndian.PutUint16(buf[22:24], 0xFFFF) // 移动保持压力
		binary.BigEndian.PutUint32(buf[24:28], 0)      // action_button = 无按钮变化
		binary.BigEndian.PutUint32(buf[28:32], 1)      // buttons = 仍在按住 PRIMARY
	}
	return buf, nil
}

// KeyAction 描述一次按键事件。Keycode 使用 Android KeyEvent 的键值。
type KeyAction struct {
	Down    bool
	Keycode uint32
}

// encodeKeycode 把单个按键按下/抬起编码成 14 字节控制帧。
func encodeKeycode(a KeyAction) []byte {
	buf := make([]byte, 14)
	buf[0] = msgInjectKeycode
	if a.Down {
		buf[1] = 0 // ACTION_DOWN
	} else {
		buf[1] = 1 // ACTION_UP
	}
	binary.BigEndian.PutUint32(buf[2:6], a.Keycode)
	// repeat=0, metastate=0，由后续字段默认零值表示。
	return buf
}

// encodeKeyCombo 把一组键值编码为“按下再抬起”的事件序列，用于返回键、Home 等
// 需要成对 down/up 的组合操作。
func encodeKeyCombo(keycodes ...uint32) []byte {
	out := make([]byte, 0, len(keycodes)*28)
	for _, key := range keycodes {
		out = append(out, encodeKeycode(KeyAction{Down: true, Keycode: key})...)
	}
	// 抬起顺序与按下相反，模拟真实按键释放。
	for i := len(keycodes) - 1; i >= 0; i-- {
		out = append(out, encodeKeycode(KeyAction{Down: false, Keycode: keycodes[i]})...)
	}
	return out
}

// encodeText 把 UTF-8 文本编码为控制帧，用于向输入框写入文字（含中文）。
func encodeText(text string) ([]byte, error) {
	data := []byte(text)
	if len(data) == 0 || len(data) > maxTextBytes {
		return nil, fmt.Errorf("%w: text length %d out of range", ErrInvalidControl, len(data))
	}
	buf := make([]byte, 5+len(data))
	buf[0] = msgInjectText
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(data)))
	copy(buf[5:], data)
	return buf, nil
}

// ScrollAction 描述一次滚动事件。HScroll/VScroll 是归一化到 [-1,1] 的定点数。
type ScrollAction struct {
	X       uint32
	Y       uint32
	Width   uint32
	Height  uint32
	HScroll float64
	VScroll float64
}

// encodeScroll 把滚动事件编码成 21 字节控制帧。
func encodeScroll(a ScrollAction) ([]byte, error) {
	if a.Width == 0 || a.Height == 0 || a.Width > maxScreenSize || a.Height > maxScreenSize {
		return nil, fmt.Errorf("%w: invalid screen size %dx%d", ErrInvalidControl, a.Width, a.Height)
	}
	if a.HScroll < -1 || a.HScroll > 1 || a.VScroll < -1 || a.VScroll > 1 {
		return nil, fmt.Errorf("%w: scroll must be within [-1,1]", ErrInvalidControl)
	}

	buf := make([]byte, 21)
	buf[0] = msgInjectScroll
	binary.BigEndian.PutUint32(buf[1:5], a.X)
	binary.BigEndian.PutUint32(buf[5:9], a.Y)
	binary.BigEndian.PutUint16(buf[9:11], uint16(a.Width))
	binary.BigEndian.PutUint16(buf[11:13], uint16(a.Height))
	// scrcpy 用 i16 定点数表示 [-1,1] 的滚动量，按 0x7FFF 归一化。
	binary.BigEndian.PutUint16(buf[13:15], uint16(int16(a.HScroll*0x7FFF)))
	binary.BigEndian.PutUint16(buf[15:17], uint16(int16(a.VScroll*0x7FFF)))
	// buttons 固定为 1，表示有按键状态。
	buf[17] = 1
	return buf, nil
}

// encodeBackOrScreenOn 编码“返回或亮屏”事件，用于安卓 12+ 的合成返回手势。
func encodeBackOrScreenOn() []byte {
	return []byte{msgBackOrScreenOn, 0}
}

// frameMetaSize 是 scrcpy 视频帧头（FrameMeta）的字节数：8 字节 pts + 4 字节长度。
const frameMetaSize = 12

// codecHeaderSize 是 codec 头的最小字节数：4 字节 codec 标识 + 4 字节宽度 + 4 字节高度。
const codecHeaderSize = 12

// codecHeaderSizeExtended 是 scrcpy-server v4 的扩展头大小：额外带 4 字节 flags。
// 部分构建版本的头是 codec+flags+width+height 共 16 字节。
const codecHeaderSizeExtended = 16

// FrameMeta 表示一帧视频的元信息，从视频连接的数据流中解析得到。
type FrameMeta struct {
	PTS  uint64
	Size uint32
}

// parseFrameMeta 从字节流中读取 12 字节的帧头。
func parseFrameMeta(data []byte) (FrameMeta, error) {
	if len(data) < frameMetaSize {
		return FrameMeta{}, fmt.Errorf("frame meta truncated: need %d bytes, got %d", frameMetaSize, len(data))
	}
	return FrameMeta{
		PTS:  binary.BigEndian.Uint64(data[0:8]),
		Size: binary.BigEndian.Uint32(data[8:12]),
	}, nil
}

// Codec 表示视频流的编码格式。
type Codec string

const (
	CodecH264 Codec = "h264"
	CodecH265 Codec = "h265"
)

// CodecHeader 描述视频连接开头的 codec 信息，含编码格式、flags 与分辨率。
type CodecHeader struct {
	Codec  Codec
	Flags  uint32
	Width  uint32
	Height uint32
	// Size 是 codec 头实际占用的字节数（12 或 16），供调用方正确跳过。
	Size int
}

// parseCodecHeader 解析视频连接开头的 codec 头，兼容 12 字节与 16 字节两种格式。
// 先按 12 字节读 width/height，若分辨率不合理（疑似把 flags 读成了 width），
// 再按 16 字节（codec+flags+width+height）重读，与 scrcpy v4 的容错一致。
func parseCodecHeader(data []byte) (CodecHeader, error) {
	if len(data) < codecHeaderSize {
		return CodecHeader{}, fmt.Errorf("codec header truncated: need %d bytes, got %d", codecHeaderSize, len(data))
	}
	codecName := string(data[0:4])
	switch codecName {
	case "h264", "h265":
	default:
		return CodecHeader{}, fmt.Errorf("unsupported codec %q", codecName)
	}

	width := binary.BigEndian.Uint32(data[4:8])
	height := binary.BigEndian.Uint32(data[8:12])

	// 12 字节格式：宽高都应是合理值。
	if plausibleSize(width) && plausibleSize(height) {
		return CodecHeader{Codec: Codec(codecName), Width: width, Height: height, Size: codecHeaderSize}, nil
	}

	// 16 字节扩展格式：codec+flags+width+height。
	if len(data) >= codecHeaderSizeExtended {
		flags := binary.BigEndian.Uint32(data[4:8])
		extWidth := binary.BigEndian.Uint32(data[8:12])
		extHeight := binary.BigEndian.Uint32(data[12:16])
		if plausibleSize(extWidth) && plausibleSize(extHeight) {
			return CodecHeader{
				Codec: Codec(codecName), Flags: flags,
				Width: extWidth, Height: extHeight, Size: codecHeaderSizeExtended,
			}, nil
		}
	}
	return CodecHeader{}, fmt.Errorf("unrecognized codec header layout (width=%d height=%d)", width, height)
}

// plausibleSize 判断分辨率是否为合理值（0 < v <= 10000）。
func plausibleSize(v uint32) bool {
	return v > 0 && v <= 10000
}
