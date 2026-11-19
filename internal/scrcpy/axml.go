// axml.go 用纯 Go 解析 APK 内的 AndroidManifest.xml（AXML 二进制格式），
// 提取 <manifest package="..."> 中的应用包名。
//
// 背景：批量卸载 APK 时需要知道目标应用的包名（pm uninstall 需要包名而非
// 文件名）。APK 本质是 ZIP，其中 AndroidManifest.xml 是 AXML（Android 二进制
// XML）而非文本。这里不依赖外部 aapt 工具，直接解析 AXML 的字符串池，定位
// manifest 节点的 package 属性值。
//
// AXML 结构（小端序，4 字节对齐的 chunk）：
//   - 头部：magic=0x00080003（RES_XML_TYPE），后面跟字符串池 chunk、资源 map
//     chunk、以及 XML 树节点 chunk。
//   - 字符串池 chunk：magic=0x001C0001（RES_STRING_POOL_TYPE），包含 UTF-16
//     或 UTF-8 字符串数组，索引从 0 起。Android 的字符串池索引有 8 字节
//     对齐偏移（现代 AAPT2 产物），解析时需按 flags 判断。
//   - 节点 chunk：magic=0x00100102（RES_XML_START_ELEMENT_TYPE），manifest 是
//     根元素，其 attribute 里 name 为 "package" 的 value 指向字符串池索引。
//
// 解析策略：不完整还原 XML 树，只需定位第一个 START_ELEMENT（即 manifest）
// 的 package 属性。字符串池必在节点之前，因此顺序解析即可。
package scrcpy

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// axml 相关常量（小端序读取）。
//
// 注意：AXML 的 chunk 头是 type(uint16) + headerSize(uint16) + size(uint32)。
// type 字段只占低 16 位，其完整值的高 16 位编码的是该 chunk 的固定
// headerSize（如 0x0003 表示 RES_XML_TYPE 且 headerSize=8）。这里只取
// type 的低 16 位用于比较，headerSize 从 chunk 头的第 3~4 字节单独读取。
const (
	chunkTypeXML          = 0x0003     // RES_XML_TYPE：AXML 文件头（完整值 0x00080003）
	chunkTypeStringPool   = 0x0001     // RES_STRING_POOL_TYPE（完整值 0x001C0001）
	chunkTypeResourceMap  = 0x0180     // RES_XML_RESOURCE_MAP_TYPE（完整值 0x00080180）
	chunkTypeStartElement = 0x0102     // RES_XML_START_ELEMENT_TYPE（完整值 0x00100102）
	chunkTypeEndElement   = 0x0103     // RES_XML_END_ELEMENT_TYPE（完整值 0x00100103）
	chunkTypeStartNS      = 0x0100     // RES_XML_START_NAMESPACE_TYPE（完整值 0x00100100）
	chunkTypeEndNS        = 0x0101     // RES_XML_END_NAMESPACE_TYPE（完整值 0x00100101）
	stringPoolUTF8Flag    = 0x00000100 // UTF-8 字符串池标志
)

// APKPackageName 从 APK 文件内容中解析出应用包名。
// data 是 APK 的完整字节（通常由上传接口读入内存，APK 一般几十 MB 以内）。
func APKPackageName(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("apk 不是有效的 zip: %w", err)
	}
	var manifest *zip.File
	for _, f := range zr.File {
		if f.Name == "AndroidManifest.xml" {
			manifest = f
			break
		}
	}
	if manifest == nil {
		return "", fmt.Errorf("apk 内未找到 AndroidManifest.xml")
	}
	rc, err := manifest.Open()
	if err != nil {
		return "", fmt.Errorf("打开 AndroidManifest.xml 失败: %w", err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return "", fmt.Errorf("读取 AndroidManifest.xml 失败: %w", err)
	}
	return parseManifestPackage(raw)
}

