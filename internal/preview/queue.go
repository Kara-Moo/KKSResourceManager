// Package preview 实现缩略图的缓存与生成队列。
//
// 重构规格 §9 的核心约束（红线 N1）：
//
//	UI 请求路径**禁止同步解码 PNG**。
//	一次九宫格 = 9 张卡，每张可能 42MB。同步解码会让界面冻结 5~15 秒。
//
//	本包提供两级缓存 + 有界并发队列：
//	  - 内存 LRU（256 张）：命中立即返回，零 IO
//	  - 磁盘缓存（sha1 分片）：命中只读文件，不解码
//	  - 都未命中 → 入队返回 pending，由后台 worker 生成后批量通知前端
//
// 冷启动绝不生成缩略图：16150 张 × 30ms = 8 分钟，完全不可接受。
package preview

import (
	"crypto/sha1"
	"errors"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fingerprint 文件指纹。
type Fingerprint struct {
	Size    int64
	ModTime int64
}

// SizeClass 缩略图尺寸档。
//
// 两档必须彼此独立（键不同、磁盘文件不同）：
// 九宫格 320×400 撑到 376px 宽会发虚，单卡预览需要 720×960；
// 若共用一档，要么九宫格浪费解码，要么大图糊。
type SizeClass string

const (
	// SizeGrid 九宫格档：320×400
	SizeGrid SizeClass = "g"
	// SizeLarge 单卡大图档：720×960
	SizeLarge SizeClass = "l"
)

// ThumbKey 返回九宫格档的缓存键。
//
// key = sha1(abs + "|" + size + "|" + mtime)
//
// 包含 size 与 mtime 的意义：文件内容变了 key 就变，旧缓存自动失效，
// 无需任何显式清理逻辑。
func ThumbKey(abs string, fp Fingerprint) string {
	return ThumbKeySized(abs, fp, SizeGrid)
}

// ThumbKeySized 返回指定尺寸档的缓存键。
//
// large 档在基础键后加 "-l" 后缀 —— 同一文件的两种尺寸互不覆盖。
func ThumbKeySized(abs string, fp Fingerprint, class SizeClass) string {
	raw := abs + "|" + strconv.FormatInt(fp.Size, 10) + "|" + strconv.FormatInt(fp.ModTime, 10)
	sum := sha1.Sum([]byte(raw))
	key := hex.EncodeToString(sum[:])
	if class == SizeLarge {
		return key + "-l"
	}
	return key
}

// diskPath 返回缩略图在磁盘缓存中的路径。
//
// 布局：thumb/<sha1(abs)[:2]>/<sha1(abs+size+mtime)>.jpg
// 两级目录避免单目录文件过多。
func diskPath(diskDir, abs string, fp Fingerprint, class SizeClass) string {
	absSum := sha1.Sum([]byte(abs))
	keySum := sha1.Sum([]byte(abs + "|" + strconv.FormatInt(fp.Size, 10) + "|" + strconv.FormatInt(fp.ModTime, 10)))
	shard := hex.EncodeToString(absSum[:])[:2]
	name := hex.EncodeToString(keySum[:])
	if class == SizeLarge {
		name += "-l"
	}
	return filepath.Join(diskDir, shard, name+".jpg")
}

// ---------------------------------------------------------------------------
// 内存 LRU
// ---------------------------------------------------------------------------

type lruEntry struct {
	key   string
	value string
}

// lruCache 简单的 LRU 缓存（容量固定，淘汰最久未用）。
type lruCache struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*listNode
	order    *listNode // 最近使用在表头
}

type listNode struct {
	key   string
	value string
	prev  *listNode
	next  *listNode
}

func newLRU(capacity int) *lruCache {
	if capacity <= 0 {
		capacity = 256
	}
	return &lruCache{
		capacity: capacity,
		items:    make(map[string]*listNode, capacity),
		order:    &listNode{},
	}
}

// Get 读取并提升到最近使用。
func (c *lruCache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.items[key]
	if !ok {
		return "", false
	}
	c.moveToFront(n)
	return n.value, true
}

// Put 写入并提升到最近使用，必要时淘汰最久未用的。
func (c *lruCache) Put(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n, ok := c.items[key]; ok {
		n.value = value
		c.moveToFront(n)
		return
	}
	n := &listNode{key: key, value: value}
	c.items[key] = n
	c.pushFront(n)
	for len(c.items) > c.capacity {
		c.popBack()
	}
}

