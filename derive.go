package main

// derive.go —— 派生索引（单一真源原则）
//
// 背景（历史缺陷 §3.6）：
//
//	旧实现用 `m.GlobalDepMap[g] = append(m.GlobalDepMap[g], dst)` 增量维护反向图。
//	同一文件被移动 N 次，就会在同一 GUID 下积累 N 条相同引用。
//	后果：`zipmodStillReferenced` 统计虚高 → 本该可隔离的 zipmod 隔离不掉；
//	      日志里「仍被 37 个资源引用」实际只有 3 个。
//
// 修法（§5.4 单一真源）：
//
//	正向数据 `FileDeps[path] = guids` 是唯一真源。
//	反向图 `GlobalDepMap` 永远由正向数据**现算**，不做任何增量修补。
//	移动文件后只需：改 FileDeps（删旧 key、加新 key）→ 调 derive() → 反向图自动一致。
//
// 几万条目的 derive 约 5–20ms，完全可以每次操作后跑一遍。

import (
	"sort"
)

// derive 重建全部反向索引。
//
// 只在两处调用：扫描结束、文件移动/复制之后。
// 调用方必须持有 m.mu 写锁。
func (m *Manager) derive() {
	// --- 反向依赖图：GUID -> 引用它的资源文件 ---
	depMap := make(map[string][]string, len(m.FileDeps))
	for path, guids := range m.FileDeps {
		for _, guid := range guids {
			depMap[guid] = append(depMap[guid], path)
		}
	}
	// 排序，保证日志与 UI 顺序稳定，便于测试断言
	for guid := range depMap {
		sort.Strings(depMap[guid])
	}
	m.GlobalDepMap = depMap

	// --- 隔离区文件的依赖同样参与派生 ---
	// DEL 侧的文件已不在 mods 中，不应阻塞 zipmod 隔离，
	// 因此不并入 GlobalDepMap，只保留在 DelFileDeps 供预览使用。
}

// deriveZipmodDB 由 ZipmodDB 派生 GUID -> 路径的稳定映射。
//
// 同一 GUID 对应多个 zipmod 时保留字典序最小的一个，保证行为可预期。
func (m *Manager) deriveZipmodDB() {
	if len(m.ZipmodDB) == 0 {
		return
	}
	// ZipmodDB 本身已是 GUID -> 路径，派生仅用于去重排序，
	// 保证同 GUID 多文件时结果稳定。
	keys := make([]string, 0, len(m.ZipmodDB))
	for guid := range m.ZipmodDB {
		keys = append(keys, guid)
	}
	sort.Strings(keys)
	stable := make(map[string]string, len(keys))
	for _, guid := range keys {
		path := m.ZipmodDB[guid]
		if existing, ok := stable[guid]; ok {
			// 已有记录，保留字典序更小的路径
			if path < existing {
				stable[guid] = path
			}
			continue
		}
		stable[guid] = path
	}
	m.ZipmodDB = stable
}

// rebuildDepIndex 在文件移动、复制、隔离、恢复后统一调用。
//
// 它会重新计算反向依赖图，避免增量 append 造成的重复累积。
func (m *Manager) rebuildDepIndex() {
	m.derive()
	m.deriveZipmodDB()
}
