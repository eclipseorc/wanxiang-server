package scrcpy

import (
	"encoding/binary"
	"testing"
)

// 校验触摸事件编码的字节布局与 scrcpy 协议一致。
func TestEncodeTouchDownLayout(t *testing.T) {
	buf, err := encodeTouch(TouchAction{
		Action: actionDown, X: 100, Y: 200, Width: 1080, Height: 1920,
		Pointer: pointerIDMouse,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(buf) != 32 {
		t.Fatalf("touch frame size = %d, want 32", len(buf))
	}
	if buf[0] != msgInjectTouchEvent {
		t.Fatalf("type = %d, want %d", buf[0], msgInjectTouchEvent)
	}
	if buf[1] != byte(actionDown) {
		t.Fatalf("action = %d, want down(%d)", buf[1], actionDown)
	}
	if got := binary.BigEndian.Uint64(buf[2:10]); got != pointerIDMouse {
		t.Fatalf("pointer = %x, want mouse pointer", got)
	}
	if got := binary.BigEndian.Uint32(buf[10:14]); got != 100 {
		t.Fatalf("x = %d, want 100", got)
	}
	if got := binary.BigEndian.Uint32(buf[14:18]); got != 200 {
		t.Fatalf("y = %d, want 200", got)
	}
	if got := binary.BigEndian.Uint16(buf[22:24]); got != 0xFFFF {
		t.Fatalf("pressure = %d, want 65535 on down", got)
	}
}

// 抬起事件的压力应为 0。
func TestEncodeTouchUpPressure(t *testing.T) {
	buf, err := encodeTouch(TouchAction{
		Action: actionUp, X: 0, Y: 0, Width: 1080, Height: 1920,
		Pointer: pointerIDMouse,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint16(buf[22:24]); got != 0 {
		t.Fatalf("pressure = %d, want 0 on up", got)
	}
}

// action_button / buttons 字段是 4 字节 u32（大端），字节序错位会导致
// 点击被设备识别成长按，这里显式校验布局。
func TestEncodeTouchButtonFields(t *testing.T) {
	down, _ := encodeTouch(TouchAction{
		Action: actionDown, X: 0, Y: 0, Width: 1080, Height: 1920, Pointer: pointerIDMouse,
	})
	if got := binary.BigEndian.Uint32(down[24:28]); got != 1 {
		t.Fatalf("down action_button = %d, want 1 (PRIMARY)", got)
	}
	if got := binary.BigEndian.Uint32(down[28:32]); got != 1 {
		t.Fatalf("down buttons = %d, want 1 (PRIMARY)", got)
	}

	up, _ := encodeTouch(TouchAction{
		Action: actionUp, X: 0, Y: 0, Width: 1080, Height: 1920, Pointer: pointerIDMouse,
	})
	if got := binary.BigEndian.Uint32(up[24:28]); got != 1 {
		t.Fatalf("up action_button = %d, want 1 (PRIMARY released)", got)
	}
	if got := binary.BigEndian.Uint32(up[28:32]); got != 0 {
		t.Fatalf("up buttons = %d, want 0 (none pressed)", got)
	}
}

// 越界坐标应返回错误而不是生成非法帧。
func TestEncodeTouchRejectsOutOfRange(t *testing.T) {
	if _, err := encodeTouch(TouchAction{
		Action: actionDown, X: 2000, Y: 0, Width: 1080, Height: 1920,
		Pointer: pointerIDMouse,
	}); err == nil {
		t.Fatal("expected error for x beyond screen width")
	}
	if _, err := encodeTouch(TouchAction{
		Action: actionMove, X: 0, Y: 0, Width: 0, Height: 1920,
		Pointer: pointerIDMouse,
	}); err == nil {
		t.Fatal("expected error for zero width")
	}
	if _, err := encodeTouch(TouchAction{
		Action: 99, X: 0, Y: 0, Width: 1080, Height: 1920,
		Pointer: pointerIDMouse,
	}); err == nil {
		t.Fatal("expected error for invalid action")
	}
}

// 返回键组合应产生 down+up 成对事件，顺序正确。
func TestEncodeKeyCombo(t *testing.T) {
	// Android KEYCODE_BACK = 4。
	buf := encodeKeyCombo(4)
	if len(buf) != 28 {
		t.Fatalf("combo size = %d, want 28 (down14 + up14)", len(buf))
	}
	// 第一个是 down。
	if buf[1] != 0 {
		t.Fatalf("first event action = %d, want down(0)", buf[1])
	}
	// 第二个是 up。
	if buf[15] != 1 {
		t.Fatalf("second event action = %d, want up(1)", buf[15])
	}
	if got := binary.BigEndian.Uint32(buf[16:20]); got != 4 {
		t.Fatalf("up keycode = %d, want 4", got)
	}
}

// 文本编码应包含长度前缀和原始 UTF-8 内容。
func TestEncodeText(t *testing.T) {
	buf, err := encodeText("你好")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("你好")
	if buf[0] != msgInjectText {
		t.Fatalf("type = %d, want %d", buf[0], msgInjectText)
	}
	if got := binary.BigEndian.Uint32(buf[1:5]); got != uint32(len(want)) {
		t.Fatalf("length = %d, want %d", got, len(want))
	}
	if string(buf[5:]) != "你好" {
		t.Fatalf("text = %q, want 你好", buf[5:])
	}
}

// 空文本和超长文本都应被拒绝。
func TestEncodeTextRejectsInvalidLength(t *testing.T) {
	if _, err := encodeText(""); err == nil {
		t.Fatal("expected error for empty text")
	}
	tooLong := make([]byte, maxTextBytes+1)
	for i := range tooLong {
		tooLong[i] = 'a'
	}
	if _, err := encodeText(string(tooLong)); err == nil {
		t.Fatal("expected error for overlong text")
	}
}

// 滚动事件校验边界与定点数转换。
func TestEncodeScroll(t *testing.T) {
	buf, err := encodeScroll(ScrollAction{
		X: 10, Y: 20, Width: 1080, Height: 1920, HScroll: 0, VScroll: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(buf) != 21 {
		t.Fatalf("scroll size = %d, want 21", len(buf))
	}
	if got := int16(binary.BigEndian.Uint16(buf[15:17])); got != -0x7FFF {
		t.Fatalf("vscroll = %d, want -32767", got)
	}
	if _, err := encodeScroll(ScrollAction{
		X: 0, Y: 0, Width: 1080, Height: 1920, VScroll: 2,
	}); err == nil {
		t.Fatal("expected error for scroll beyond 1")
	}
}

// 帧头解析：8 字节 pts + 4 字节长度。
func TestParseFrameMeta(t *testing.T) {
	raw := make([]byte, frameMetaSize)
	binary.BigEndian.PutUint64(raw[0:8], 123456789)
	binary.BigEndian.PutUint32(raw[8:12], 4096)
	meta, err := parseFrameMeta(raw)
	if err != nil {
		t.Fatal(err)
	}
	if meta.PTS != 123456789 || meta.Size != 4096 {
		t.Fatalf("meta = %+v, want pts=123456789 size=4096", meta)
	}
	if _, err := parseFrameMeta(raw[:5]); err == nil {
		t.Fatal("expected error for truncated frame meta")
	}
}

// codec 头解析：识别 h264/h265 与分辨率，拒绝未知 codec，兼容 12/16 字节两种格式。
func TestParseCodecHeader(t *testing.T) {
	// 12 字节格式。
	raw := make([]byte, codecHeaderSize)
	copy(raw[0:4], "h264")
	binary.BigEndian.PutUint32(raw[4:8], 1080)
	binary.BigEndian.PutUint32(raw[8:12], 1920)
	hdr, err := parseCodecHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Codec != CodecH264 || hdr.Width != 1080 || hdr.Height != 1920 || hdr.Size != 12 {
		t.Fatalf("got %+v, want h264 1080x1920 size=12", hdr)
	}

	// 16 字节扩展格式（codec+flags+width+height）。
	ext := make([]byte, codecHeaderSizeExtended)
	copy(ext[0:4], "h264")
	binary.BigEndian.PutUint32(ext[4:8], 0x80000000) // flags
	binary.BigEndian.PutUint32(ext[8:12], 1024)
	binary.BigEndian.PutUint32(ext[12:16], 456)
	hdr, err = parseCodecHeader(ext)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Width != 1024 || hdr.Height != 456 || hdr.Size != 16 || hdr.Flags != 0x80000000 {
		t.Fatalf("extended header got %+v, want 1024x456 size=16 flags=0x80000000", hdr)
	}

	copy(raw[0:4], "av1")
	if _, err := parseCodecHeader(raw); err == nil {
		t.Fatal("expected error for unsupported codec")
	}
}

// isSessionPacket 应正确区分 v5 的 session packet（首字节最高位=1）与 media packet。
func TestIsSessionPacket(t *testing.T) {
	if !isSessionPacket([]byte{0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}) {
		t.Fatal("session packet with MSB set should be recognized")
	}
	if isSessionPacket([]byte{0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}) {
		t.Fatal("media packet with MSB clear should not be session packet")
	}
}

// parseSessionPacket 应解析出 v5 session packet 里的视频分辨率。
func TestParseSessionPacket(t *testing.T) {
	pkt := make([]byte, sessionPacketSize)
	pkt[0] = 0x80 // session packet flag
	binary.BigEndian.PutUint32(pkt[4:8], 720)
	binary.BigEndian.PutUint32(pkt[8:12], 1612)
	w, h, err := parseSessionPacket(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if w != 720 || h != 1612 {
		t.Fatalf("got %dx%d, want 720x1612", w, h)
	}
}
