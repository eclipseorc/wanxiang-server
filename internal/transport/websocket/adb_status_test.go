package websocket

import (
	"reflect"
	"testing"
)

// TestSerialIPSet 验证把 adb 在线 serial 列表转换成 IP 集合的逻辑。
func TestSerialIPSet(t *testing.T) {
	got := serialIPSet([]string{
		"192.168.1.100:5555",
		"192.168.1.101:5555",
		"192.168.1.100:4444", // 同一 IP 不同端口，应去重
		"invalid-serial",     // 无冒号，应被忽略
	})
	want := map[string]bool{
		"192.168.1.100": true,
		"192.168.1.101": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("serialIPSet = %v, want %v", got, want)
	}
}

// TestSerialIPSetEmpty 验证空列表返回空集合。
func TestSerialIPSetEmpty(t *testing.T) {
	got := serialIPSet(nil)
	if len(got) != 0 {
		t.Fatalf("serialIPSet(nil) = %v, want empty", got)
	}
}

// TestSerialIPSetIPv6 验证 IPv6 serial（含多个冒号）能正确截取第一个冒号前作为地址。
func TestSerialIPSetIPv6(t *testing.T) {
	got := serialIPSet([]string{"fe80::abcd:5555"})
	if got["fe80"] != true {
		t.Fatalf("serialIPSet IPv6 = %v, want key fe80", got)
	}
}

// TestMdnsAddrByIP 验证把 mDNS 地址列表转成 IP → 地址映射。
func TestMdnsAddrByIP(t *testing.T) {
	got := mdnsAddrByIP([]string{
		"192.168.41.177:35583",
		"192.168.41.178:40000",
	})
	want := map[string]string{
		"192.168.41.177": "192.168.41.177:35583",
		"192.168.41.178": "192.168.41.178:40000",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mdnsAddrByIP = %v, want %v", got, want)
	}
}

// TestMdnsAddrByIPEmpty 验证空列表返回空映射。
func TestMdnsAddrByIPEmpty(t *testing.T) {
	if got := mdnsAddrByIP(nil); len(got) != 0 {
		t.Fatalf("mdnsAddrByIP(nil) = %v, want empty", got)
	}
}
