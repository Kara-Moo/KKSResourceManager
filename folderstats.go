package main

// folderstats.go —— 文件夹统计的一次性聚合
//
// 重构规格 §5.6 指出的性能缺陷：
//
//	旧实现 buildTree 对**每个**文件夹节点调用 computeFolderStats（内部 dirSize 递归），
//	8000 文件 / 500 目录的树等于遍历 500 次全树 —— O(n²) IO，界面卡死。
//
// 修法：一次后序遍历，得出每个目录的统计。
//	结果同时供左/右列表与文件夹预览使用，避免重复计算。

import (
	"os"
	"path/filepath"
	"sort"
)

// dirStat 单个目录的统计。
type dirStat struct {
	// DirectFolders 直接子文件夹数
	DirectFolders int
	// DirectCards 直接子文件数
	DirectCards int
	// RecursiveFiles 递归文件总数
	RecursiveFiles int
	// RecursiveSize 递归总字节数
	RecursiveSize int64
}

// statsAggregator 一次遍历收集所有目录的统计。
type statsAggregator struct {
	stats map[string]*dirStat
	// subfolders 目录 -> 其直接子文件夹名（按名排序）
	subfolders map[string][]string
}

// newStatsAggregator 对 root 做一次后序遍历，收集整棵树的统计。
//
// 只读一级目录项 + 递归累加，不解析任何文件内容。
func newStatsAggregator(root string) *statsAggregator {
	a := &statsAggregator{
		stats:      make(map[string]*dirStat, 256),
		subfolders: make(map[string][]string, 256),
	}
	if root == "" {
		return a
	}
	a.walk(root)
	return a
}

// walk 后序遍历：先处理子目录，再累加到父目录。
func (a *statsAggregator) walk(dir string) *dirStat {
	st := &dirStat{}
	a.stats[dir] = st

	entries, err := os.ReadDir(dir)
	if err != nil {
		return st
	}
	for _, e := range entries {
		child := filepath.Join(dir, e.Name())
		if e.IsDir() {
			st.DirectFolders++
			a.subfolders[dir] = append(a.subfolders[dir], e.Name())
			childStat := a.walk(child)
			st.RecursiveFiles += childStat.RecursiveFiles
			st.RecursiveSize += childStat.RecursiveSize
			continue
		}
		st.DirectCards++
		st.RecursiveFiles++
		if info, err := e.Info(); err == nil {
			st.RecursiveSize += info.Size()
		}
	}
	sort.Strings(a.subfolders[dir])
	return st
}

// get 返回目录的统计；目录不在聚合范围内时返回零值。
func (a *statsAggregator) get(dir string) *dirStat {
	if st, ok := a.stats[dir]; ok {
		return st
	}
	return &dirStat{}
}

// toFolderStats 把内部统计转换为对外 DTO。
func (a *statsAggregator) toFolderStats(dir string) *FolderStats {
	st := a.get(dir)
	fs := &FolderStats{
		SubfolderCount: st.DirectFolders,
		CardCount:      st.DirectCards,
		TotalFiles:     st.RecursiveFiles,
		TotalSize:      st.RecursiveSize,
	}
	// 前 3 个子文件夹用于预览，其余记入 remaining
	names := a.subfolders[dir]
	if len(names) > 3 {
		fs.PreviewFolders = make([]FolderPreview, 0, 3)
		for _, n := range names[:3] {
			child := filepath.Join(dir, n)
			fs.PreviewFolders = append(fs.PreviewFolders, FolderPreview{
				Name:  n,
				Path:  child,
				Count: a.get(child).RecursiveFiles,
			})
		}
		fs.RemainingFolders = append([]string(nil), names[3:]...)
	} else {
		fs.PreviewFolders = make([]FolderPreview, 0, len(names))
		for _, n := range names {
			child := filepath.Join(dir, n)
			fs.PreviewFolders = append(fs.PreviewFolders, FolderPreview{
				Name:  n,
				Path:  child,
				Count: a.get(child).RecursiveFiles,
			})
		}
	}
	return fs
}