// Len 返回当前条目数。
func (c *lruCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// pushFront 把 n 插到链表最前（紧邻哨兵之后）。
//
// 哨兵 c.order 的 next 是最近使用，prev 是最久未用。
// 插入时必须同时维护 c.order.prev，否则链表尾部指针断裂，
// popBack 会取到错误节点导致淘汰循环无法终止。
func (c *lruCache) pushFront(n *listNode) {
	n.prev = c.order
	n.next = c.order.next
	if c.order.next != nil {
		c.order.next.prev = n
	} else {
		// 链表原本为空，新节点同时是尾部
		c.order.prev = n
	}
	c.order.next = n
}

// moveToFront 把 n 移到最前。
func (c *lruCache) moveToFront(n *listNode) {
	if c.order.next == n {
		return
	}
	c.unlink(n)
	c.pushFront(n)
}

// unlink 把 n 从链表中摘除。
func (c *lruCache) unlink(n *listNode) {
	if n.prev != nil {
		n.prev.next = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	}
	// n 是最后一个节点时，哨兵的 prev 需要回退
	if c.order.prev == n {
		c.order.prev = n.prev
	}
	n.prev, n.next = nil, nil
}

// popBack 淘汰最久未用的节点。
func (c *lruCache) popBack() {
	// 哨兵的 prev 是最久未用的真实节点
	last := c.order.prev
	if last == nil {
		return
	}
	c.unlink(last)
	delete(c.items, last.key)
}

// ---------------------------------------------------------------------------
// 队列
// ---------------------------------------------------------------------------

// Result 一张缩略图的生成结果。
type Result struct {
	ThumbKey string `json:"thumbKey"`
	DataURI  string `json:"dataUri"`
	Pending  bool   `json:"pending"`
}

// NotifyFunc 批量通知回调。
//
// worker 完成后按 100ms 窗口批量触发，避免每张图都发一次事件。
type NotifyFunc func(results []Result)

// Generator 缩略图生成函数（由调用方注入实际解码逻辑）。
//
// class 决定输出尺寸：SizeGrid=320×400，SizeLarge=720×960。
type Generator func(abs string, fp Fingerprint, class SizeClass) (dataURI string, err error)

// job 一个待生成的缩略图任务。
//
// 去重与排队以 key 为单位，而不是 abs ——
// 同一文件的 grid 与 large 是两件不同的工作，必须各自成队，
// 否则先排入的那档会把另一档吞掉。
type job struct {
	abs   string
	fp    Fingerprint
	class SizeClass
	key   string
}

// Queue 缩略图生成队列。
type Queue struct {
	workers int
	diskDir string
	mem     *lruCache
	gen     Generator
	notify  NotifyFunc

	mu       sync.Mutex
	hi       []string
	lo       []string
	inflight map[string]bool
	jobs     map[string]job
	closed   bool

	// wake 有新任务时唤醒 worker
	wake chan struct{}
	// done 队列关闭时通知所有 worker 退出
	done chan struct{}

	// notifyMu 保护批量通知缓冲
	notifyMu   sync.Mutex
	notifyBuf  []Result

	// errMu 保护 onErr 回调
	errMu sync.Mutex
	onErr func(abs string, err error)

	// stats 统计（用于性能埋点与测试断言）
	statsMu    sync.Mutex
	diskHits   int
	memHits    int
	misses     int
	generated  int
	failed     int
	dedupCount int
}

// SetErrorHandler 设置生成失败的回调。
//
// 队列不直接依赖日志设施，由调用方注入 —— 这样 preview 包保持无副作用，
// 同时满足「失败必须可见」的要求（不允许静默丢弃）。
func (q *Queue) SetErrorHandler(fn func(abs string, err error)) {
	q.errMu.Lock()
	q.onErr = fn
	q.errMu.Unlock()
}

// reportError 把生成失败交给回调。
func (q *Queue) reportError(abs string, err error) {
	q.errMu.Lock()
	fn := q.onErr
	q.errMu.Unlock()
	if fn != nil {
		fn(abs, err)
	}
}

// NewQueue 创建队列。
//
// workers 固定为 3：机械盘超过 3 会加剧寻道，SSD 也不建议超过 4。
func NewQueue(diskDir string, workers int, gen Generator, notify NotifyFunc) *Queue {
	if workers <= 0 {
		workers = 3
	}
	if workers > 4 {
		workers = 4
	}
	q := &Queue{
		workers:  workers,
		diskDir:  diskDir,
		mem:      newLRU(256),
		gen:      gen,
		notify:   notify,
		inflight: make(map[string]bool),
		jobs:     make(map[string]job),
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	for i := 0; i < workers; i++ {
		go q.worker()
	}
	q.startBatcher()
	return q
}

// Request 请求缩略图。
//
// 返回 (dataURI, pending)：
//   - 命中内存 → 立即返回，pending=false
//   - 命中磁盘 → 读文件返回，pending=false
//   - 都未命中 → 入队，返回 ("", true)
func (q *Queue) Request(abs string, fp Fingerprint, priority bool) (string, bool) {
	return q.RequestSized(abs, fp, SizeGrid, priority)
}

// RequestSized 请求指定尺寸档的缩略图。
//
// 两档各自走自己的缓存键与磁盘文件，互不影响。
func (q *Queue) RequestSized(abs string, fp Fingerprint, class SizeClass, priority bool) (string, bool) {
	key := ThumbKeySized(abs, fp, class)

	// 1. 内存命中
	if v, ok := q.mem.Get(key); ok {
		q.statsMu.Lock()
		q.memHits++
		q.statsMu.Unlock()
		return v, false
	}

	// 2. 磁盘命中（只读文件，不解码）
	if path := diskPath(q.diskDir, abs, fp, class); fileExists(path) {
		if data, err := os.ReadFile(path); err == nil {
			uri := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(data)
			q.mem.Put(key, uri)
			q.statsMu.Lock()
			q.diskHits++
			q.statsMu.Unlock()
			return uri, false
		}
	}

	// 3. 未命中 → 入队
	q.enqueue(abs, fp, class, key, priority)
	q.statsMu.Lock()
	q.misses++
	q.statsMu.Unlock()
	return "", true
}

// enqueue 把任务加入队列（去重）。
func (q *Queue) enqueue(abs string, fp Fingerprint, class SizeClass, key string, priority bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return
	}
	// 已在飞行中 → 不重复入队（但计入去重数，便于观察）
	if q.inflight[key] {
		q.dedupCount++
		return
	}
	// 已在队列中 → 需要时提升优先级
	for i, k := range q.hi {
		if k == key {
			if !priority {
				return
			}
			q.hi = append(q.hi[:i], q.hi[i+1:]...)
			q.hi = append([]string{key}, q.hi...)
			return
		}
	}
	for i, k := range q.lo {
		if k == key {
			if !priority {
				return
			}
			q.lo = append(q.lo[:i], q.lo[i+1:]...)
			q.hi = append([]string{key}, q.hi...)
			return
		}
	}

	q.jobs[key] = job{abs: abs, fp: fp, class: class, key: key}
	if priority {
		q.hi = append(q.hi, key)
	} else {
		q.lo = append(q.lo, key)
	}
	q.notifyWorker()
}

// notifyWorker 唤醒一个等待中的 worker。
func (q *Queue) notifyWorker() {
	select {
	case q.wake <- struct{}{}:
	default:
		// 已有 worker 被唤醒，无需重复
	}
}

// worker 后台生成循环。
func (q *Queue) worker() {
	for {
		j, ok := q.next()
		if !ok {
			// 队列暂时为空：等待新任务，而不是退出。
			// 早前版本直接 return，导致 worker 在第一次空队列检查时就死了，
			// 所有缩略图任务无人处理（pending 永远不消）。
			q.waitForTask()
			continue
		}

		uri, err := q.gen(j.abs, j.fp, j.class)

		q.mu.Lock()
		delete(q.inflight, j.key)
		q.mu.Unlock()

		if err == nil && uri != "" {
			q.mem.Put(j.key, uri)
			q.writeDisk(j, uri)
			q.statsMu.Lock()
			q.generated++
			q.statsMu.Unlock()
			q.emit(Result{ThumbKey: j.key, DataURI: uri})
			continue
		}

		// 失败：计数 + 上报。
		//
		// 早前版本这里什么都不做，失败的任务彻底消失，
		// 用户只看到格子一直是灰块，日志里也没有任何线索。
		q.statsMu.Lock()
		q.failed++
		q.statsMu.Unlock()
		if err == nil {
			err = errors.New("生成结果为空")
		}
		q.reportError(j.abs, err)
	}
}

// signal 通知 worker 有新任务。
type signal struct {
	ch chan struct{}
}

// waitForTask 等待新任务或关闭信号。
func (q *Queue) waitForTask() {
	select {
	case <-q.wake:
	case <-q.done:
	}
}

// next 取下一个任务（高优先级优先）。
func (q *Queue) next() (job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return job{}, false
	}
	if len(q.hi) > 0 {
		key := q.hi[0]
		q.hi = q.hi[1:]
		return q.popJobLocked(key)
	}
	if len(q.lo) > 0 {
		key := q.lo[0]
		q.lo = q.lo[1:]
		return q.popJobLocked(key)
	}
	return job{}, false
}

