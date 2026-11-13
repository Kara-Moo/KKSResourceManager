package main

// zipmodops.go —— zipmod 的两个管理操作（规格 §7）
//
//	[🩹 修复缺失 zipmod]  把被隔离/被删除但仍被引用的 zipmod 放回 mods
//	[🧹 隔离多余 zipmod]  把没有任何引用的 zipmod 移入隔离区
//
// 两者都是**两阶段**：Plan（零副作用，出清单）→ 用户确认 → Commit（事务执行）。
//
// §7.2 的重要约束：
//
//	「多余」判定必须先 derive 再算 —— 若索引是脏的会把在用的 mod 误判为多余，
//	这是最危险的单点。所以 PlanIsolateExtra 第一步固定调用 derive()。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"kksresourcemanager/internal/index"
	"kksresourcemanager/internal/tx"
)

// ---------------------------------------------------------------------------
// 修复缺失 zipmod
// ---------------------------------------------------------------------------

// MissingFixPlan 修复缺失的计划。
type MissingFixPlan struct {
	PlanID string `json:"planId"`
	// Recoverable 可从隔离区/回收站放回的
	Recoverable []RecoverableItem `json:"recoverable"`
	// Unrecoverable 本地不存在、需重新下载的
	Unrecoverable []UnrecoverableItem `json:"unrecoverable"`
	// TotalBytes 可修复部分的总大小
	TotalBytes int64 `json:"totalBytes"`
}

// RecoverableItem 一个可修复的 zipmod。
type RecoverableItem struct {
	GUID string `json:"guid"`
	// From 隔离区/回收站中的绝对路径
	From string `json:"from"`
	// To 目标绝对路径
	To string `json:"to"`
	// RefCards 引用它的卡片数
	RefCards int `json:"refCards"`
	// RefPlugins 引用它的插件数
	RefPlugins int `json:"refPlugins"`
	// Source 来源：del-zipmod | deled
	Source string `json:"source"`
	// Name 文件名
	Name string `json:"name"`
}

// UnrecoverableItem 一个无法修复的 zipmod。
type UnrecoverableItem struct {
	GUID string `json:"guid"`
	// RefCards 引用它的卡片数
	RefCards int `json:"refCards"`
	// RefPlugins 引用它的插件数
	RefPlugins int `json:"refPlugins"`
	// Sample 前 3 个引用文件
	Sample []string `json:"sample"`
}

// PlanFixMissing 生成修复缺失 zipmod 的计划（零副作用）。
//
// 「缺失」的严格定义（§7.1）：
//
//	g 被卡片或插件需要
//	∧ g 不在 GuidToZipmod
//	∧ g 不在 IsolatedGuidMap（也不在回收站里）
//
// 候选来源按优先级：DEL-zipmod → DELED/zipmod。
func (m *Manager) PlanFixMissing() MissingFixPlan {
	m.mu.Lock()
	defer m.mu.Unlock()

	plan := MissingFixPlan{PlanID: fmt.Sprintf("fix-%d", nowMS())}

	// 先派生，确保索引是最新的
	m.rebuildDepIndex()

	// 收集候选：隔离区 + 回收站
	candidates := m.collectIsolatedZipmods()

	// 找出所有被需要的 GUID
	needed := make(map[string]bool)
	for guid := range m.GlobalDepMap {
		needed[guid] = true
	}
	for guid := range m.PluginDepMap {
		needed[guid] = true
	}

	// 已有的（不需要修复）
	have := make(map[string]bool, len(m.ZipmodDB))
	for guid := range m.ZipmodDB {
		have[guid] = true
	}

	// 可修复的
	recoverableByGUID := make(map[string]RecoverableItem)
	for _, c := range candidates {
		if c.guid == "" || have[c.guid] || !needed[c.guid] {
			continue
		}
		if _, dup := recoverableByGUID[c.guid]; dup {
			continue
		}
		recoverableByGUID[c.guid] = RecoverableItem{
			GUID:       c.guid,
			From:       c.path,
			To:         m.targetPathForGUID(c.guid, c.rel),
			RefCards:   len(m.GlobalDepMap[c.guid]),
			RefPlugins: len(m.PluginDepMap[c.guid]),
			Source:     c.source,
			Name:       filepath.Base(c.path),
		}
	}

	for _, item := range recoverableByGUID {
		plan.Recoverable = append(plan.Recoverable, item)
	}
	sort.Slice(plan.Recoverable, func(i, j int) bool {
		return plan.Recoverable[i].GUID < plan.Recoverable[j].GUID
	})
	for _, item := range plan.Recoverable {
		if info, err := os.Stat(item.From); err == nil {
			plan.TotalBytes += info.Size()
		}
	}

	// 无法修复的：被需要但本地任何地方都没有
	for guid := range needed {
		if have[guid] {
			continue
		}
		if _, ok := recoverableByGUID[guid]; ok {
			continue
		}
		item := UnrecoverableItem{
			GUID:       guid,
			RefCards:   len(m.GlobalDepMap[guid]),
			RefPlugins: len(m.PluginDepMap[guid]),
		}
		for _, ref := range m.GlobalDepMap[guid] {
			if len(item.Sample) >= 3 {
				break
			}
			item.Sample = append(item.Sample, filepath.Base(ref))
		}
		plan.Unrecoverable = append(plan.Unrecoverable, item)
	}
	sort.Slice(plan.Unrecoverable, func(i, j int) bool {
		return plan.Unrecoverable[i].GUID < plan.Unrecoverable[j].GUID
	})

	return plan
}

