package main

// panel_support.go —— 面板所需的 Manager 辅助方法与状态
//
// 把这些从 panel.go 拆出来，避免主文件过长。

import (
	"strconv"
	"sync"
	"sync/atomic"

	"kksresourcemanager/internal/core"
	"kksresourcemanager/internal/preview"
)

// strconvItoa 避免在多个文件重复 import strconv。
func strconvItoa(n int) string { return strconv.Itoa(n) }

// panelGenSeq 面板代数（前端用于丢弃过期响应）。
var panelGenSeq atomic.Int64

// panelGen 返回当前面板代数。
func (m *Manager) panelGen() int64 { return panelGenSeq.Add(1) }

// pickCover 返回目录的封面卡绝对路径（规格 §3.4）。
//
// 一级优先 + 深度 3 + 缓存，绝不全树遍历。
func (m *Manager) pickCover(dir string) string {
	if dir == "" {
		return ""
	}
	if m.covers == nil {
		return ""
	}
	if p, ok := m.covers.Get(dir); ok {
		return p
	}
	listDir := func(d string) (*core.DirSnapshot, error) {
		return m.snapshots.ListDir(d, func(name string, isDir bool) bool { return true })
	}
	p := core.PickCover(dir, listDir)
	m.covers.Put(dir, p)
	return p
}

// countFilesCached 统计目录下的文件数（递归，走快照缓存）。
func (m *Manager) countFilesCached(dir string) int {
	st, ok := m.fileCounts.Get(dir)
	if ok {
		return st
	}
	n, _ := countFilesIn(dir)
	m.fileCounts.Put(dir, n)
	return n
}

// countSubDirsCached 统计目录下的直接子文件夹数。
func (m *Manager) countSubDirsCached(dir string) int {
	snap, err := m.snapshots.ListDir(dir, func(name string, isDir bool) bool { return isDir })
	if err != nil {
		return 0
	}
	return len(snap.Entries)
}

// countCardsCached 统计目录下的直接卡片数。
func (m *Manager) countCardsCached(dir string) int {
	snap, err := m.snapshots.ListDir(dir, func(name string, isDir bool) bool {
		return !isDir && core.IsCardFile(name)
	})
	if err != nil {
		return 0
	}
	return len(snap.Entries)
}

// dirSizeCached 统计目录的总大小。
func (m *Manager) dirSizeCached(dir string) int64 {
	if v, ok := m.dirSizes.Get(dir); ok {
		return v
	}
	_, size := dirSize(dir)
	m.dirSizes.Put(dir, size)
	return size
}

// countCache 简单计数缓存。
type countCache struct {
	mu    sync.RWMutex
	items map[string]int
}

func newCountCache() *countCache {
	return &countCache{items: make(map[string]int)}
}

func (c *countCache) Get(k string) (int, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.items[k]
	return v, ok
}

func (c *countCache) Put(k string, v int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[k] = v
}

func (c *countCache) Invalidate(k string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, k)
}

// sizeCache 大小缓存。
type sizeCache struct {
	mu    sync.RWMutex
	items map[string]int64
}

func newSizeCache() *sizeCache {
	return &sizeCache{items: make(map[string]int64)}
}

func (c *sizeCache) Get(k string) (int64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.items[k]
	return v, ok
}

func (c *sizeCache) Put(k string, v int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[k] = v
}

func (c *sizeCache) Invalidate(k string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, k)
}

// invalidateCaches 事务提交后清理受影响目录的缓存。
func (m *Manager) invalidateCaches(dirs ...string) {
	for _, d := range dirs {
		m.snapshots.InvalidateAncestors(d)
		m.covers.InvalidateAncestors(d)
		m.fileCounts.Invalidate(d)
		m.dirSizes.Invalidate(d)
	}
}

// thumbQueueFor 确保缩略图队列存在并返回。
func (m *Manager) thumbQueueFor() *preview.Queue {
	if m.thumbs != nil {
		return m.thumbs
	}
	return nil
}
