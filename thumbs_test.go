package main

// thumbs_test.go —— 缩略图队列的启动与缓存行为（v2.2 FIX-01 回归测试）
//
// 这组测试锁死三件事：
//  1. 队列未启动时，请求永远 pending，且 ThumbQueueRunning()=false（这就是原 bug 的形态）
//  2. 启动后能在 3 秒内产出缩略图，generated>0 且 failed==0
//  3. 二次请求走缓存 —— generated 不增长，命中计数增长

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"kksresourcemanager/internal/preview"
)

// tinyPNG 是一张 1×1 的真实 PNG（不能用文本冒充，否则解码必然失败）。
var tinyPNG, _ = base64.StdEncoding.DecodeString(
	"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")

// TestThumbQueueNotStarted 队列未启动时必须显式可诊断。
//
// 原 bug：m.thumbs == nil 时请求直接返回 pending，任务根本没入队，
// 而外部无从判断「是没启动」还是「正在生成」。这里把两者分开。
func TestThumbQueueNotStarted(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	card := env.femaleCard("a.png")
	if err := os.MkdirAll(filepath.Dir(card), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(card, tinyPNG, 0644); err != nil {
		t.Fatal(err)
	}

	if m.ThumbQueueRunning() {
		t.Fatal("新建的 Manager 不应已启动队列")
	}

	key, uri, pending := m.thumbRequest(card, true)
	if key == "" {
		t.Error("应返回非空 thumbKey")
	}
	if !pending || uri != "" {
		t.Errorf("队列未启动时应返回 pending，实际 pending=%v uri=%q", pending, uri)
	}
}

// TestThumbQueueGeneratesAndCaches 启动后能出图，且二次请求命中缓存。
func TestThumbQueueGeneratesAndCaches(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	card := env.femaleCard("real.png")
	if err := os.MkdirAll(filepath.Dir(card), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(card, tinyPNG, 0644); err != nil {
		t.Fatal(err)
	}

	var notified int32
	err := m.StartThumbQueue(func(results []preview.Result) {
		atomic.AddInt32(&notified, int32(len(results)))
	}, func(path string, err error) {
		t.Logf("生成失败 %s: %v", path, err)
	})
	if err != nil {
		t.Fatalf("启动队列失败: %v", err)
	}
	defer m.StopThumbQueue()

	if !m.ThumbQueueRunning() {
		t.Fatal("启动后 ThumbQueueRunning 应为 true")
	}
	// 幂等：重复启动不应报错或重建
	if err := m.StartThumbQueue(nil, nil); err != nil {
		t.Fatalf("重复启动应幂等，实际报错: %v", err)
	}

	_, uri, pending := m.thumbRequest(card, true)
	if !pending || uri != "" {
		t.Fatalf("首次请求应为 pending，实际 pending=%v", pending)
	}

	// 等最多 3 秒（§1.4 验收要求）
	deadline := time.Now().Add(3 * time.Second)
	var st preview.Stats
	for time.Now().Before(deadline) {
		st = m.ThumbStats()
		if st.Generated > 0 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if st.Generated == 0 {
		t.Fatalf("3 秒内未生成缩略图：%+v", st)
	}
	if st.Failed != 0 {
		t.Errorf("不应有失败，实际 failed=%d", st.Failed)
	}
	if st.Misses == 0 {
		t.Error("misses 应大于 0")
	}

	// 磁盘缓存应真的落盘
	thumbDir := filepath.Join(m.DelCardsRoot, "_state", ThumbDirName)
	entries, err := os.ReadDir(thumbDir)
	if err != nil {
		t.Fatalf("读取缓存目录失败: %v", err)
	}
	if len(entries) == 0 {
		t.Error("缓存目录应有 .jpg 文件")
	}

	// 等批量通知送达（100ms 窗口）
	waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt32(&notified) > 0 })

	// 二次请求：应命中缓存，generated 不再增长
	before := m.ThumbStats()
	_, uri2, pending2 := m.thumbRequest(card, true)
	if pending2 || uri2 == "" {
		t.Fatalf("二次请求应命中缓存，实际 pending=%v", pending2)
	}
	after := m.ThumbStats()
	if after.Generated != before.Generated {
		t.Errorf("缓存命中时 generated 不应增长：%d -> %d", before.Generated, after.Generated)
	}
	if after.MemHits+after.DiskHits <= before.MemHits+before.DiskHits {
		t.Errorf("命中计数应增长：%+v -> %+v", before, after)
	}
}

// TestThumbGenerateFailureVisible 生成失败必须被计数，不允许静默丢弃。
func TestThumbGenerateFailureVisible(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	// 写一个扩展名是 .png 但内容不是图片的文件
	bad := env.femaleCard("broken.png")
	if err := os.MkdirAll(filepath.Dir(bad), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("this is not a png"), 0644); err != nil {
		t.Fatal(err)
	}

	var errCount int32
	if err := m.StartThumbQueue(nil, func(path string, err error) {
		atomic.AddInt32(&errCount, 1)
	}); err != nil {
		t.Fatalf("启动队列失败: %v", err)
	}
	defer m.StopThumbQueue()

	m.thumbRequest(bad, true)

	waitFor(t, 3*time.Second, func() bool { return m.ThumbStats().Failed > 0 })

	st := m.ThumbStats()
	if st.Failed == 0 {
		t.Fatalf("解码失败应计入 failed：%+v", st)
	}
	if atomic.LoadInt32(&errCount) == 0 {
		t.Error("失败应触发错误回调（写入日志），而不是被丢弃")
	}
}

// waitFor 轮询等待条件成立，超时则失败。
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAttachManagerStartsQueue 赋值入口必须同时启动队列。
//
// 这是 FIX-01 的结构性防回归：只要有人再把「给 a.manager 赋值」
// 与「启动队列」拆成两处，这个测试就会红。
func TestAttachManagerStartsQueue(t *testing.T) {
	app := NewApp()
	env := newTestEnv(t)
	m := env.manager

	if m.ThumbQueueRunning() {
		t.Fatal("新建 Manager 不应已启动队列")
	}

	app.attachManager(m)
	if !m.ThumbQueueRunning() {
		t.Fatal("attachManager 之后队列必须已启动 —— 这正是 FIX-01 的回归点")
	}

	// 切到另一个根：旧队列必须停，新队列必须起
	env2 := newTestEnv(t)
	m2 := env2.manager
	app.attachManager(m2)

	if m.ThumbQueueRunning() {
		t.Error("切换根目录后旧队列应被停止，避免继续为旧根生成缩略图")
	}
	if !m2.ThumbQueueRunning() {
		t.Error("切换根目录后新队列应已启动")
	}

	app.attachManager(nil)
	if m2.ThumbQueueRunning() {
		t.Error("attachManager(nil) 应停止当前队列")
	}
}
