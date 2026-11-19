package scrcpy

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"testing"
)

// buildTestAXML 构造一个最小但结构正确的 AXML 字节流，manifest package 为
// 传入的 pkg。仅包含：XML 头、字符串池、一个 START_ELEMENT（manifest）+ 其
// package 属性、END_ELEMENT。字符串池用 UTF-8 编码，便于手工构造。
//
// 结构参考 Android AXML 二进制规范（小端、4 字节对齐）。
func buildTestAXML(pkg string) []byte {
	// —— 字符串池内容 ——
	// 需要的字符串：manifest、package、pkg 值。
	strs := []string{"manifest", "package", pkg}

	// 构造字符串数据区（UTF-8，每个：变长长度前缀 + bytes + NUL）。
	var strData []byte
	offsets := make([]uint32, len(strs))
	for i, s := range strs {
		offsets[i] = uint32(len(strData))
		b := []byte(s)
		// 变长长度前缀（utf-8）
		if len(b) > 0x7f {
			strData = append(strData, byte((len(b)>>8)|0x80), byte(len(b)))
		} else {
			strData = append(strData, byte(len(b)))
		}
		strData = append(strData, b...)
		strData = append(strData, 0) // NUL 结尾
	}
	// 4 字节对齐字符串数据
	for len(strData)%4 != 0 {
		strData = append(strData, 0)
	}

	// 字符串池 chunk 头：8 字节，body 从 headerSize 之后开始。
	stringPoolHeaderSize := 28
	stringPoolSize := stringPoolHeaderSize + len(offsets)*4 + len(strData)
	stringPoolChunk := make([]byte, 0, stringPoolSize)
	stringPoolChunk = appendChunkHeader(stringPoolChunk, chunkTypeStringPool, stringPoolSize, stringPoolHeaderSize)
	// stringCount, styleCount, flags(UTF8), stringsStart, stylesStart
	stringPoolChunk = appendU32(stringPoolChunk, uint32(len(strs)))                           // stringCount
	stringPoolChunk = appendU32(stringPoolChunk, 0)                                           // styleCount
	stringPoolChunk = appendU32(stringPoolChunk, stringPoolUTF8Flag)                          // flags: UTF-8
	stringPoolChunk = appendU32(stringPoolChunk, uint32(stringPoolHeaderSize+len(offsets)*4)) // stringsStart
	stringPoolChunk = appendU32(stringPoolChunk, 0)                                           // stylesStart
	for _, off := range offsets {
		stringPoolChunk = appendU32(stringPoolChunk, off)
	}
	stringPoolChunk = append(stringPoolChunk, strData...)

	// —— START_ELEMENT（manifest）——
	// 布局：header(8) + line(4) + comment(4) + ns(4) + name(4)
	//       + attrStart(2)+attrSize(2)+attrCount(2)+idIdx(2)+classIdx(2)+styleIdx(2)
	//       + attribute(20 bytes)
	// name 字段 = 字符串池索引 0（"manifest"）
	// attribute：ns=-1, name=索引1("package"), rawValue=索引2(pkg), typedValue(空)
	//
	// 注意：attribute 数据固定在 chunk 头偏移 36 处，但这里故意把 attributeStart
	// 字段写成非标准的 20（复现 AAPT2/uni-app 打包产物的真实行为），确保解析器
	// 不依赖该字段、始终用固定偏移 36 定位 attribute。
	startHeaderSize := 36
	attrSize := 20
	startSize := startHeaderSize + attrSize
	start := make([]byte, 0, startSize)
	start = appendChunkHeader(start, chunkTypeStartElement, startSize, startHeaderSize)
	start = appendU32(start, 0)                // line
	start = appendU32(start, 0xFFFFFFFF)       // comment = -1
	start = appendU32(start, 0xFFFFFFFF)       // ns = -1
	start = appendU32(start, 0)                // name = "manifest"
	start = appendU16(start, 20)               // attributeStart（故意非标准，实测值）
	start = appendU16(start, uint16(attrSize)) // attributeSize
	start = appendU16(start, 1)                // attributeCount
	start = appendU16(start, 0)                // idIndex
	start = appendU16(start, 0)                // classIndex
	start = appendU16(start, 0)                // styleIndex
	// attribute：ns(-1) name(1) rawValue(2) typedValue{size=8,res0,dataType,data}
	start = appendU32(start, 0xFFFFFFFF) // attr ns
	start = appendU32(start, 1)          // attr name = "package"
	start = appendU32(start, 2)          // attr rawValue = pkg
	start = appendU32(start, 0)          // typedValue size=0（占位）
	start = appendU32(start, 0)          // typedValue 第二字段（占位）

	// —— END_ELEMENT ——
	end := make([]byte, 0, 24)
	end = appendChunkHeader(end, chunkTypeEndElement, 24, 24)
	end = appendU32(end, 0)          // line
	end = appendU32(end, 0xFFFFFFFF) // comment
	end = appendU32(end, 0xFFFFFFFF) // ns
	end = appendU32(end, 0)          // name

	// —— XML 文件头 ——
	fileHeaderSize := 8
	total := fileHeaderSize + stringPoolSize + startSize + 24
	out := make([]byte, 0, total)
	out = appendChunkHeader(out, chunkTypeXML, total, fileHeaderSize)

	// 拼接
	out = append(out, stringPoolChunk...)
	out = append(out, start...)
	out = append(out, end...)
	return out
}

// appendChunkHeader 追加 8 字节 chunk 头（type + headerSize + size）。
func appendChunkHeader(dst []byte, typ uint16, size, headerSize int) []byte {
	dst = appendU16(dst, typ)
	dst = appendU16(dst, uint16(headerSize))
	dst = appendU32(dst, uint32(size))
	return dst
}

func appendU16(dst []byte, v uint16) []byte {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	return append(dst, b[:]...)
}

func appendU32(dst []byte, v uint32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	return append(dst, b[:]...)
}

// buildTestAPK 把 AndroidManifest.xml（AXML）包成一个最小 zip 返回。
func buildTestAPK(pkg string) []byte {
	axml := buildTestAXML(pkg)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("AndroidManifest.xml")
	_, _ = f.Write(axml)
	_ = zw.Close()
	return buf.Bytes()
}

func TestParseManifestPackage(t *testing.T) {
	cases := []string{
		"com.example.app",
		"com.example.mytest",
		"a.b.c.d.e",
	}
	for _, pkg := range cases {
		axml := buildTestAXML(pkg)
		got, err := parseManifestPackage(axml)
		if err != nil {
			t.Fatalf("parseManifestPackage(%q) error: %v", pkg, err)
		}
		if got != pkg {
			t.Fatalf("parseManifestPackage(%q) = %q, want %q", pkg, got, pkg)
		}
	}
}

func TestAPKPackageName(t *testing.T) {
	apk := buildTestAPK("com.example.zipped")
	got, err := APKPackageName(apk)
	if err != nil {
		t.Fatalf("APKPackageName error: %v", err)
	}
	if got != "com.example.zipped" {
		t.Fatalf("APKPackageName = %q, want %q", got, "com.example.zipped")
	}
}
