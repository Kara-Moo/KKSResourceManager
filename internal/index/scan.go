package index

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"kksresourcemanager/internal/core"
)

// ProgressFunc 扫描进度回调。
//
// v2.4 起索引只有一个阶段，phase 恒为 "resource"。
// 签名保持不变（前端契约不动）。
type ProgressFunc func(phase string, done, total int)

// ScanStats 一次扫描的统计。
type ScanStats struct {
	// Reused 复用旧索引的条目数（零 IO）
	Reused int `json:"reused"`
	// Parsed 本次需要更新条目的文件数（只做 stat，不解析内容）
	Parsed int `json:"parsed"`
	// Removed 从索引移除的条目数
	Removed int `json:"removed"`
	// Failed 读元数据失败的文件数
	Failed int `json:"failed"`
	// Cards 索引中的卡片总数
	Cards int `json:"cards"`
	// DurationMs 耗时（毫秒）
	DurationMs int64 `json:"durationMs"`
}

// Scanner 负责执行增量扫描。
type Scanner struct {
	// Root KKS 根目录
	Root string
	// ResourceRoots 资源类型 -> 源目录
	ResourceRoots map[string]string
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
// 这是「冷启动 ≤5s / 热启动 ≤300ms」的关键。
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

// collectMetadata 遍历 root，返回匹配文件 -> 指纹。
//
// 只做 WalkDir + Info，不打开文件内容。
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

// isCardFile 判断是否为受支持的角色卡文件（PNG / JPG / JPEG）。
func isCardFile(path string, d fs.DirEntry) bool {
	return core.IsCardFile(path)
}

// Scan 执行增量扫描（单阶段）。
//
// 流程：
//  1. 遍历 6 个资源目录，只对卡片文件做 stat，收集指纹；
//  2. 与旧索引 diff：指纹未变 → 复用（不打开文件）；
//  3. 新增/变更 → 写入条目（Kind + Rel + 指纹）；
//  4. 旧条目中新路径不存在 → 删除。
//
// 全程不解析 PNG、不读 DLL、不解析 zipmod。
func (s *Scanner) Scan(ctx context.Context, old *IndexData, progress ProgressFunc) (*IndexData, ScanStats, error) {
	start := time.Now()
	stats := ScanStats{}

	if err := ctx.Err(); err != nil {
		return nil, stats, err
	}

	var idx *IndexData
	if old != nil && old.IsCompatible(s.Root) {
		idx = old
	} else {
		idx = NewIndexData(s.Root)
	}

	// --- 1. 元数据遍历（只 stat，不打开文件） ---
	meta := make(map[string]Fingerprint)
	for _, root := range s.ResourceRoots {
		for p, fp := range collectMetadata(root, isCardFile) {
			meta[p] = fp
		}
	}

	// --- 2. diff ---
	oldMeta := fingerprintsOfCards(idx.Cards)
	d := s.diffMetadata(oldMeta, meta)

	stats.Reused = len(d.same)
	stats.Parsed = len(d.added) + len(d.changed)
	stats.Removed = len(d.removed)

	total := len(meta)
	if progress != nil {
		progress("resource", 0, total)
	}

	// --- 3. 新增 / 变更条目（不解析内容） ---
	for _, p := range d.added {
		kind, rel := s.classifyCard(p)
		idx.Cards[p] = &CardEntry{Fingerprint: meta[p], Kind: kind, Rel: rel}
	}
	for _, p := range d.changed {
		kind, rel := s.classifyCard(p)
		idx.Cards[p] = &CardEntry{Fingerprint: meta[p], Kind: kind, Rel: rel}
	}

	// --- 4. 清理已删除 ---
	for _, p := range d.removed {
		delete(idx.Cards, p)
	}

	idx.ScannedAt = time.Now().UnixNano()
	stats.Cards = len(idx.Cards)
	stats.DurationMs = time.Since(start).Milliseconds()

	if progress != nil {
		progress("resource", total, total)
	}
	return idx, stats, nil
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

// fingerprintsOfCards 提取卡片指纹表。
func fingerprintsOfCards(m map[string]*CardEntry) map[string]Fingerprint {
	result := make(map[string]Fingerprint, len(m))
	for path, e := range m {
		result[path] = e.Fingerprint
	}
	return result
}
