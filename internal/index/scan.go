package index

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// ProgressFunc 扫描进度回调。
//
// phase 取值 "zipmod" / "resource" / "plugin"，用于前端区分阶段。
type ProgressFunc func(phase string, done, total int)

// ScanStats 一次扫描的统计。
type ScanStats struct {
	// Reused 复用旧索引的条目数（零 IO）
	Reused int `json:"reused"`
	// Parsed 本次实际解析的文件数
	Parsed int `json:"parsed"`
	// Removed 从索引中移除的条目数
	Removed int `json:"removed"`
	// Failed 解析失败的文件数
	Failed int `json:"failed"`
	// Zipmods 索引中的 zipmod 总数
	Zipmods int `json:"zipmods"`
	// Cards 索引中的卡片总数
	Cards int `json:"cards"`
	// Plugins 索引中的 DLL 总数
	Plugins int `json:"plugins"`
	// DurationMs 耗时（毫秒）
	DurationMs int64 `json:"durationMs"`
	// MissingGUIDs 被引用但本地缺失的 GUID
	MissingGUIDs []string `json:"missingGuids,omitempty"`
}

// Scanner 负责执行增量扫描。
type Scanner struct {
	// Root KKS 根目录
	Root string
	// ModsDir mods 目录
	ModsDir string
	// ResourceRoots 资源类型 -> 源目录
	ResourceRoots map[string]string
	// PluginRoots DLL 扫描目录
	PluginRoots []string

	// ParseZipmod 解析 zipmod 的 GUID 与依赖
	ParseZipmod func(path string) (guid string, deps []string, err error)
	// ParseCard 解析卡片依赖的 GUID
	ParseCard func(path string) (guids []string, err error)
	// MatchPlugin 匹配 DLL 中硬编码的 GUID
	MatchPlugin func(path string, guids []string) (hits []string, err error)
}

// diffResult 一次 diff 的结果。
type diffResult struct {
	added   []string
	changed []string
	removed []string
	same    []string
}

// diffMetadata 对比旧索引与磁盘现状。
//
// 只读元数据（size + mtime），**不打开任何文件内容**。
// 这是「二次启动 ≤ 2 秒」的关键。
func (s *Scanner) diffMetadata(old map[string]Fingerprint, current map[string]Fingerprint) diffResult {
	var r diffResult
	for path, fp := range current {
		oldFP, ok := old[path]
		if !ok {
			r.added = append(r.added, path)
			continue
		}
		// 判定 changed 的条件是 size 不同 || mtime 不同，两者全同才算 same。
		// Windows 上 FAT/exFAT 的 mtime 精度是 2 秒，因此不能只比 mtime。
		if !oldFP.Equal(fp) {
			r.changed = append(r.changed, path)
			continue
		}
		r.same = append(r.same, path)
	}
	for path := range old {
		if _, ok := current[path]; !ok {
			r.removed = append(r.removed, path)
		}
	}
	sort.Strings(r.added)
	sort.Strings(r.changed)
	sort.Strings(r.removed)
	sort.Strings(r.same)
	return r
}

// collectMetadata 遍历目录收集文件元数据。
//
// 只用 WalkDir + d.Info()，不打开文件内容。
func collectMetadata(root string, match func(path string, d fs.DirEntry) bool) map[string]Fingerprint {
	result := make(map[string]Fingerprint)
	if root == "" {
		return result
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !match(path, d) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		result[path] = FingerprintOf(info.Size(), info.ModTime())
		return nil
	})
	return result
}

func isZipmodFile(path string, d fs.DirEntry) bool {
	return strings.EqualFold(filepath.Ext(path), ".zipmod")
}

func isPNGFile(path string, d fs.DirEntry) bool {
	return strings.EqualFold(filepath.Ext(path), ".png")
}

func isDLLFile(path string, d fs.DirEntry) bool {
	return strings.EqualFold(filepath.Ext(path), ".dll")
}

