package main

// e2e_test.go —— 端到端集成测试
//
// 用真实的 zipmod 文件（含 manifest.xml）验证：
// 扫描 -> 构建依赖图 -> 隔离 -> 恢复 的完整链路。

import (
	"path/filepath"
	"testing"
)

// makeZipmod 生成一个带 manifest.xml 的 zipmod。
func makeZipmod(t *testing.T, path, guid string) {
	t.Helper()
	writeZip(t, path, map[string]string{
		"manifest.xml": `<?xml version="1.0" encoding="utf-8"?>
<manifest>
  <name>Mod ` + guid + `</name>
  <guid>` + guid + `</guid>
  <version>1.0.0</version>
  <author>tester</author>
</manifest>`,
		"data.txt": "payload of " + guid,
	})
}





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
