// Package core 提供右栏面板所需的基础设施：自然序排序、目录快照、封面缓存。
//
// 设计目标（重构规格 §3.4 / §9.1）：
//
//	单次 BuildPanel 的磁盘操作 ≤ 2 次 ReadDir，命中缓存时为 0 次。
//	绝不为了渲染列表或面板去遍历整棵树。
package core

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// 自然序
// ---------------------------------------------------------------------------

// NaturalLess 按"人类阅读顺序"比较：数字段按数值比、其余按字节比、大小写不敏感。
//
// 为什么不能用 sort.Strings：
//
//	sort.Strings 会把 "22.7.10" 排在 "22.7.9" 前面 —— 字典序里 '.' (0x2E) < '9' (0x39)。
//	用户看到会认为排序坏了。
func NaturalLess(a, b string) bool {
	// 逐段比较：数字段按数值，其余按小写字节序
	ai, bi := 0, 0
	for ai < len(a) && bi < len(b) {
		ac, bc := a[ai], b[bi]
		aNum := isDigit(ac)
		bNum := isDigit(bc)

		if aNum && bNum {
			// 取出两边的数字段
			an, aEnd := readNumber(a, ai)
			bn, bEnd := readNumber(b, bi)
			if an != bn {
				return an < bn
			}
			// 数值相同则比较位数（避免前导零造成歧义）
			if aEnd-ai != bEnd-bi {
				return aEnd-ai < bEnd-bi
			}
			ai, bi = aEnd, bEnd
			continue
		}

		// 非数字段：忽略大小写比较
		al, bl := lower(ac), lower(bc)
		if al != bl {
			return al < bl
		}
		ai++
		bi++
	}

	// 公共前缀相同，短的在前
	return len(a)-ai < len(b)-bi
}

// isDigit 判断是否为 ASCII 数字。
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// lower 转小写（ASCII）。
func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

// readNumber 从位置 i 开始读一个数字段，返回数值与结束位置。
func readNumber(s string, i int) (uint64, int) {
	start := i
	var v uint64
	for i < len(s) && isDigit(s[i]) {
		// 溢出保护：位数过多时停止累加，保留已读部分
		if v < (1 << 62) {
			v = v*10 + uint64(s[i]-'0')
		}
		i++
	}
	// 去掉前导零
	allZero := true
	for k := start; k < i; k++ {
		if s[k] != '0' {
			allZero = false
			break
		}
	}
	if allZero && i > start {
		return 0, i
	}
	return v, i
}

// SortNatural 对字符串列表按自然序排序。
func SortNatural(list []string) {
	sort.SliceStable(list, func(i, j int) bool { return NaturalLess(list[i], list[j]) })
}

// ---------------------------------------------------------------------------
// 目录快照
// ---------------------------------------------------------------------------

// DirEntry 一项目录内容。
type DirEntry struct {
	Name string
	Path string
	IsDir bool
	Size int64
	ModTime int64
}

// DirSnapshot 目录内容的快照。
type DirSnapshot struct {
	Entries []DirEntry
	// DirModTime 快照时的目录 mtime
	DirModTime int64
}

// SnapshotCache 目录快照缓存（LRU 128）。
type SnapshotCache struct {
	mu       sync.RWMutex
	capacity int
	items    map[string]*snapshotEntry
	order    []string // 简单的 FIFO 淘汰
}

type snapshotEntry struct {
	snap *DirSnapshot
}

// NewSnapshotCache 创建快照缓存。
func NewSnapshotCache(capacity int) *SnapshotCache {
	if capacity <= 0 {
		capacity = 128
	}
	return &SnapshotCache{
		capacity: capacity,
		items:    make(map[string]*snapshotEntry, capacity),
	}
}

// Get 返回目录快照。
//
// 命中条件：缓存存在且目录 mtime 未变。
// 目录 mtime 变化说明内容有增删，缓存失效。
func (c *SnapshotCache) Get(dir string) (*DirSnapshot, bool) {
	c.mu.RLock()
	e, ok := c.items[dir]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	info, err := os.Stat(dir)
	if err != nil {
		c.Invalidate(dir)
		return nil, false
	}
	if info.ModTime().UnixNano() != e.snap.DirModTime {
		c.Invalidate(dir)
		return nil, false
	}
	return e.snap, true
}

// Put 写入快照。
func (c *SnapshotCache) Put(dir string, snap *DirSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.items[dir]; !exists {
		c.order = append(c.order, dir)
	}
	c.items[dir] = &snapshotEntry{snap: snap}
	// FIFO 淘汰超出的条目
	for len(c.order) > c.capacity {
		old := c.order[0]
		c.order = c.order[1:]
		delete(c.items, old)
	}
}

