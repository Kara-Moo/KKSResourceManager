package index

import (
	"crypto/sha1"
	"encoding/hex"
	"sort"
)

// Derived 是由正向数据派生出的反向索引集合。
//
// 这些数据**不落盘**，每次启动现算（§5.4）。
// 派生耗时与条目数成正比，几万条目约 5~20ms。
type Derived struct {
	// GlobalDepMap GUID -> 引用它的资源文件绝对路径列表
	GlobalDepMap map[string][]string
	// GuidToZipmod GUID -> zipmod 绝对路径
	GuidToZipmod map[string]string
	// MissingGUIDs 被资源引用但本地没有对应 zipmod 的 GUID
	MissingGUIDs []string
	// ExtraZipmods 本地存在但没有任何资源引用的 zipmod GUID
	ExtraZipmods []string
}

// Derive 由正向数据派生全部反向索引。
//
// 这是全项目**唯一**构造 GlobalDepMap 的地方。
// 禁止在任何其它地方增量 append 反向图 ——
// 那会导致同一文件被移动 N 次后积累 N 条重复引用，
// 使 zipmod 隔离判定失真（历史缺陷 §3.6）。
func Derive(idx *IndexData) *Derived {
	d := &Derived{
		GlobalDepMap: make(map[string][]string, len(idx.Cards)),
		GuidToZipmod: make(map[string]string, len(idx.Zipmods)),
	}

	// --- 反向依赖图：GUID -> 引用它的卡片 ---
	for path, card := range idx.Cards {
		for _, guid := range card.GUIDs {
			d.GlobalDepMap[guid] = append(d.GlobalDepMap[guid], path)
		}
	}
	// 排序，保证日志与 UI 顺序稳定，便于测试断言
	for guid := range d.GlobalDepMap {
		sort.Strings(d.GlobalDepMap[guid])
	}

	// --- GUID -> zipmod ---
	// 同 GUID 多文件时保留字典序最小的路径，行为稳定可预期。
	zipmodPaths := make([]string, 0, len(idx.Zipmods))
	for path := range idx.Zipmods {
		zipmodPaths = append(zipmodPaths, path)
	}
	sort.Strings(zipmodPaths)
	for _, path := range zipmodPaths {
		guid := idx.Zipmods[path].GUID
		if guid == "" {
			continue
		}
		if _, dup := d.GuidToZipmod[guid]; !dup {
			d.GuidToZipmod[guid] = path
		}
	}

	// --- 缺失的 mod：被引用但本地没有 ---
	for guid := range d.GlobalDepMap {
		if _, ok := d.GuidToZipmod[guid]; !ok {
			d.MissingGUIDs = append(d.MissingGUIDs, guid)
		}
	}
	sort.Strings(d.MissingGUIDs)

	// --- 多余的 mod：本地有但没人引用 ---
	for guid := range d.GuidToZipmod {
		if len(d.GlobalDepMap[guid]) == 0 {
			d.ExtraZipmods = append(d.ExtraZipmods, guid)
		}
	}
	sort.Strings(d.ExtraZipmods)

	return d
}

// HashGUIDSet 计算 GUID 集合的稳定哈希。
//
// 用于判断「zipmod 集合是否变化」—— 若变化则 DLL 需重新扫描，
// 否则可复用上次的匹配结果（§5.8）。
func HashGUIDSet(guids []string) string {
	sorted := make([]string, len(guids))
	copy(sorted, guids)
	sort.Strings(sorted)

	h := sha1.New()
	for _, g := range sorted {
		h.Write([]byte(g))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// GUIDSetOf 收集索引中所有 zipmod 的 GUID 并去重。
func GUIDSetOf(idx *IndexData) []string {
	seen := make(map[string]struct{}, len(idx.Zipmods))
	result := make([]string, 0, len(idx.Zipmods))
	for _, z := range idx.Zipmods {
		if z.GUID == "" {
			continue
		}
		if _, ok := seen[z.GUID]; ok {
			continue
		}
		seen[z.GUID] = struct{}{}
		result = append(result, z.GUID)
	}
	sort.Strings(result)
	return result
}