// popJobLocked 取出任务并标记为飞行中（调用方须持锁）。
func (q *Queue) popJobLocked(key string) (job, bool) {
	j, ok := q.jobs[key]
	if !ok {
		return job{}, false
	}
	delete(q.jobs, key)
	q.inflight[key] = true
	return j, true
}

// writeDisk 把缩略图写入磁盘缓存。
func (q *Queue) writeDisk(j job, dataURI string) {
	path := diskPath(q.diskDir, j.abs, j.fp, j.class)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return
	}
	payload := decodeBase64(dataURI)
	if len(payload) == 0 {
		return
	}
	// 临时文件 + rename，避免读到半截文件
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0644); err != nil {
		return
	}
	os.Rename(tmp, path)
}

// emit 把结果加入批量通知缓冲。
func (q *Queue) emit(r Result) {
	q.notifyMu.Lock()
	q.notifyBuf = append(q.notifyBuf, r)
	q.notifyMu.Unlock()
}

// startBatcher 启动 100ms 批量通知循环。
func (q *Queue) startBatcher() {
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			q.flushNotify()
		}
	}()
}

func (q *Queue) flushNotify() {
	q.notifyMu.Lock()
	if len(q.notifyBuf) == 0 {
		q.notifyMu.Unlock()
		return
	}
	buf := q.notifyBuf
	q.notifyBuf = nil
	q.notifyMu.Unlock()

	if q.notify != nil {
		q.notify(buf)
	}
}