// Scan 执行增量扫描。
//
// 流程（严格按规格 §5.3）：
//  1. 加载旧索引
//  2. 一次元数据遍历，收集三组待对比集合
//  3. diff：added / changed / removed / same
//  4. 并行解析 added + changed
//  5. 清理 removed
//  6. 派生反向索引
//  7. 原子保存
func (s *Scanner) Scan(ctx context.Context, old *IndexData, progress ProgressFunc) (*IndexData, *Derived, ScanStats, error) {
	start := time.Now()
	stats := ScanStats{}

	// --- 1. 准备旧索引 ---
	var idx *IndexData
	if old != nil && old.IsCompatible(s.Root) {
		idx = old
	} else {
		idx = NewIndexData(s.Root)
	}

	// --- 2. 元数据遍历 ---
	zipmodMeta := collectMetadata(s.ModsDir, isZipmodFile)
	cardMeta := make(map[string]Fingerprint)
	for _, root := range s.ResourceRoots {
		for p, fp := range collectMetadata(root, isPNGFile) {
			cardMeta[p] = fp
		}
	}
	pluginMeta := make(map[string]Fingerprint)
	for _, root := range s.PluginRoots {
		for p, fp := range collectMetadata(root, isDLLFile) {
			pluginMeta[p] = fp
		}
	}

	// --- 3. diff ---
	oldZipmodMeta := fingerprintsOfZipmods(idx.Zipmods)
	oldCardMeta := fingerprintsOfCards(idx.Cards)
	oldPluginMeta := fingerprintsOfPlugins(idx.Plugins)

	zd := s.diffMetadata(oldZipmodMeta, zipmodMeta)
	cd := s.diffMetadata(oldCardMeta, cardMeta)
	pd := s.diffMetadata(oldPluginMeta, pluginMeta)

	stats.Reused = len(zd.same) + len(cd.same) + len(pd.same)

	// --- 4. 并行解析 ---
	// zipmod
	needZipmod := append(append([]string{}, zd.added...), zd.changed...)
	if err := s.parseZipmods(ctx, idx, needZipmod, progress); err != nil {
		return nil, nil, stats, err
	}
	stats.Parsed += len(needZipmod)

	// 卡片
	needCard := append(append([]string{}, cd.added...), cd.changed...)
	if err := s.parseCards(ctx, idx, needCard, progress); err != nil {
		return nil, nil, stats, err
	}
	stats.Parsed += len(needCard)

	// --- 5. 清理 removed ---
	for _, p := range zd.removed {
		delete(idx.Zipmods, p)
	}
	for _, p := range cd.removed {
		delete(idx.Cards, p)
	}
	for _, p := range pd.removed {
		delete(idx.Plugins, p)
	}
	stats.Removed = len(zd.removed) + len(cd.removed) + len(pd.removed)

	// --- 6. 派生 ---
	derived := Derive(idx)
	stats.MissingGUIDs = derived.MissingGUIDs

	// --- 7. 保存 ---
	idx.ScannedAt = time.Now().UnixNano()
	idx.GuidSetHash = HashGUIDSet(GUIDSetOf(idx))

	stats.Zipmods = len(idx.Zipmods)
	stats.Cards = len(idx.Cards)
	stats.Plugins = len(idx.Plugins)
	stats.DurationMs = time.Since(start).Milliseconds()

	return idx, derived, stats, nil
}

// parseZipmods 并行解析 zipmod。
func (s *Scanner) parseZipmods(ctx context.Context, idx *IndexData, paths []string, progress ProgressFunc) error {
	if len(paths) == 0 {
		return nil
	}
	workers := scanWorkers(len(paths))
	type result struct {
		path string
		guid string
		deps []string
		err  error
	}
	out := make(chan result, len(paths))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for _, p := range paths {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				out <- result{path: path, err: ctx.Err()}
				return
			}
			defer func() { <-sem }()

			guid, deps, err := s.ParseZipmod(path)
			out <- result{path: path, guid: guid, deps: deps, err: err}
		}(p)
	}
	go func() { wg.Wait(); close(out) }()

	done := 0
	for r := range out {
		done++
		if progress != nil {
			progress("zipmod", done, len(paths))
		}
		if r.err != nil {
			continue
		}
		fp, err := fingerprintOf(r.path)
		if err != nil {
			continue
		}
		idx.Zipmods[r.path] = &ZipmodEntry{
			Fingerprint: fp,
			GUID:        r.guid,
			Deps:        r.deps,
		}
	}
	return ctx.Err()
}

// parseCards 并行解析卡片。
func (s *Scanner) parseCards(ctx context.Context, idx *IndexData, paths []string, progress ProgressFunc) error {
	if len(paths) == 0 {
		return nil
	}
	workers := scanWorkers(len(paths))
	type result struct {
		path  string
		kind  string
		rel   string
		guids []string
		err   error
	}
	out := make(chan result, len(paths))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for _, p := range paths {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				out <- result{path: path, err: ctx.Err()}
				return
			}
			defer func() { <-sem }()

			guids, err := s.ParseCard(path)
			kind, rel := s.classifyCard(path)
			out <- result{path: path, kind: kind, rel: rel, guids: guids, err: err}
		}(p)
	}
	go func() { wg.Wait(); close(out) }()

	done := 0
	for r := range out {
		done++
		if progress != nil {
			progress("resource", done, len(paths))
		}
		fp, err := fingerprintOf(r.path)
		if err != nil {
			continue
		}
		entry := &CardEntry{
			Fingerprint: fp,
			Kind:        r.kind,
			Rel:         r.rel,
			GUIDs:       r.guids,
		}
		if entry.GUIDs == nil {
			entry.GUIDs = []string{}
		}
		if r.err != nil {
			entry.ParseErr = r.err.Error()
		}
		idx.Cards[r.path] = entry
	}
	return ctx.Err()
}