// isolatedCandidate 一个隔离区/回收站中的 zipmod 候选。
type isolatedCandidate struct {
	guid   string
	path   string
	rel    string
	source string // del-zipmod | deled
}

// collectIsolatedZipmods 收集隔离区与回收站中的 zipmod。
//
// **惰性扫描**：只在需要时调用（进入 zipmod 模式或执行修复时），
// 避免每次启动都额外扫盘。
func (m *Manager) collectIsolatedZipmods() []isolatedCandidate {
	var result []isolatedCandidate

	// 来源 1：DEL-zipmod
	delZipmod := m.DelZipmodDir()
	_ = filepath.Walk(delZipmod, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), zipmodExt) {
			return nil
		}
		guid := ""
		if entry, ok := m.indexedZipmod(path); ok {
			guid = entry.GUID
		} else {
			guid = m.zipmodGUID(path)
		}
		rel := filepath.Base(path)
		if entry, ok := m.indexedZipmod(path); ok && entry.Rel != "" {
			rel = entry.Rel
		}
		result = append(result, isolatedCandidate{
			guid:   guid,
			path:   path,
			rel:    filepath.ToSlash(rel),
			source: "del-zipmod",
		})
		return nil
	})

	// 来源 2：DELED/zipmod
	trashZipmod := filepath.Join(m.DelCardsRoot, "DELED", "zipmod")
	_ = filepath.Walk(trashZipmod, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), zipmodExt) {
			return nil
		}
		guid := m.zipmodGUID(path)
		rel := filepath.Base(path)
		// 回收站元数据里记了原路径
		if meta, ok := m.trashMetaFor(path); ok && meta.OriginalAbs != "" {
			if r, err := filepath.Rel(m.ModsDir(), meta.OriginalAbs); err == nil {
				rel = filepath.ToSlash(r)
			}
		}
		result = append(result, isolatedCandidate{
			guid:   guid,
			path:   path,
			rel:    rel,
			source: "deled",
		})
		return nil
	})

	return result
}

// indexedZipmod 从索引里取 zipmod 条目。
func (m *Manager) indexedZipmod(path string) (*index.ZipmodEntry, bool) {
	if m.index == nil {
		return nil, false
	}
	if e, ok := m.index.Zipmods[path]; ok {
		return e, true
	}
	if e, ok := m.index.IsolatedZipmods[path]; ok {
		return e, true
	}
	return nil, false
}

// targetPathForGUID 计算某个 GUID 放回 mods 后的目标路径。
//
// 优先用 OriginRel（隔离时记录的原相对路径）；未知则放 mods 根。
func (m *Manager) targetPathForGUID(guid, fallbackRel string) string {
	rel := fallbackRel
	if m.index != nil {
		if origin, ok := m.index.OriginRel[guid]; ok && origin != "" {
			rel = origin
		}
	}
	if rel == "" {
		rel = guid + zipmodExt
	}
	return filepath.Join(m.ModsDir(), filepath.FromSlash(rel))
}

