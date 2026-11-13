package main

// e2e_test.go —— 端到端集成测试
//
// 用真实的 zipmod 文件（含 manifest.xml）验证：
// 扫描 -> 构建依赖图 -> 隔离 -> 恢复 的完整链路。

import (
	"os"
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

func TestEndToEndScanIsolateRestore(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	// --- 构造场景 ---
	// 两个角色卡，都依赖 mod A；其中一个还依赖 mod B
	cardShared := env.femaleCard(filepath.Join("group", "card1.png"))
	cardSole := env.femaleCard(filepath.Join("group", "card2.png"))
	writeFile(t, cardShared, "card1")
	writeFile(t, cardSole, "card2")

	modA := filepath.Join(m.ModsDir(), "modA.zipmod")
	modB := filepath.Join(m.ModsDir(), "modB.zipmod")
	makeZipmod(t, modA, "guid.a")
	makeZipmod(t, modB, "guid.b")

	// --- 执行全量扫描 ---
	result := m.ScanAll(nil)
	if result.ZipmodCount != 2 {
		t.Fatalf("应索引到 2 个 zipmod，实际 %d", result.ZipmodCount)
	}
	// 卡片是伪造的 PNG，无法解析出真实依赖，这里直接注入依赖关系模拟
	m.FileDeps[cardShared] = []string{"guid.a"}
	m.FileDeps[cardSole] = []string{"guid.a", "guid.b"}
	m.derive()

	// --- 隔离 cardShared ---
	// guid.a 仍被 cardSole 引用 -> 保留
	// guid.b 不涉及
	res := m.Isolate([]string{cardShared})
	if res.MovedCards != 1 {
		t.Fatalf("应移动 1 个资源，实际 %d，日志 %v", res.MovedCards, res.Logs)
	}
	if res.MovedMods != 0 {
		t.Errorf("共享 mod 不应移动，实际移动 %d 个", res.MovedMods)
	}
	if res.KeptMods != 1 {
		t.Errorf("应保留 1 个 mod，实际 %d", res.KeptMods)
	}

	// 目录结构保留
	isolated := filepath.Join(m.DelDir(ResourceKinds[0]), "group", "card1.png")
	if _, err := os.Stat(isolated); err != nil {
		t.Fatalf("隔离区未保留目录结构: %v", err)
	}
	// 游戏原目录仍在
	if _, err := os.Stat(filepath.Dir(cardShared)); err != nil {
		t.Errorf("游戏目录不应被删除: %v", err)
	}

	// --- 再隔离 cardSole ---
	// 此时 guid.a 的引用只剩 cardSole，移除后彻底孤立 -> 可移动
	// guid.b 本就只被 cardSole 引用 -> 同时孤立 -> 一并移动
	res2 := m.Isolate([]string{cardSole})
	if res2.MovedCards != 1 {
		t.Fatalf("应移动 1 个资源，实际 %d", res2.MovedCards)
	}
	if res2.MovedMods != 2 {
		t.Errorf("2 个孤立 mod 都应被移动，实际移动 %d 个，日志 %v", res2.MovedMods, res2.Logs)
	}
	// modA 与 modB 都已移入隔离区
	for _, name := range []string{"modA.zipmod", "modB.zipmod"} {
		if _, err := os.Stat(filepath.Join(m.DelZipmodDir(), name)); err != nil {
			t.Errorf("%s 未移入隔离区: %v", name, err)
		}
	}

	// --- 恢复 ---
	res3 := m.Restore([]string{
		filepath.Join(m.DelDir(ResourceKinds[0]), "group", "card1.png"),
		filepath.Join(m.DelZipmodDir(), "modA.zipmod"),
		filepath.Join(m.DelZipmodDir(), "modB.zipmod"),
	})
	if res3.RestoredCard != 1 {
		t.Errorf("应恢复 1 个资源，实际 %d，日志 %v", res3.RestoredCard, res3.Logs)
	}
	if res3.RestoredMod != 2 {
		t.Errorf("应恢复 2 个 mod，实际 %d，日志 %v", res3.RestoredMod, res3.Logs)
	}
	// 资源回到原位
	if _, err := os.Stat(cardShared); err != nil {
		t.Errorf("资源未回到原位置: %v", err)
	}
	// mod 回到 mods 目录
	for _, p := range []string{modA, modB} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("mod 未回到 mods 目录 %s: %v", p, err)
		}
	}
	// 隔离区 zipmod 目录应已清空
	entries, err := os.ReadDir(m.DelZipmodDir())
	if err != nil {
		t.Fatalf("读取隔离区失败: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("隔离区 zipmod 应已清空，实际剩余 %d 项", len(entries))
	}
	// 8 个标准隔离子目录必须始终保留（只增不删）
	for _, k := range ResourceKinds {
		if _, err := os.Stat(m.DelDir(k)); err != nil {
			t.Errorf("标准隔离目录 %s 不应被清理掉: %v", k.DelName, err)
		}
	}
	if _, err := os.Stat(m.DelZipmodDir()); err != nil {
		t.Errorf("标准隔离目录 %s 不应被清理掉: %v", delZipmodDirName, err)
	}
}

func TestScanAllFindsZipmodGUIDs(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	modA := filepath.Join(m.ModsDir(), "modA.zipmod")
	nested := filepath.Join(m.ModsDir(), "sub", "modC.zipmod")
	makeZipmod(t, modA, "guid.a")
	makeZipmod(t, nested, "guid.c")

	result := m.ScanAll(nil)
	if result.ZipmodCount != 2 {
		t.Fatalf("应索引到 2 个 zipmod（含子目录），实际 %d", result.ZipmodCount)
	}
	if _, ok := m.ZipmodDB["guid.a"]; !ok {
		t.Error("未能索引 guid.a")
	}
	if _, ok := m.ZipmodDB["guid.c"]; !ok {
		t.Error("未能索引嵌套目录中的 guid.c")
	}
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