// Invalidate 使某个目录的快照失效。
func (c *SnapshotCache) Invalidate(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, dir)
	for i, d := range c.order {
		if d == dir {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

// InvalidateAncestors 使某目录及其所有祖先的快照失效。
//
// 事务提交后调用：目录内容变了，祖先目录的 mtime 通常也会变
// （新增/删除子项会更新父目录 mtime），但保险起见一起清。
func (c *SnapshotCache) InvalidateAncestors(dir string) {
	c.Invalidate(dir)
	for p := filepath.Dir(dir); len(p) > 2 && p != filepath.Dir(p); p = filepath.Dir(p) {
		c.Invalidate(p)
	}
}

// ---------------------------------------------------------------------------
// 目录列举
// ---------------------------------------------------------------------------

// ListDir 列举目录的一级内容（带快照缓存）。
//
// 快照命中时零磁盘操作（除一次 Stat 判断 mtime）。
func (c *SnapshotCache) ListDir(dir string, filter func(name string, isDir bool) bool) (*DirSnapshot, error) {
	if snap, ok := c.Get(dir); ok {
		return snap, nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	snap := &DirSnapshot{Entries: make([]DirEntry, 0, len(entries))}
	for _, e := range entries {
		if filter != nil && !filter(e.Name(), e.IsDir()) {
			continue
		}
		item := DirEntry{
			Name: e.Name(),
			Path: filepath.Join(dir, e.Name()),
			IsDir: e.IsDir(),
		}
		if info, err := e.Info(); err == nil {
			item.Size = info.Size()
			item.ModTime = info.ModTime().UnixNano()
		}
		snap.Entries = append(snap.Entries, item)
	}

	// 快照里按自然序排好，命中时无需再排
	sort.SliceStable(snap.Entries, func(i, j int) bool {
		return NaturalLess(snap.Entries[i].Name, snap.Entries[j].Name)
	})

	if info, err := os.Stat(dir); err == nil {
		snap.DirModTime = info.ModTime().UnixNano()
	}
	c.Put(dir, snap)
	return snap, nil
}

// ---------------------------------------------------------------------------
// 封面卡缓存
// ---------------------------------------------------------------------------

// CoverEntry 一个目录的封面卡。
type CoverEntry struct {
	// Path 封面卡的绝对路径（"" 表示无封面）
	Path string
	// DirModTime 目录 mtime（用于失效判定）
	DirModTime int64
}

// CoverCache 封面缓存（LRU 512）。
type CoverCache struct {
	mu       sync.RWMutex
	capacity int
	items    map[string]*CoverEntry
	order    []string
}

// NewCoverCache 创建封面缓存。
func NewCoverCache(capacity int) *CoverCache {
	if capacity <= 0 {
		capacity = 512
	}
	return &CoverCache{
		capacity: capacity,
		items:    make(map[string]*CoverEntry, capacity),
	}
}

// Get 读取封面缓存。
func (c *CoverCache) Get(dir string) (string, bool) {
	c.mu.RLock()
	e, ok := c.items[dir]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	info, err := os.Stat(dir)
	if err != nil {
		c.Invalidate(dir)
		return "", false
	}
	if info.ModTime().UnixNano() != e.DirModTime {
		c.Invalidate(dir)
		return "", false
	}
	return e.Path, true
}

// Put 写入封面缓存。
func (c *CoverCache) Put(dir string, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	dirMod := int64(0)
	if info, err := os.Stat(dir); err == nil {
		dirMod = info.ModTime().UnixNano()
	}
	if _, exists := c.items[dir]; !exists {
		c.order = append(c.order, dir)
	}
	c.items[dir] = &CoverEntry{Path: path, DirModTime: dirMod}
	for len(c.order) > c.capacity {
		old := c.order[0]
		c.order = c.order[1:]
		delete(c.items, old)
	}
}

// Invalidate 使封面缓存失效。
func (c *CoverCache) Invalidate(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, dir)
	for i, d := range c.order {
		if d == dir {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

// InvalidateAncestors 使某目录及祖先的封面缓存失效。
func (c *CoverCache) InvalidateAncestors(dir string) {
	c.Invalidate(dir)
	for p := filepath.Dir(dir); len(p) > 2 && p != filepath.Dir(p); p = filepath.Dir(p) {
		c.Invalidate(p)
	}
}

// ---------------------------------------------------------------------------
// 封面选取
// ---------------------------------------------------------------------------

// coverLimits 封面查找的边界（规格 §3.4）。
const (
	// coverMaxDepth BFS 最大深度
	coverMaxDepth = 3
	// coverMaxDirs BFS 最多访问的目录数
	coverMaxDirs = 64
)

// PickCover 返回目录的"封面卡"绝对路径；找不到返回 ""。
//
// 规则（按序短路，绝不无限制递归）：
//  1. 直接子文件里自然序第一张 .png → 命中即返回（最常见，1 次 ReadDir）
//  2. 否则按子目录名自然序 BFS，深度 <= 3，最多访问 64 个目录
//  3. 仍无 → 返回 ""（前端渲染无封面占位，不是错误）
func PickCover(dir string, listDir func(string) (*DirSnapshot, error)) string {
	// 第一层：直接子文件
	snap, err := listDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range snap.Entries {
		if !e.IsDir && isPNGName(e.Name) {
			return e.Path
		}
	}

	// 第二层：BFS，最多 3 层 / 64 个目录
	type qitem struct {
		path  string
		depth int
	}
	queue := make([]qitem, 0, 16)
	for _, e := range snap.Entries {
		if e.IsDir {
			queue = append(queue, qitem{e.Path, 1})
		}
	}

	visited := 0
	for len(queue) > 0 && visited < coverMaxDirs {
		cur := queue[0]
		queue = queue[1:]
		visited++

		if cur.depth > coverMaxDepth {
			continue
		}
		snap, err := listDir(cur.path)
		if err != nil {
			continue
		}
		for _, e := range snap.Entries {
			if !e.IsDir && isPNGName(e.Name) {
				return e.Path
			}
		}
		if cur.depth < coverMaxDepth {
			for _, e := range snap.Entries {
				if e.IsDir {
					queue = append(queue, qitem{e.Path, cur.depth + 1})
				}
			}
		}
	}
	return ""
}

// isPNGName 判断文件名是否为 PNG。
func isPNGName(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".png")
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

// ParseSize 把字节数格式化为易读文本。
func ParseSize(bytes int64) string {
	if bytes <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	v := float64(bytes)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return strconv.FormatInt(bytes, 10) + " B"
	}
	return strconv.FormatFloat(v, 'f', 1, 64) + " " + units[i]
}
