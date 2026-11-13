package main

// zipmoddetail.go —— zipmod 占用详情（规格 §4.2 / §4.3）
//
// 这是排查「为什么这个 mod 隔离不了」的关键信息：
// 哪些卡片引用它、哪些 DLL 硬编码了它的 GUID、命中方式是 UTF-8 还是 UTF-16LE。

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"kksresourcemanager/internal/index"
)

// RequestThumb 请求一张缩略图（只查缓存，未命中返回 pending）。
func (m *Manager) RequestThumb(abs string) (key, dataURI string, pending bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.thumbRequest(abs, true)
}

// ZipmodDetailOf 返回指定 GUID 的占用详情。
func (m *Manager) ZipmodDetailOf(guid string) ZipmodDetail {
	m.mu.RLock()
	defer m.mu.RUnlock()

	detail := ZipmodDetail{GUID: guid}

	path, ok := m.ZipmodDB[guid]
	if !ok {
		return detail
	}
	detail.Path = path
	detail.Base = filepath.Base(path)
	if info, err := os.Stat(path); err == nil {
		detail.Size = info.Size()
	}

	// --- 引用它的卡片 ---
	cards := append([]string(nil), m.GlobalDepMap[guid]...)
	sort.Strings(cards)
	detail.Cards = cards
	detail.RefCards = len(cards)

	// --- 引用它的插件 ---
	pluginRels := append([]string(nil), m.PluginDepMap[guid]...)
	sort.Strings(pluginRels)
	for _, rel := range pluginRels {
		abs := filepath.Join(m.KksRoot, filepath.FromSlash(rel))
		pd := PluginDetail{
			Name: filepath.Base(rel),
			Rel:  rel,
			Abs:  abs,
		}
		// 找出该 DLL 命中的全部 GUID 与命中方式
		if entry, ok := m.pluginEntry(abs); ok {
			pd.GUIDs = entry.GUIDs
			pd.HitMode = detectHitMode(abs, guid)
		}
		detail.Plugins = append(detail.Plugins, pd)
	}
	detail.RefPlugins = len(detail.Plugins)

	return detail
}

// pluginEntry 从索引中取某个 DLL 的条目。
func (m *Manager) pluginEntry(abs string) (*index.PluginEntry, bool) {
	if m.index == nil {
		return nil, false
	}
	e, ok := m.index.Plugins[abs]
	return e, ok
}

// detectHitMode 检测 GUID 在 DLL 中的命中编码方式。
//
// 返回 "UTF-8" / "UTF-16LE" / ""（未命中）。
func detectHitMode(abs, guid string) string {
	if guid == "" {
		return ""
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return ""
	}
	if strings.Contains(string(data), guid) {
		return "UTF-8"
	}
	if containsUTF16LE(data, guid) {
		return "UTF-16LE"
	}
	return ""
}

// containsUTF16LE 判断二进制中是否含 UTF-16LE 编码的 s。
func containsUTF16LE(data []byte, s string) bool {
	if len(s) == 0 {
		return false
	}
	// 构造 UTF-16LE 模式
	needle := make([]byte, 0, len(s)*2)
	for _, r := range s {
		if r < 0x10000 {
			needle = append(needle, byte(r), byte(r>>8))
		}
	}
	if len(needle) == 0 || len(needle) > len(data) {
		return false
	}
	return bytesIndex(data, needle) >= 0
}

// bytesIndex 是 bytes.Index 的薄封装。
func bytesIndex(data, needle []byte) int {
	return bytes.Index(data, needle)
}
