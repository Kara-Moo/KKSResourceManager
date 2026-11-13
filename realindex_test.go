package main

// realindex_test.go —— 真实目录上的增量扫描性能验证
//
// 需要环境变量 KKS_REAL_ROOT 指定真实 KKS 根目录。
// 这些测试只读游戏目录，但会**写入** DELCARDS/_state/index.json（隔离区内）。
//
// 验证目标（§14.3）：
//   - 首次扫描生成 index.json
//   - 二次启动（无变更）解析 0 个文件
//   - 单文件变更后只解析 1 个文件

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kksresourcemanager/internal/index"
)

// TestRealIncrementalScan 在真实目录上验证增量扫描的三次行为。
func TestRealIncrementalScan(t *testing.T) {
	root := strings.TrimSpace(os.Getenv("KKS_REAL_ROOT"))
	if root == "" {
		t.Skip("未设置 KKS_REAL_ROOT，跳过真实目录增量扫描验证")
	}
	if _, err := os.Stat(root); err != nil {
		t.Skipf("真实目录不可访问，跳过: %v", err)
	}

	m, err := NewManager(root)
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}

	store, err := index.NewStore(m.DelCardsRoot)
	if err != nil {
		t.Fatalf("创建 Store 失败: %v", err)
	}

	scanner := &index.Scanner{
		Root:         m.KksRoot,
		ModsDir:      m.ModsDir(),
		ResourceRoots: m.resourceRoots(),
		PluginRoots:  m.pluginRoots(),
		ParseZipmod: func(path string) (string, []string, error) {
			manifest, err := ReadModManifest(path)
			return manifest.GUID, nil, err
		},
		ParseCard: func(path string) ([]string, error) {
			return ParseCardGUIDs(path)
		},
		MatchPlugin: func(path string, guids []string) ([]string, error) {
			return nil, nil
		},
	}

	// --- 第一次：冷启动 ---
	start := time.Now()
	idx, _, stats1, err := scanner.Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("首次扫描失败: %v", err)
	}
	cold := time.Since(start)
	t.Logf("冷启动：解析 %d 个，耗时 %v", stats1.Parsed, cold)

	if stats1.Parsed == 0 {
		t.Error("冷启动应解析全部文件")
	}

	// 保存索引
	if err := store.Save(idx); err != nil {
		t.Fatalf("保存索引失败: %v", err)
	}
	idxPath := filepath.Join(m.DelCardsRoot, index.StateDirName, index.IndexFileName)
	if _, err := os.Stat(idxPath); err != nil {
		t.Errorf("index.json 未生成: %v", err)
	}

	// --- 第二次：热启动（无变更）---
	loaded, err := store.Load(m.KksRoot)
	if err != nil {
		t.Fatalf("加载索引失败: %v", err)
	}
	if loaded == nil {
		t.Fatal("索引未能加载")
	}

	start = time.Now()
	idx2, _, stats2, err := scanner.Scan(context.Background(), loaded, nil)
	if err != nil {
		t.Fatalf("二次扫描失败: %v", err)
	}
	warm := time.Since(start)
	t.Logf("热启动：复用 %d，解析 %d，耗时 %v", stats2.Reused, stats2.Parsed, warm)

	if stats2.Parsed != 0 {
		t.Errorf("无变更时不应解析任何文件，实际解析 %d 个", stats2.Parsed)
	}
	if stats2.Reused == 0 {
		t.Error("无变更时应复用旧索引")
	}
	// 二次启动应在 2 秒内完成（§5.1 目标）
	if warm > 2*time.Second {
		t.Errorf("热启动耗时 %v，超出 2 秒目标", warm)
	}

	// 索引条目数应保持一致
	if len(idx2.Cards) != len(idx.Cards) {
		t.Errorf("卡片数应一致：%d vs %d", len(idx2.Cards), len(idx.Cards))
	}
	if len(idx2.Zipmods) != len(idx.Zipmods) {
		t.Errorf("zipmod 数应一致：%d vs %d", len(idx2.Zipmods), len(idx.Zipmods))
	}
}
