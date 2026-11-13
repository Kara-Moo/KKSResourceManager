package main

// realthumb_test.go —— 缩略图队列的真实目录验证（v2.2 FIX-01 验收）
//
// 用真实根目录跑完整链路：attachManager 启动队列 → BuildPanel 请求缩略图
// → 后台生成 → 二次请求命中缓存。
//
// 运行：
//   KKS_REAL_ROOT="E:\KKV114" go test -run TestRealThumb -v -timeout 10m

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kksresourcemanager/internal/index"
)

func TestRealThumbQueue(t *testing.T) {
	root := strings.TrimSpace(os.Getenv("KKS_REAL_ROOT"))
	if root == "" {
		t.Skip("未设置 KKS_REAL_ROOT，跳过")
	}

	m, err := NewManager(root)
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}

	// 复用已有索引（避免重新扫描）
	store, err := index.NewStore(m.DelCardsRoot)
	if err != nil {
		t.Fatalf("打开索引失败: %v", err)
	}
	old, _ := store.Load(m.KksRoot)
	if old == nil {
		t.Skip("无索引，请先跑一次扫描")
	}
	m.applyIndex(old, index.Derive(old))

	// --- 1. 通过 attachManager 启动队列（这是 FIX-01 的修复点）---
	app := NewApp()
	app.attachManager(m)
	defer m.StopThumbQueue()

	if !m.ThumbQueueRunning() {
		t.Fatal("attachManager 之后队列必须已启动")
	}

	// --- 2. 找一个有卡片的目录 ---
	femaleDir := filepath.Join(m.KksRoot, "UserData", "chara", "female")
	entries, err := os.ReadDir(femaleDir)
	if err != nil {
		t.Fatalf("读取女卡目录失败: %v", err)
	}

	var cardDir string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(femaleDir, e.Name())
		subEntries, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		cards := 0
		for _, se := range subEntries {
			if !se.IsDir() && strings.EqualFold(filepath.Ext(se.Name()), ".png") {
				cards++
			}
		}
		if cards >= 5 {
			cardDir = sub
			break
		}
	}
	if cardDir == "" {
		t.Skip("未找到含 ≥5 张卡片的目录")
	}
	rel, _ := filepath.Rel(femaleDir, cardDir)
	t.Logf("测试目录: %s（%s 下）", cardDir, filepath.ToSlash(rel))

	// --- 3. 首次 BuildPanel：应命中 miss 并返回 pending ---
	panel, err := m.BuildPanel("left", "female_card", filepath.ToSlash(rel), "")
	if err != nil {
		t.Fatalf("BuildPanel 失败: %v", err)
	}
	pending := 0
	for _, sec := range panel.Sections {
		for _, tile := range sec.Tiles {
			if tile.Pending {
				pending++
			}
		}
	}
	t.Logf("首次面板：%d 个格子待生成", pending)

	// --- 4. 等 3 秒，缩略图应可用 ---
	//
	// 冷启动（首次跑）：misses>0 → 后台生成 → generated 增长
	// 热启动（缓存已在磁盘）：diskHits>0，generated 不增长
	// 两种都是正确行为，断言按实际路径分别校验。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st := m.ThumbStats()
		if st.Generated > 0 || st.DiskHits > 0 || st.MemHits > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	stats := m.ThumbStats()
	t.Logf("3 秒后统计：generated=%d memHits=%d diskHits=%d misses=%d failed=%d",
		stats.Generated, stats.MemHits, stats.DiskHits, stats.Misses, stats.Failed)

	if stats.Generated == 0 && stats.DiskHits == 0 && stats.MemHits == 0 {
		t.Fatalf("3 秒内未产出任何可用缩略图：%+v", stats)
	}
	if stats.Failed > 0 {
		t.Errorf("生成失败 %d 张，应为 0", stats.Failed)
	}
	if pending > 0 && stats.Generated == 0 && stats.DiskHits == 0 {
		t.Errorf("面板报告 %d 个格子 pending，但队列既未生成也无磁盘命中", pending)
	}

	// 等批量通知把剩余几张也送完（仅冷启动需要）
	time.Sleep(1200 * time.Millisecond)
	statsAfter := m.ThumbStats()
	t.Logf("补齐后：generated=%d failed=%d", statsAfter.Generated, statsAfter.Failed)

	// --- 5. 二次请求：应全部命中缓存，generated 不增长 ---
	before := m.ThumbStats()
	panel2, err := m.BuildPanel("left", "female_card", filepath.ToSlash(rel), "")
	if err != nil {
		t.Fatalf("二次 BuildPanel 失败: %v", err)
	}
	stillPending := 0
	for _, sec := range panel2.Sections {
		for _, tile := range sec.Tiles {
			if tile.Pending {
				stillPending++
			}
		}
	}
	after := m.ThumbStats()
	t.Logf("二次面板：仍待生成 %d 个；generated %d -> %d；命中 %d -> %d",
		stillPending, before.Generated, after.Generated,
		before.MemHits+before.DiskHits, after.MemHits+after.DiskHits)

	if stillPending > 0 {
		t.Errorf("二次请求仍有 %d 个格子 pending，说明缓存未生效", stillPending)
	}
	if after.Generated > before.Generated {
		t.Errorf("缓存命中时不应重新生成：%d -> %d", before.Generated, after.Generated)
	}
	if after.MemHits+after.DiskHits <= before.MemHits+before.DiskHits {
		t.Errorf("命中计数应增长：%+v -> %+v", before, after)
	}

	// --- 6. 缓存目录应有 .jpg（sha1 分片，需递归查找）---
	thumbDir := filepath.Join(m.DelCardsRoot, "_state", ThumbDirName)
	jpg := 0
	var total int64
	err = filepath.Walk(thumbDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(info.Name()), ".jpg") {
			return nil
		}
		jpg++
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatalf("遍历缓存目录失败: %v", err)
	}
	if jpg == 0 {
		t.Error("缓存目录应出现 .jpg 文件")
	}
	if jpg > 0 {
		avg := float64(total) / float64(jpg) / 1024
		t.Logf("缓存文件 %d 个，平均 %.0f KB", jpg, avg)
		// §12 验收：缩略图体积应在 20–60KB 量级
		if avg > 200 {
			t.Errorf("缩略图平均 %.0f KB，超过预期（应 ≤200KB）", avg)
		}
	}
}