// CommitFixMissing 执行修复：把选中的 zipmod 放回 mods。
func (m *Manager) CommitFixMissing(guids []string) (restored int, skipped int, failed []string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	failed = make([]string, 0)
	plan := m.planFixMissingLocked()
	if len(plan.Recoverable) == 0 {
		return 0, 0, failed, fmt.Errorf("没有可修复的 zipmod")
	}

	// 只处理用户勾选的
	wanted := make(map[string]bool, len(guids))
	for _, g := range guids {
		wanted[g] = true
	}

	txn, err := m.beginTx("fix-missing", fmt.Sprintf("修复 %d 个缺失 zipmod", len(guids)))
	if err != nil {
		return 0, 0, failed, err
	}

	for _, item := range plan.Recoverable {
		if len(wanted) > 0 && !wanted[item.GUID] {
			continue
		}
		// 目标已存在同 GUID → 跳过（已存在，无需修复）
		if _, statErr := os.Stat(item.To); statErr == nil {
			skipped++
			failed = append(failed, fmt.Sprintf("%s：目标已存在，跳过", item.Name))
			continue
		}
		// 纵深防御：再跑一次三道防线
		if ok, reason := m.checkZipmodMovable(item.GUID, nil); !ok {
			skipped++
			failed = append(failed, fmt.Sprintf("%s：%s", item.Name, reason))
			continue
		}
		if err := os.MkdirAll(filepath.Dir(item.To), 0755); err != nil {
			failed = append(failed, fmt.Sprintf("%s：创建目录失败 %v", item.Name, err))
			continue
		}
		if err := txn.Move(item.From, item.To); err != nil {
			failed = append(failed, fmt.Sprintf("%s：%v", item.Name, err))
			continue
		}
		restored++
	}

	if len(failed) > 0 && restored == 0 {
		m.rollbackTx(txn)
		return 0, skipped, failed, fmt.Errorf("修复失败")
	}
	if err := m.commitTx(txn); err != nil {
		return restored, skipped, failed, err
	}
	m.rebuildDepIndex()
	m.invalidateCaches()
	return restored, skipped, failed, nil
}

// planFixMissingLocked 是 PlanFixMissing 的持锁版本。
func (m *Manager) planFixMissingLocked() MissingFixPlan {
	plan := MissingFixPlan{PlanID: fmt.Sprintf("fix-%d", nowMS())}
	m.rebuildDepIndex()

	candidates := m.collectIsolatedZipmods()
	needed := make(map[string]bool)
	for guid := range m.GlobalDepMap {
		needed[guid] = true
	}
	for guid := range m.PluginDepMap {
		needed[guid] = true
	}
	have := make(map[string]bool, len(m.ZipmodDB))
	for guid := range m.ZipmodDB {
		have[guid] = true
	}

	seen := make(map[string]bool)
	for _, c := range candidates {
		if c.guid == "" || have[c.guid] || !needed[c.guid] || seen[c.guid] {
			continue
		}
		seen[c.guid] = true
		plan.Recoverable = append(plan.Recoverable, RecoverableItem{
			GUID:       c.guid,
			From:       c.path,
			To:         m.targetPathForGUID(c.guid, c.rel),
			RefCards:   len(m.GlobalDepMap[c.guid]),
			RefPlugins: len(m.PluginDepMap[c.guid]),
			Source:     c.source,
			Name:       filepath.Base(c.path),
		})
	}
	sort.Slice(plan.Recoverable, func(i, j int) bool {
		return plan.Recoverable[i].GUID < plan.Recoverable[j].GUID
	})
	for _, item := range plan.Recoverable {
		if info, err := os.Stat(item.From); err == nil {
			plan.TotalBytes += info.Size()
		}
	}

	for guid := range needed {
		if have[guid] || seen[guid] {
			continue
		}
		item := UnrecoverableItem{
			GUID:       guid,
			RefCards:   len(m.GlobalDepMap[guid]),
			RefPlugins: len(m.PluginDepMap[guid]),
		}
		for _, ref := range m.GlobalDepMap[guid] {
			if len(item.Sample) >= 3 {
				break
			}
			item.Sample = append(item.Sample, filepath.Base(ref))
		}
		plan.Unrecoverable = append(plan.Unrecoverable, item)
	}
	sort.Slice(plan.Unrecoverable, func(i, j int) bool {
		return plan.Unrecoverable[i].GUID < plan.Unrecoverable[j].GUID
	})
	return plan
}

// ---------------------------------------------------------------------------
// 隔离多余 zipmod
// ---------------------------------------------------------------------------

// ExtraPlan 隔离多余 zipmod 的计划。
type ExtraPlan struct {
	PlanID string `json:"planId"`
	// Candidates 可隔离的候选（默认全选，按大小降序）
	Candidates []ExtraCandidate `json:"candidates"`
	// Skipped 永不自动隔离的（列出原因）
	Skipped []ExtraSkipped `json:"skipped"`
	// TotalBytes 总大小
	TotalBytes int64 `json:"totalBytes"`
}

