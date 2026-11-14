package main

// e2e_test.go —— 端到端集成测试
//
// 验证扫描 -> 索引 -> 树数据 的完整链路。

import (
	"path/filepath"
	"testing"
)

func TestScanAllCountsResources(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	writeFile(t, env.femaleCard("a.png"), "x")
	writeFile(t, env.femaleCard(filepath.Join("s", "b.png")), "x")
	writeFile(t, filepath.Join(env.root, "UserData", "Studio", "scene", "c.png"), "x")
	writeFile(t, filepath.Join(env.root, "UserData", "coordinate", "female", "d.png"), "x")

	result := m.ScanAll(nil)
	if result.ResourceCount != 4 {
		t.Errorf("应扫描到 4 个资源文件，实际 %d", result.ResourceCount)
	}
}