// parseManifestPackage 从 AXML 字节流中提取 manifest 的 package 属性。
func parseManifestPackage(data []byte) (string, error) {
	if len(data) < 8 {
		return "", fmt.Errorf("AndroidManifest.xml 过短")
	}
	// chunk 头：type(uint16) + headerSize(uint16) + size(uint32)。
	magic := binary.LittleEndian.Uint16(data[0:2])
	if magic != chunkTypeXML {
		return "", fmt.Errorf("不是 AXML 格式（type=0x%04x）", magic)
	}
	headerSize := int(binary.LittleEndian.Uint16(data[2:4]))
	offset := headerSize
	// 第一个字符串池：读取其字符串列表。
	var pool *stringPool
	for offset+8 <= len(data) {
		chunkType := binary.LittleEndian.Uint16(data[offset : offset+2])
		chunkSize := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		if chunkSize < 8 || offset+chunkSize > len(data) {
			return "", fmt.Errorf("AXML chunk 越界 at %d", offset)
		}
		body := data[offset : offset+chunkSize]
		switch chunkType {
		case chunkTypeStringPool:
			p, err := parseStringPool(body)
			if err != nil {
				return "", err
			}
			pool = p
		case chunkTypeStartElement:
			if pool == nil {
				return "", fmt.Errorf("遇到元素节点但尚无字符串池")
			}
			// 第一个 START_ELEMENT 即 manifest，解析其 package 属性。
			pkg, err := manifestPackage(body, pool)
			if err != nil {
				return "", err
			}
			if pkg == "" {
				return "", fmt.Errorf("manifest 节点缺少 package 属性")
			}
			return pkg, nil
		}
		offset += chunkSize
	}
	return "", fmt.Errorf("AXML 中未找到 manifest 元素")
}

// stringPool 保存解析出的字符串池。strings[i] 对应索引 i 的字符串。
type stringPool struct {
	strings []string
}

// parseStringPool 解析 RES_STRING_POOL_TYPE chunk，返回字符串数组。
//
// chunk 布局（从 chunk 头之后）：
//
//	header(8) + stringCount(4) + styleCount(4) + flags(4) + stringsStart(4)
//	+ stylesStart(4) + offsets[stringCount](4 each) + stringData
//
// offsets 是相对 stringsStart 的字节偏移。flags 含 UTF8_FLAG 时字符串为
// 变长编码（前 1~2 字节为长度），否则为 UTF-16。
func parseStringPool(chunk []byte) (*stringPool, error) {
	if len(chunk) < 28 {
		return nil, fmt.Errorf("字符串池 chunk 过短")
	}
	count := int(binary.LittleEndian.Uint32(chunk[8:12]))
	flags := binary.LittleEndian.Uint32(chunk[16:20])
	stringsStart := int(binary.LittleEndian.Uint32(chunk[20:24]))
	if count < 0 || count > 1<<20 {
		return nil, fmt.Errorf("字符串池数量异常: %d", count)
	}
	offsetsStart := 28
	pool := &stringPool{strings: make([]string, count)}
	for i := 0; i < count; i++ {
		offPos := offsetsStart + i*4
		if offPos+4 > len(chunk) {
			return nil, fmt.Errorf("字符串池 offset 越界")
		}
		off := int(binary.LittleEndian.Uint32(chunk[offPos : offPos+4]))
		strPos := stringsStart + off
		if strPos >= len(chunk) {
			return nil, fmt.Errorf("字符串池数据越界")
		}
		s, err := decodePoolString(chunk[strPos:], flags)
		if err != nil {
			return nil, fmt.Errorf("解码字符串池第 %d 项失败: %w", i, err)
		}
		pool.strings[i] = s
	}
	return pool, nil
}

// decodePoolString 解码字符串池中单个字符串（变长长度前缀 + 内容 + NUL 结尾）。
func decodePoolString(data []byte, flags uint32) (string, error) {
	if len(data) < 2 {
		return "", fmt.Errorf("字符串过短")
	}
	// UTF-8 模式下长度是变长：若首字节高位置位，长度为 (b1&0x7F)<<8 | b2。
	var byteLen, charLen int
	if flags&stringPoolUTF8Flag != 0 {
		n := 0
		if data[0]&0x80 != 0 {
			n = int(data[0]&0x7f)<<8 | int(data[1])
			byteLen = 2
		} else {
			n = int(data[0])
			byteLen = 1
		}
		charLen = n // UTF-8 时 byteLen 与 charLen 通常同值
		_ = charLen
	} else {
		// UTF-16：长度以 2 字节为单位。
		byteLen = 2
		if len(data) >= 4 {
			charLen = int(binary.LittleEndian.Uint16(data[0:2]))
		}
	}
	body := data[byteLen:]
	var out string
	if flags&stringPoolUTF8Flag != 0 {
		// UTF-8 字符串以 NUL 结尾。
		end := bytes.IndexByte(body, 0)
		if end < 0 {
			end = len(body)
		}
		out = string(body[:end])
	} else {
		// UTF-16LE 字符串，长度以 2 字节计，以 0x0000 结尾。
		maxUnits := len(body) / 2
		if maxUnits > charLen {
			maxUnits = charLen
		}
		units := make([]uint16, 0, maxUnits)
		for i := 0; i < maxUnits; i++ {
			u := binary.LittleEndian.Uint16(body[i*2 : i*2+2])
			if u == 0 {
				break
			}
			units = append(units, u)
		}
		out = utf16ToString(units)
	}
	return out, nil
}

