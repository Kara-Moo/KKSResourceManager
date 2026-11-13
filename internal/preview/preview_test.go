package preview

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestThumbKeyStable 验证 thumbKey 的稳定性与区分性。
func TestThumbKeyStable(t *testing.T) {
	fp := Fingerprint{Size: 100, ModTime: 12345}
	k1 := ThumbKey("/a/b.png", fp)
	k2 := ThumbKey("/a/b.png", fp)
	if k1 != k2 {
		t.Error("相同输入应产生相同 key")
	}
	// 路径不同 → key 不同
	if ThumbKey("/a/c.png", fp) == k1 {
		t.Error("不同路径应产生不同 key")
	}
	// size 不同 → key 不同（文件被修改后缓存自动失效）
	if ThumbKey("/a/b.png", Fingerprint{Size: 101, ModTime: 12345}) == k1 {
		t.Error("size 不同应产生不同 key")
	}
	// mtime 不同 → key 不同
	if ThumbKey("/a/b.png", Fingerprint{Size: 100, ModTime: 12346}) == k1 {
		t.Error("mtime 不同应产生不同 key")
	}
}

// TestLRUEviction 验证 LRU 淘汰最久未用的条目。
func TestLRUEviction(t *testing.T) {
	c := newLRU(2)
	c.Put("a", "1")
	c.Put("b", "2")
	c.Put("c", "3")

	if c.Len() != 2 {
		t.Errorf("容量应为 2，实际 %d", c.Len())
	}
	if _, ok := c.Get("a"); ok {
		t.Error("最久未用的 a 应被淘汰")
	}
	if _, ok := c.Get("c"); !ok {
		t.Error("最新的 c 应存在")
	}
}

// TestLRUGetPromotes 验证 Get 会提升到最近使用。
func TestLRUGetPromotes(t *testing.T) {
	c := newLRU(2)
	c.Put("a", "1")
	c.Put("b", "2")
	// 访问 a，使其成为最近使用
	c.Get("a")
	c.Put("c", "3")
	// b 应被淘汰（它最久未用）
	if _, ok := c.Get("b"); ok {
		t.Error("b 应被淘汰")
	}
	if _, ok := c.Get("a"); !ok {
		t.Error("a 被访问过，应保留")
	}
}

// TestQueueRequestMissIsPending 验证未命中返回 pending 且不阻塞。
func TestQueueRequestMissIsPending(t *testing.T) {
	dir := t.TempDir()
	q := NewQueue(dir, 2, func(abs string, fp Fingerprint, class SizeClass) (string, error) {
		return "", nil
	}, nil)
	defer q.Close()

	uri, pending := q.Request("/nonexistent/x.png", Fingerprint{Size: 1, ModTime: 1}, true)
	if !pending {
		t.Error("未命中应返回 pending=true")
	}
	if uri != "" {
		t.Error("未命中应返回空 dataURI")
	}
}

// TestQueueDedup 验证同一任务不会重复入队。
func TestQueueDedup(t *testing.T) {
	dir := t.TempDir()
	var inflight atomic.Int32
	release := make(chan struct{})

	q := NewQueue(dir, 1, func(abs string, fp Fingerprint, class SizeClass) (string, error) {
		inflight.Add(1)
		<-release
		return "", nil
	}, nil)
	defer q.Close()

	fp := Fingerprint{Size: 1, ModTime: 1}
	q.Request("/a/x.png", fp, true)
	// 等 worker 取走任务
	time.Sleep(50 * time.Millisecond)
	// 再请求同一个，应被去重
	for i := 0; i < 5; i++ {
		q.Request("/a/x.png", fp, true)
	}
	close(release)
	time.Sleep(50 * time.Millisecond)

	if inflight.Load() != 1 {
		t.Errorf("同一任务应只生成 1 次，实际 %d", inflight.Load())
	}
}

// TestQueueConcurrentLimit 验证并发不超过 worker 数。
func TestQueueConcurrentLimit(t *testing.T) {
	dir := t.TempDir()
	var current atomic.Int32
	var peak atomic.Int32

	q := NewQueue(dir, 3, func(abs string, fp Fingerprint, class SizeClass) (string, error) {
		n := current.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		current.Add(-1)
		return "", nil
	}, nil)
	defer q.Close()

	for i := 0; i < 20; i++ {
		q.Request(filepath.Join("/a", string(rune('a'+i))+".png"), Fingerprint{Size: 1, ModTime: int64(i)}, true)
	}
	time.Sleep(300 * time.Millisecond)

	if p := peak.Load(); p > 3 {
		t.Errorf("并发峰值 %d 超过 worker 数 3", p)
	}
}