// classifyCard 判定卡片所属类型与相对路径。
func (s *Scanner) classifyCard(path string) (kind, rel string) {
	for k, root := range s.ResourceRoots {
		if r, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(r, "..") {
			return k, filepath.ToSlash(r)
		}
	}
	return "", filepath.Base(path)
}

// scanWorkers 计算并行 worker 数。
//
// 磁盘密集型任务，超过 8 个 worker 无收益且伤机械盘。
func scanWorkers(n int) int {
	w := runtime.NumCPU()
	if w > 8 {
		w = 8
	}
	if w < 1 {
		w = 1
	}
	if n < w {
		w = n
	}
	if w < 1 {
		w = 1
	}
	return w
}

// fingerprintOf 取单个文件的指纹。
func fingerprintOf(path string) (Fingerprint, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Fingerprint{}, err
	}
	return FingerprintOf(info.Size(), info.ModTime()), nil
}

func fingerprintsOfZipmods(m map[string]*ZipmodEntry) map[string]Fingerprint {
	result := make(map[string]Fingerprint, len(m))
	for path, e := range m {
		result[path] = e.Fingerprint
	}
	return result
}

func fingerprintsOfCards(m map[string]*CardEntry) map[string]Fingerprint {
	result := make(map[string]Fingerprint, len(m))
	for path, e := range m {
		result[path] = e.Fingerprint
	}
	return result
}

func fingerprintsOfPlugins(m map[string]*PluginEntry) map[string]Fingerprint {
	result := make(map[string]Fingerprint, len(m))
	for path, e := range m {
		result[path] = e.Fingerprint
	}
	return result
}

// PluginMatches 重新扫描插件（DLL）的 GUID 匹配。
//
// 复用指纹缓存：DLL 未变且 GUID 集未变时不读文件内容。
func (s *Scanner) PluginMatches(ctx context.Context, idx *IndexData, guidSet []string, progress ProgressFunc) error {
	all := make(map[string]Fingerprint)
	for _, root := range s.PluginRoots {
		for p, fp := range collectMetadata(root, isDLLFile) {
			all[p] = fp
		}
	}
	old := fingerprintsOfPlugins(idx.Plugins)
	d := s.diffMetadata(old, all)

	setHash := HashGUIDSet(guidSet)
	need := append(append([]string{}, d.added...), d.changed...)
	// GUID 集合变化时，所有 DLL 都需要重扫
	guidSetChanged := idx.GuidSetHash != setHash
	if guidSetChanged {
		need = make([]string, 0, len(all))
		for p := range all {
			need = append(need, p)
		}
		sort.Strings(need)
	}

	workers := scanWorkers(len(need))
	type result struct {
		path string
		hits []string
		err  error
	}
	out := make(chan result, len(need))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for _, p := range need {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				out <- result{path: path, err: ctx.Err()}
				return
			}
			defer func() { <-sem }()

			hits, err := s.MatchPlugin(path, guidSet)
			out <- result{path: path, hits: hits, err: err}
		}(p)
	}
	go func() { wg.Wait(); close(out) }()

	done := 0
	for r := range out {
		done++
		if progress != nil {
			progress("plugin", done, len(need))
		}
		if r.err != nil {
			continue
		}
		fp, err := fingerprintOf(r.path)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(s.Root, r.path)
		idx.Plugins[r.path] = &PluginEntry{
			Fingerprint: fp,
			Rel:         filepath.ToSlash(rel),
			GUIDs:       r.hits,
		}
	}

	// 清理已删除的 DLL
	for _, p := range d.removed {
		delete(idx.Plugins, p)
	}

	// 重建 PluginDepMap
	depMap := make(map[string][]string, 256)
	for _, e := range idx.Plugins {
		for _, g := range e.GUIDs {
			depMap[g] = append(depMap[g], e.Rel)
		}
	}
	for g := range depMap {
		sort.Strings(depMap[g])
	}
	idx.PluginDepMap = depMap

	return nil
}

// String 返回扫描统计的可读摘要（用于日志）。
func (st ScanStats) String() string {
	return fmt.Sprintf("增量扫描：复用 %d，解析 %d，移除 %d，耗时 %dms",
		st.Reused, st.Parsed, st.Removed, st.DurationMs)
}