// manifestPackage 解析 START_ELEMENT chunk，提取 name 为 "package" 的属性值。
//
// START_ELEMENT chunk 布局（相对 chunk 头）：
//
//	header(8) + line(4) + comment(4) + ns(4) + name(4)
//	+ attributeStart(2) + attributeSize(2) + attributeCount(2)
//	+ idIndex(2) + classIndex(2) + styleIndex(2)
//	+ attributes[count](attributeSize each)
//
// 因此 attributeStart 位于 chunk[24:26]，attributeSize 在 chunk[26:28]，
// attributeCount 在 chunk[28:30]。
//
// 每个 attribute 结构（20 字节 = 5 个 uint32）：
//
//	ns(4) name(4) rawValue(4) typedValue{size(2) res0(1) dataType(1) data(4)}
//
// name/rawValue 是字符串池索引；typedValue 的 dataType 为 TYPE_STRING(3) 时
// data 也是字符串池索引。
func manifestPackage(chunk []byte, pool *stringPool) (string, error) {
	if len(chunk) < 36 {
		return "", fmt.Errorf("元素节点 chunk 过短")
	}
	// attribute 数据固定在 chunk 头偏移 36 处：8 字节 chunk 头 + lineNumber(4)
	// + comment(4) + ns(4) + name(4) + attributeStart(2) + attributeSize(2)
	// + attributeCount(2) + idIndex(2) + classIndex(2) + styleIndex(2) = 36。
	// 不能读 attributeStart 字段：部分 AAPT2/三方打包产物（如 uni-app/DCloud）
	// 会把该字段写成非标准的相对偏移（实测 20），直接用它会导致 attribute
	// 全部错位、找不到 package 属性。
	attrStart := 36
	attrSize := int(binary.LittleEndian.Uint16(chunk[26:28]))
	attrCount := int(binary.LittleEndian.Uint16(chunk[28:30]))
	if attrSize < 20 {
		attrSize = 20 // 属性结构最小 5 个 4 字节字段
	}
	for i := 0; i < attrCount; i++ {
		attrOff := attrStart + i*attrSize
		if attrOff+20 > len(chunk) {
			return "", fmt.Errorf("属性越界")
		}
		attr := chunk[attrOff:]
		// ns(4) name(4) rawValue(4) ...
		nameIdx := int(binary.LittleEndian.Uint32(attr[4:8]))
		if nameIdx < 0 || nameIdx >= len(pool.strings) {
			continue
		}
		if pool.strings[nameIdx] != "package" {
			continue
		}
		// 值可能是字符串（rawValue 直接是字符串索引），也可能是 typedValue
		// 指向字符串。优先用 rawValue；若 rawValue 为 -1 则取 typedValue 的
		// 字符串索引。
		rawIdx := int32(binary.LittleEndian.Uint32(attr[8:12]))
		if rawIdx >= 0 && int(rawIdx) < len(pool.strings) {
			return pool.strings[rawIdx], nil
		}
		// typedValue 位于 attr 偏移 12 之后：size(2) res0(1) dataType(1) data(4)。
		// 若 dataType == 3（TYPE_STRING），data 即字符串索引。
		if len(attr) >= 20 {
			dataType := attr[15]
			if dataType == 0x03 {
				dataIdx := int(binary.LittleEndian.Uint32(attr[16:20]))
				if dataIdx >= 0 && dataIdx < len(pool.strings) {
					return pool.strings[dataIdx], nil
				}
			}
		}
	}
	return "", nil
}

// utf16ToString 把 UTF-16 码元序列转成 Go 字符串（处理基本多文种平面及代理对）。
func utf16ToString(units []uint16) string {
	runes := make([]rune, 0, len(units))
	for i := 0; i < len(units); i++ {
		u := units[i]
		if u >= 0xD800 && u <= 0xDBFF && i+1 < len(units) {
			lo := units[i+1]
			if lo >= 0xDC00 && lo <= 0xDFFF {
				r := 0x10000 + (rune(u)-0xD800)<<10 + (rune(lo) - 0xDC00)
				runes = append(runes, r)
				i++
				continue
			}
		}
		runes = append(runes, rune(u))
	}
	return string(runes)
}