// ExtraCandidate 一个可隔离的候选。
type ExtraCandidate struct {
	GUID string `json:"guid"`
	// Path 当前绝对路径
	Path string `json:"path"`
	// Base 文件名
	Base string `json:"base"`
	// Size 大小
	Size int64 `json:"size"`
	// Note 备注："" | "重复副本"
	Note string `json:"note"`
}

// ExtraSkipped 一个被跳过的 zipmod（附原因）。
type ExtraSkipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// PlanIsolateExtra 生成隔离多余 zipmod 的计划（零副作用）。
//
// 「多余」的严格定义（三个条件全部满足）：
//
//	g ∈ GuidToZipmod
//	∧ len(GlobalDepMap[g]) == 0     无卡片引用
//	∧ len(PluginDepMap[g])  == 0     无 DLL 硬编码引用
//	∧ len(modDependents(g)) == 0     无其它 zipmod 依赖它
func (m *Manager) PlanIsolateExtra() ExtraPlan {
	m.mu.Lock()
	defer m.mu.Unlock()

	plan := ExtraPlan{PlanID: fmt.Sprintf("extra-%d", nowMS())}

	// 第一步固定 derive —— 索引脏会把在用的 mod 误判为多余（最危险的单点）
	m.rebuildDepIndex()

	// 同 GUID 多文件：保留字典序最小者为主副本，其余标「重复副本」
	byGUID := make(map[string][]string)
	for guid, path := range m.ZipmodDB {
		byGUID[guid] = append(byGUID[guid], path)
	}
	for guid := range byGUID {
		sort.Strings(byGUID[guid])
	}

	for guid, paths := range byGUID {
		for i, path := range paths {
			size := int64(0)
			if info, err := os.Stat(path); err == nil {
				size = info.Size()
			}

			// 永不自动隔离的情况
			if guid == "" {
				plan.Skipped = append(plan.Skipped, ExtraSkipped{
					Path:   path,
					Reason: "无法识别 GUID，需人工确认",
				})
				continue
			}
			if i > 0 {
				// 同 GUID 的重复副本（保留字典序最小者）
				plan.Candidates = append(plan.Candidates, ExtraCandidate{
					GUID: guid, Path: path, Base: filepath.Base(path),
					Size: size, Note: "重复副本",
				})
				continue
			}
			if len(m.GlobalDepMap[guid]) > 0 {
				plan.Skipped = append(plan.Skipped, ExtraSkipped{
					Path:   path,
					Reason: fmt.Sprintf("仍被 %d 个资源引用", len(m.GlobalDepMap[guid])),
				})
				continue
			}
			if len(m.PluginDepMap[guid]) > 0 {
				plan.Skipped = append(plan.Skipped, ExtraSkipped{
					Path: path,
					Reason: fmt.Sprintf("[插件保护] 被 %d 个插件硬编码引用",
						len(m.PluginDepMap[guid])),
				})
				continue
			}
			if deps := m.modDependents(guid); len(deps) > 0 {
				plan.Skipped = append(plan.Skipped, ExtraSkipped{
					Path:   path,
					Reason: "依赖链不完整，隔离后更难恢复",
				})
				continue
			}
			plan.Candidates = append(plan.Candidates, ExtraCandidate{
				GUID: guid, Path: path, Base: filepath.Base(path), Size: size,
			})
		}
	}

	// 按大小降序
	sort.Slice(plan.Candidates, func(i, j int) bool {
		return plan.Candidates[i].Size > plan.Candidates[j].Size
	})
	for _, c := range plan.Candidates {
		plan.TotalBytes += c.Size
	}
	sort.Slice(plan.Skipped, func(i, j int) bool {
		return plan.Skipped[i].Path < plan.Skipped[j].Path
	})

	return plan
}

