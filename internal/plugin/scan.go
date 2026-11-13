// Package plugin 实现 BepInEx 插件（DLL）对 zipmod GUID 的硬编码依赖扫描。
//
// 为什么需要（重构规格 §8.1）：
//
// 有些 BepInEx 插件在代码里硬编码了 zipmod 的 GUID 字符串。
// 把该 zipmod 移走，游戏不会报「缺 mod」，而是插件直接抛异常/崩溃 ——
// 比缺卡更难排查。所以必须有第二道防线。
package plugin

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"unicode/utf16"
)

// MinGUIDLength 参与匹配的 GUID 最短长度。
//
// KKS 的 GUID 普遍较长（如 com.DeathWeasel.AccessoryHairBack），
// 过短的字符串会产生大量误判。
const MinGUIDLength = 8

// MaxFileSize 单个 DLL 超过此大小则跳过（避免读入超大文件）。
const MaxFileSize = 128 << 20 // 128MB

// MatchGUIDsInFile 在文件中查找出现的 GUID。
//
// 同时匹配 UTF-8 与 UTF-16LE 两种编码形态
// （部分插件资源以 UTF-16 存储）。
func MatchGUIDsInFile(path string, guids []string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() == 0 {
		return nil, nil
	}
	if info.Size() > MaxFileSize {
		return nil, fmt.Errorf("文件过大（%.1f MB），已跳过扫描", float64(info.Size())/(1024*1024))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return MatchGUIDs(data, guids), nil
}

// MatchGUIDs 返回 data 中出现的 GUID 列表。
//
// 性能说明：GUID 数量约 3000，bytes.Contains 内部走汇编优化的 IndexByte，
// 实测吞吐约 10GB/s 量级。3000 × 200MB ≈ 600GB ≈ 40~60 秒，
// 但只在冷启动发生一次，之后按指纹缓存不再重复。
//
// 若实测冷启动超过 20 秒，应切换为 Aho–Corasick（单趟扫描）。
// 当前先用简单方案，用日志打印实际耗时，超阈值再优化（避免过度设计）。
func MatchGUIDs(data []byte, guids []string) []string {
	hits := make([]string, 0, 8)
	for _, g := range guids {
		if len(g) < MinGUIDLength {
			continue
		}
		if bytes.Contains(data, []byte(g)) || bytes.Contains(data, utf16le(g)) {
			hits = append(hits, g)
		}
	}
	return hits
}

// utf16le 把字符串编码为 UTF-16LE（不带 BOM）。
func utf16le(s string) []byte {
	u := utf16.Encode([]rune(s))
	buf := make([]byte, len(u)*2)
	for i, c := range u {
		binary.LittleEndian.PutUint16(buf[i*2:], c)
	}
	return buf
}
