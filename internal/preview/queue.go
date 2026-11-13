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

// ThumbKey 返回缩略图缓存键。
//
// key = sha1(abs + "|" + size + "|" + mtime)
//
// 包含 size 与 mtime 的意义：文件内容变了 key 就变，旧缓存自动失效，
// 无需任何显式清理逻辑。
func ThumbKey(abs string, fp Fingerprint) string {
	raw := abs + "|" + strconv.FormatInt(fp.Size, 10) + "|" + strconv.FormatInt(fp.ModTime, 10)
	sum := sha1.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// diskPath 返回缩略图在磁盘缓存中的路径。
//
// 布局：thumb/<sha1(abs)[:2]>/<sha1(abs+size+mtime)>.jpg
// 两级目录避免单目录文件过多。
func diskPath(diskDir, abs string, fp Fingerprint) string {
	absSum := sha1.Sum([]byte(abs))
	keySum := sha1.Sum([]byte(abs + "|" + strconv.FormatInt(fp.Size, 10) + "|" + strconv.FormatInt(fp.ModTime, 10)))
	shard := hex.EncodeToString(absSum[:])[:2]
	return filepath.Join(diskDir, shard, hex.EncodeToString(keySum[:])+".jpg")
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
type Generator func(abs string, fp Fingerprint) (dataURI string, err error)

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
	pending  map[string]Fingerprint
	closed   bool

	// wake 有新任务时唤醒 worker
	wake chan struct{}
	// done 队列关闭时通知所有 worker 退出
	done chan struct{}

	// notifyMu 保护批量通知缓冲
	notifyMu   sync.Mutex
	notifyBuf  []Result

	// stats 统计（用于性能埋点与测试断言）
	statsMu    sync.Mutex
	diskHits   int
	memHits    int
	misses     int
	generated  int
	dedupCount int
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
		pending:  make(map[string]Fingerprint),
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
	key := ThumbKey(abs, fp)

	// 1. 内存命中
	if v, ok := q.mem.Get(key); ok {
		q.statsMu.Lock()
		q.memHits++
		q.statsMu.Unlock()
		return v, false
	}

	// 2. 磁盘命中（只读文件，不解码）
	if path := diskPath(q.diskDir, abs, fp); fileExists(path) {
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
	q.enqueue(abs, fp, key, priority)
	q.statsMu.Lock()
	q.misses++
	q.statsMu.Unlock()
	return "", true
}

// enqueue 把任务加入队列（去重）。
func (q *Queue) enqueue(abs string, fp Fingerprint, key string, priority bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return
	}
	// 已在飞行中 → 不重复入队
	if q.inflight[key] {
		q.dedupCount++
		return
	}
	// 已在队列中 → 提升优先级
	for i, p := range q.hi {
		if p == abs {
			if !priority {
				return
			}
			// 从 hi 移到前面
			q.hi = append(q.hi[:i], q.hi[i+1:]...)
			q.hi = append([]string{abs}, q.hi...)
			return
		}
	}
	for i, p := range q.lo {
		if p == abs {
			if !priority {
				return
			}
			q.lo = append(q.lo[:i], q.lo[i+1:]...)
			q.hi = append([]string{abs}, q.hi...)
			return
		}
	}

	q.pending[abs] = fp
	if priority {
		q.hi = append(q.hi, abs)
	} else {
		q.lo = append(q.lo, abs)
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
		abs, ok := q.next()
		if !ok {
			// 队列暂时为空：等待新任务，而不是退出。
			// 早前版本直接 return，导致 worker 在第一次空队列检查时就死了，
			// 所有缩略图任务无人处理（pending 永远不消）。
			q.waitForTask()
			continue
		}

		q.mu.Lock()
		fp := q.pending[abs]
		key := ThumbKey(abs, fp)
		delete(q.pending, abs)
		q.inflight[key] = true
		q.mu.Unlock()

		uri, err := q.gen(abs, fp)

		q.mu.Lock()
		delete(q.inflight, key)
		q.mu.Unlock()

		if err == nil && uri != "" {
			q.mem.Put(key, uri)
			q.writeDisk(abs, fp, uri)
			q.statsMu.Lock()
			q.generated++
			q.statsMu.Unlock()
			q.emit(Result{ThumbKey: key, DataURI: uri})
		}
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
func (q *Queue) next() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if q.closed {
			return "", false
		}
		if len(q.hi) > 0 {
			abs := q.hi[0]
			q.hi = q.hi[1:]
			return abs, true
		}
		if len(q.lo) > 0 {
			abs := q.lo[0]
			q.lo = q.lo[1:]
			return abs, true
		}
		return "", false
	}
}

// writeDisk 把缩略图写入磁盘缓存。
func (q *Queue) writeDisk(abs string, fp Fingerprint, dataURI string) {
	path := diskPath(q.diskDir, abs, fp)
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
	MemHits    int `json:"memHits"`
	DiskHits   int `json:"diskHits"`
	Misses     int `json:"misses"`
	Generated  int `json:"generated"`
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

// DiskCachePath 导出磁盘缓存路径计算（供测试与外部清理使用）。
func DiskCachePath(diskDir, abs string, fp Fingerprint) string {
	return diskPath(diskDir, abs, fp)
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