// CommitIsolateExtra 执行隔离：把选中的多余 zipmod 移入 DEL-zipmod。
func (m *Manager) CommitIsolateExtra(guids []string) (isolated int, bytes int64, skipped int, failed []string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	failed = make([]string, 0)
	plan := m.planIsolateExtraLocked()

	wanted := make(map[string]bool, len(guids))
	for _, g := range guids {
		wanted[g] = true
	}

	txn, err := m.beginTx("isolate-extra", fmt.Sprintf("隔离 %d 个多余 zipmod", len(guids)))
	if err != nil {
		return 0, 0, 0, failed, err
	}

	delDir := m.DelZipmodDir()
	for _, c := range plan.Candidates {
		if len(wanted) > 0 && !wanted[c.GUID] {
			continue
		}
		// 纵深防御：再跑一次三道防线
		// （计划生成到用户确认之间可能已启用某张卡）
		if ok, reason := m.checkZipmodMovable(c.GUID, nil); !ok {
			skipped++
			failed = append(failed, fmt.Sprintf("%s：%s", c.Base, reason))
			continue
		}
		dst := filepath.Join(delDir, c.Base)
		if _, statErr := os.Stat(dst); statErr == nil {
			// 重名 → 加 __2
			ext := filepath.Ext(c.Base)
			stem := strings.TrimSuffix(c.Base, ext)
			for i := 2; i <= 99; i++ {
				cand := filepath.Join(delDir, fmt.Sprintf("%s__%d%s", stem, i, ext))
				if _, e := os.Stat(cand); os.IsNotExist(e) {
					dst = cand
					break
				}
			}
		}
		if err := txn.Move(c.Path, dst); err != nil {
			failed = append(failed, fmt.Sprintf("%s：%v", c.Base, err))
			continue
		}
		isolated++
		bytes += c.Size
	}

	if len(failed) > 0 && isolated == 0 {
		m.rollbackTx(txn)
		return 0, 0, skipped, failed, fmt.Errorf("隔离失败")
	}
	if err := m.commitTx(txn); err != nil {
		return isolated, bytes, skipped, failed, err
	}
	m.rebuildDepIndex()
	m.invalidateCaches()
	return isolated, bytes, skipped, failed, nil
}

// planIsolateExtraLocked 是 PlanIsolateExtra 的持锁版本。
func (m *Manager) planIsolateExtraLocked() ExtraPlan {
	plan := ExtraPlan{PlanID: fmt.Sprintf("extra-%d", nowMS())}
	m.rebuildDepIndex()

	byGUID := make(map[string][]string)
	for guid, path := range m.ZipmodDB {
		byGUID[guid] = append(byGUID[guid], path)
	}
	for guid := range byGUID {
		sort.Strings(byGUID[guid])
	}

	for guid, paths := range byGUID {
		for i, path := range paths {
			size := int64(0)
			if info, err := os.Stat(path); err == nil {
				size = info.Size()
			}
			if guid == "" {
				plan.Skipped = append(plan.Skipped, ExtraSkipped{Path: path, Reason: "无法识别 GUID，需人工确认"})
				continue
			}
			if i > 0 {
				plan.Candidates = append(plan.Candidates, ExtraCandidate{
					GUID: guid, Path: path, Base: filepath.Base(path), Size: size, Note: "重复副本",
				})
				continue
			}
			if len(m.GlobalDepMap[guid]) > 0 {
				plan.Skipped = append(plan.Skipped, ExtraSkipped{
					Path: path, Reason: fmt.Sprintf("仍被 %d 个资源引用", len(m.GlobalDepMap[guid])),
				})
				continue
			}
			if len(m.PluginDepMap[guid]) > 0 {
				plan.Skipped = append(plan.Skipped, ExtraSkipped{
					Path: path, Reason: fmt.Sprintf("[插件保护] 被 %d 个插件硬编码引用", len(m.PluginDepMap[guid])),
				})
				continue
			}
			if deps := m.modDependents(guid); len(deps) > 0 {
				plan.Skipped = append(plan.Skipped, ExtraSkipped{
					Path: path, Reason: "依赖链不完整，隔离后更难恢复",
				})
				continue
			}
			plan.Candidates = append(plan.Candidates, ExtraCandidate{
				GUID: guid, Path: path, Base: filepath.Base(path), Size: size,
			})
		}
	}
	sort.Slice(plan.Candidates, func(i, j int) bool {
		return plan.Candidates[i].Size > plan.Candidates[j].Size
	})
	for _, c := range plan.Candidates {
		plan.TotalBytes += c.Size
	}
	return plan
}

// trashMetaFor 返回回收站中某文件的元数据。
func (m *Manager) trashMetaFor(abs string) (trashMeta, bool) {
	store, err := m.trashStore()
	if err != nil {
		return trashMeta{}, false
	}
	all, err := store.Load()
	if err != nil {
		return trashMeta{}, false
	}
	for rel, meta := range all {
		joined := filepath.Join(store.Dir(), filepath.FromSlash(rel))
		if strings.EqualFold(joined, abs) {
			return trashMeta{OriginalAbs: meta.OriginalAbs, Spec: meta.Spec}, true
		}
	}
	return trashMeta{}, false
}

// trashMeta 是回收站元数据的轻量视图。
type trashMeta struct {
	OriginalAbs string
	Spec        string
}

var _ = tx.MaxSteps