// Close 停止队列。
func (q *Queue) Close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	q.mu.Unlock()
	close(q.done)
}

// Stats 返回统计信息（性能埋点与测试断言用）。
type Stats struct {
	MemHits   int `json:"memHits"`
	DiskHits  int `json:"diskHits"`
	Misses    int `json:"misses"`
	Generated int `json:"generated"`
	// Failed 生成失败数。
	//
	// 必须暴露出来：早前版本失败时静默丢弃，
	// 结果就是「图片不显示但不知道为什么」，排查成本极高。
	Failed     int `json:"failed"`
	DedupCount int `json:"dedupCount"`
	QueueLen   int `json:"queueLen"`
	Inflight   int `json:"inflight"`
}

// Stats 返回当前统计。
func (q *Queue) Stats() Stats {
	q.statsMu.Lock()
	s := Stats{
		MemHits:    q.memHits,
		DiskHits:   q.diskHits,
		Misses:     q.misses,
		Generated:  q.generated,
		Failed:     q.failed,
		DedupCount: q.dedupCount,
	}
	q.statsMu.Unlock()

	q.mu.Lock()
	s.QueueLen = len(q.hi) + len(q.lo)
	s.Inflight = len(q.inflight)
	q.mu.Unlock()
	return s
}

// fileExists 判断文件是否存在。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// DiskCachePath 导出九宫格档的磁盘缓存路径（供测试与外部清理使用）。
func DiskCachePath(diskDir, abs string, fp Fingerprint) string {
	return diskPath(diskDir, abs, fp, SizeGrid)
}

// DiskCachePathSized 导出指定尺寸档的磁盘缓存路径。
func DiskCachePathSized(diskDir, abs string, fp Fingerprint, class SizeClass) string {
	return diskPath(diskDir, abs, fp, class)
}

// decodeBase64 从 data URI 中提取出原始字节。
func decodeBase64(dataURI string) []byte {
	const prefix = "data:image/jpeg;base64,"
	if !strings.HasPrefix(dataURI, prefix) {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(dataURI[len(prefix):])
	if err != nil {
		return nil
	}
	return data
}
