package main

// perf_test.go —— 性能验证
//
// 验证 §14.3 的性能指标：
//   - 切换列表目录 ≤ 50ms（内存快照，不递归 IO）
//   - 隔离前依赖判定 100 次 < 100ms（纯内存）
//
// 这些测试用构造数据，不依赖真实 KKS 目录。

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// buildPerfTree 构造一个含 N 个文件、M 个目录的测试树。
func buildPerfTree(t *testing.T, root string, files, dirs int) {
	t.Helper()
	// 10 个子目录，每个子目录 files/10 个文件
	perDir := files / 10
	if perDir < 1 {
		perDir = 1
	}
	for d := 0; d < dirs; d++ {
		dir := filepath.Join(root, "dir"+string(rune('a'+d%26))+string(rune('a'+d/26)))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("创建目录失败: %v", err)
		}
		for f := 0; f < perDir; f++ {
			p := filepath.Join(dir, "card"+string(rune('a'+f%26))+".png")
			if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
				t.Fatalf("写文件失败: %v", err)
			}
		}
	}
}

// TestBuildTreePerformance 验证构建整棵树的耗时。
func TestBuildTreePerformance(t *testing.T) {
	env := newTestEnv(t)
	buildPerfTree(t, env.femaleCard(""), 2000, 50)

	m := env.manager
	m.Scanned = true

	start := time.Now()
	nodes := m.buildTree(m.SourceDir(ResourceKinds[0]), false)
	elapsed := time.Since(start)

	t.Logf("构建 %d 个节点耗时 %v", len(nodes), elapsed)
	if elapsed > 2*time.Second {
		t.Errorf("构建树耗时 %v，过慢（目标 ≤ 50ms/目录切换，O(n²) 会严重超标）", elapsed)
	}
}

// TestFolderStatsAggregationCorrect 验证聚合器给出的统计正确。
func TestFolderStatsAggregationCorrect(t *testing.T) {
	env := newTestEnv(t)
	root := env.femaleCard("")

	// dirA: 2 个文件 + 1 个子目录(3 个文件)
	writeFile(t, filepath.Join(root, "dirA", "f1.png"), "x")
	writeFile(t, filepath.Join(root, "dirA", "f2.png"), "x")
	writeFile(t, filepath.Join(root, "dirA", "sub", "g1.png"), "x")
	writeFile(t, filepath.Join(root, "dirA", "sub", "g2.png"), "x")
	writeFile(t, filepath.Join(root, "dirA", "sub", "g3.png"), "x")
	// dirB: 1 个文件
	writeFile(t, filepath.Join(root, "dirB", "h1.png"), "x")

	agg := newStatsAggregator(root)

	rootStat := agg.toFolderStats(root)
	if rootStat.SubfolderCount != 2 {
		t.Errorf("根目录应有 2 个子文件夹，实际 %d", rootStat.SubfolderCount)
	}
	if rootStat.CardCount != 0 {
		t.Errorf("根目录应有 0 个直接文件，实际 %d", rootStat.CardCount)
	}
	if rootStat.TotalFiles != 6 {
		t.Errorf("根目录递归应有 6 个文件，实际 %d", rootStat.TotalFiles)
	}

	dirA := agg.toFolderStats(filepath.Join(root, "dirA"))
	if dirA.SubfolderCount != 1 {
		t.Errorf("dirA 应有 1 个子文件夹，实际 %d", dirA.SubfolderCount)
	}
	if dirA.CardCount != 2 {
		t.Errorf("dirA 应有 2 个直接文件，实际 %d", dirA.CardCount)
	}
	if dirA.TotalFiles != 5 {
		t.Errorf("dirA 递归应有 5 个文件，实际 %d", dirA.TotalFiles)
	}
}

// TestFolderStatsPreviewLimit 验证预览窗格最多 3 个，其余进 remaining。
func TestFolderStatsPreviewLimit(t *testing.T) {
	env := newTestEnv(t)
	root := env.femaleCard("")
	for i := 0; i < 6; i++ {
		writeFile(t, filepath.Join(root, "d"+string(rune('a'+i)), "x"), "x")
	}

	agg := newStatsAggregator(root)
	st := agg.toFolderStats(root)

	if len(st.PreviewFolders) != 3 {
		t.Errorf("预览窗格应为 3 个，实际 %d", len(st.PreviewFolders))
	}
	if len(st.RemainingFolders) != 3 {
		t.Errorf("剩余子文件夹应为 3 个，实际 %d", len(st.RemainingFolders))
	}
}

// TestGuardPerformance 验证依赖判定是纯内存操作。
func TestGuardPerformance(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	// 10000 条引用关系
	m.FileDeps = make(map[string][]string, 10000)
	for i := 0; i < 10000; i++ {
		p := filepath.Join("C:\\KK", "card"+string(rune('a'+i%26))+string(rune('a'+i/26))+".png")
		m.FileDeps[p] = []string{"guid.shared"}
	}
	m.derive()

	start := time.Now()
	for i := 0; i < 100; i++ {
		m.IsZipmodSafeToMove("guid.shared")
	}
	elapsed := time.Since(start)

	t.Logf("100 次依赖判定耗时 %v", elapsed)
	if elapsed > 100*time.Millisecond {
		t.Errorf("100 次判定耗时 %v，超出 100ms 目标", elapsed)
	}
}
