package scrcpy

import (
	"encoding/binary"
	"testing"
)

// 浏览器发来的触摸事件应正确编码为 scrcpy 二进制。
func TestEncodeControlMessageTouch(t *testing.T) {
	raw := []byte(`{"type":"touch","action":"down","x":50,"y":60,"width":1080,"height":1920}`)
	buf, err := encodeControlMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(buf) != 32 || buf[0] != msgInjectTouchEvent {
		t.Fatalf("touch frame invalid: len=%d type=%d", len(buf), buf[0])
	}
	if got := binary.BigEndian.Uint32(buf[10:14]); got != 50 {
		t.Fatalf("x = %d, want 50", got)
	}
	if got := binary.BigEndian.Uint32(buf[14:18]); got != 60 {
		t.Fatalf("y = %d, want 60", got)
	}
}

// 文本与按键事件。
func TestEncodeControlMessageTextAndKey(t *testing.T) {
	buf, err := encodeControlMessage([]byte(`{"type":"text","text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if buf[0] != msgInjectText {
		t.Fatalf("type = %d, want text", buf[0])
	}

	buf, err = encodeControlMessage([]byte(`{"type":"key","action":"down","key":4}`))
	if err != nil {
		t.Fatal(err)
	}
	if buf[0] != msgInjectKeycode || buf[1] != 0 {
		t.Fatalf("key frame invalid: type=%d action=%d", buf[0], buf[1])
	}
}

// 滚动事件应编码为 scrcpy scroll 帧（此前前端发 scroll 但后端漏处理，
// 导致 "invalid control type" 报错）。
func TestEncodeControlMessageScroll(t *testing.T) {
	buf, err := encodeControlMessage([]byte(`{"type":"scroll","x":100,"y":200,"width":1080,"height":1920,"hScroll":0,"vScroll":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(buf) != 21 || buf[0] != msgInjectScroll {
		t.Fatalf("scroll frame invalid: len=%d type=%d", len(buf), buf[0])
	}
	// vScroll=1 应归一化为 +0x7FFF 的 i16 定点数。
	if got := int16(binary.BigEndian.Uint16(buf[15:17])); got != 0x7FFF {
		t.Fatalf("vScroll = %d, want %d", got, 0x7FFF)
	}
}

// 未知类型与非法 action 应报错。
func TestEncodeControlMessageRejectsInvalid(t *testing.T) {
	if _, err := encodeControlMessage([]byte(`{"type":"nope"}`)); err == nil {
		t.Fatal("expected error for unknown type")
	}
	if _, err := encodeControlMessage([]byte(`{"type":"touch","action":"swipe","x":0,"y":0,"width":1080,"height":1920}`)); err == nil {
		t.Fatal("expected error for invalid action")
	}
}