// TestQueueDiskCache 验证磁盘缓存命中后不再解码。
func TestQueueDiskCache(t *testing.T) {
	dir := t.TempDir()
	var genCount atomic.Int32

	q := NewQueue(dir, 2, func(abs string, fp Fingerprint, class SizeClass) (string, error) {
		genCount.Add(1)
		// 返回一个最小合法 data URI
		return "data:image/jpeg;base64,/9j/4AAQSkZJRg==", nil
	}, nil)
	defer q.Close()

	fp := Fingerprint{Size: 10, ModTime: 20}
	_, pending := q.Request("/a/x.png", fp, true)
	if !pending {
		t.Error("首次应 pending")
	}
	// 等生成完成并写盘
	time.Sleep(300 * time.Millisecond)

	// 第二次：应命中磁盘
	uri, pending2 := q.Request("/a/x.png", fp, true)
	if pending2 {
		t.Error("第二次应命中缓存（pending=false）")
	}
	if uri == "" {
		t.Error("第二次应返回 dataURI")
	}
	if genCount.Load() != 1 {
		t.Errorf("应只生成 1 次，实际 %d", genCount.Load())
	}
}

// TestQueueBatchNotify 验证批量通知（100ms 窗口）。
func TestQueueBatchNotify(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var batches [][]Result

	q := NewQueue(dir, 3, func(abs string, fp Fingerprint, class SizeClass) (string, error) {
		return "data:image/jpeg;base64,/9j/4AAQSkZJRg==", nil
	}, func(results []Result) {
		mu.Lock()
		batches = append(batches, results)
		mu.Unlock()
	})
	defer q.Close()

	for i := 0; i < 6; i++ {
		q.Request(filepath.Join("/a", string(rune('a'+i))+".png"), Fingerprint{Size: 1, ModTime: int64(i)}, true)
	}
	time.Sleep(400 * time.Millisecond)

	mu.Lock()
	total := 0
	for _, b := range batches {
		total += len(b)
	}
	mu.Unlock()

	if total != 6 {
		t.Errorf("应通知 6 条结果，实际 %d", total)
	}
}

// TestDiskPathSharded 验证磁盘路径按 sha1 前 2 位分片。
func TestDiskPathSharded(t *testing.T) {
	p := DiskCachePath("/cache", "/abs/file.png", Fingerprint{Size: 1, ModTime: 2})
	dir := filepath.Dir(p)
	base := filepath.Base(dir)
	if len(base) != 2 {
		t.Errorf("分片目录应为 2 位十六进制，实际 %q", base)
	}
	if filepath.Ext(p) != ".jpg" {
		t.Errorf("应为 .jpg 扩展名，实际 %s", filepath.Ext(p))
	}
}

// TestQueueCloseStopsWorkers 验证关闭后 worker 退出。
func TestQueueCloseStopsWorkers(t *testing.T) {
	dir := t.TempDir()
	q := NewQueue(dir, 2, func(abs string, fp Fingerprint, class SizeClass) (string, error) {
		return "", nil
	}, nil)
	q.Close()
	time.Sleep(100 * time.Millisecond)
	// 关闭后再请求不应崩溃
	q.Request("/a/x.png", Fingerprint{Size: 1, ModTime: 1}, true)
}

var _ = os.Stat

// TestSizeClassKeys 两档缓存键与磁盘路径必须互不冲突。
//
// 若共用一档：九宫格会把 320 图当大图用（糊），
// 或大图生成后覆盖九宫格缓存（白解码 720×960）。
func TestSizeClassKeys(t *testing.T) {
	fp := Fingerprint{Size: 1234, ModTime: 5678}

	grid := ThumbKey("C:/x/a.png", fp)
	large := ThumbKeySized("C:/x/a.png", fp, SizeLarge)

	if grid == large {
		t.Fatal("grid 与 large 的缓存键必须不同")
	}
	if ThumbKey("C:/x/a.png", fp) != grid {
		t.Error("ThumbKey 应等价于 SizeGrid 档")
	}
	if !strings.HasSuffix(large, "-l") {
		t.Errorf("large 键应以 -l 结尾，实际 %q", large)
	}

	pGrid := DiskCachePath("C:/cache", "C:/x/a.png", fp)
	pLarge := DiskCachePathSized("C:/cache", "C:/x/a.png", fp, SizeLarge)
	if pGrid == pLarge {
		t.Fatal("两档的磁盘路径必须不同，否则会互相覆盖")
	}
	if !strings.HasSuffix(pLarge, "-l.jpg") {
		t.Errorf("large 磁盘文件名应以 -l.jpg 结尾，实际 %q", pLarge)
	}
}

// TestQueueTwoClassesCoexist 同一文件的两档任务都能完成，互不吞掉。
func TestQueueTwoClassesCoexist(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	seen := map[SizeClass]int{}

	gen := func(abs string, fp Fingerprint, class SizeClass) (string, error) {
		mu.Lock()
		seen[class]++
		mu.Unlock()
		return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte("x")), nil
	}
	q := NewQueue(dir, 2, gen, nil)
	defer q.Close()

	fp := Fingerprint{Size: 1, ModTime: 2}
	q.Request("C:/x/a.png", fp, true)                                  // grid
	q.RequestSized("C:/x/a.png", fp, SizeLarge, true)                  // large

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if q.Stats().Generated >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	g, l := seen[SizeGrid], seen[SizeLarge]
	mu.Unlock()
	if g == 0 {
		t.Error("grid 档应被生成")
	}
	if l == 0 {
		t.Error("large 档应被生成 —— 若被 grid 任务去重吞掉，这里会是 0")
	}
}
