package main

// derive_test.go —— 派生索引测试（对应 §3.6 依赖图重复累积缺陷）
//
// 历史缺陷：afterResourceMoved 用增量 append 维护反向图，
// 同一文件被移动 N 次就会积累 N 条重复引用，
// 导致 zipmodStillReferenced 统计虚高、本该可隔离的 zipmod 隔离不掉。
//
// 修法：反向图永远由正向数据 derive() 现算。

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDeriveNoDuplicateRefs 验证重复移动同一文件不会在反向图里累积重复引用。
func TestDeriveNoDuplicateRefs(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	card := env.femaleCard("a.png")
	writeFile(t, card, "card")

	// 正向索引：card 依赖 guid.x
	m.FileDeps[card] = []string{"guid.x"}
	m.derive()

	if refs := m.GlobalDepMap["guid.x"]; len(refs) != 1 {
		t.Fatalf("初始应有 1 条引用，实际 %d: %v", len(refs), refs)
	}

	// 反复移动同一文件（每次移动后派生）
	prev := card
	for i := 0; i < 5; i++ {
		// 每次移动到不同位置，模拟真实的多次移动
		dst := filepath.Join(env.femaleCard("moved"), "v"+string(rune('0'+i))+".png")
		m.FileDeps[dst] = []string{"guid.x"}
		delete(m.FileDeps, prev)
		m.derive()

		refs := m.GlobalDepMap["guid.x"]
		if len(refs) != 1 {
			t.Fatalf("第 %d 次移动后应始终只有 1 条引用，实际 %d: %v", i+1, len(refs), refs)
		}
		if refs[0] != dst {
			t.Fatalf("第 %d 次移动后引用应指向新位置 %s，实际 %s", i+1, dst, refs[0])
		}
		prev = dst
	}
}

// TestDeriveAfterRestoreReflectsFile 验证恢复后依赖图指向新位置。
func TestDeriveAfterRestoreReflectsFile(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	card := env.femaleCard("a.png")
	writeFile(t, card, "card")
	m.Scanned = true
	m.FileDeps[card] = []string{"guid.x"}
	m.derive()

	modPath := filepath.Join(m.ModsDir(), "m.zipmod")
	writeFile(t, modPath, "mod")
	m.ZipmodDB["guid.x"] = modPath

	// 隔离：文件移走，引用应从反向图消失
	m.Isolate([]string{card})
	if refs := m.GlobalDepMap["guid.x"]; len(refs) != 0 {
		t.Errorf("隔离后不应再有引用，实际 %v", refs)
	}

	// 恢复：文件移回，引用应重新出现
	delCard := filepath.Join(m.DelDir(ResourceKinds[0]), "a.png")
	m.Restore([]string{delCard})
	refs := m.GlobalDepMap["guid.x"]
	if len(refs) != 1 || refs[0] != card {
		t.Errorf("恢复后应指向原位置 %s，实际 %v", card, refs)
	}
}

// TestDeriveSortsRefs 验证派生结果排序稳定（便于测试断言与 UI 一致）。
func TestDeriveSortsRefs(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	a := env.femaleCard("z.png")
	b := env.femaleCard("a.png")
	c := env.femaleCard("m.png")
	m.FileDeps[a] = []string{"g"}
	m.FileDeps[b] = []string{"g"}
	m.FileDeps[c] = []string{"g"}
	m.derive()

	refs := m.GlobalDepMap["g"]
	if len(refs) != 3 {
		t.Fatalf("应有 3 条引用，实际 %d", len(refs))
	}
	for i := 1; i < len(refs); i++ {
		if refs[i-1] > refs[i] {
			t.Errorf("引用未排序: %v", refs)
			break
		}
	}
}

// TestDeriveZipmodDBStable 验证同 GUID 多 zipmod 时保留稳定结果。
func TestDeriveZipmodDBStable(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	m.ZipmodDB["g"] = filepath.Join(m.ModsDir(), "z.zipmod")
	m.ZipmodDB["g"] = filepath.Join(m.ModsDir(), "a.zipmod")
	m.deriveZipmodDB()

	// map 本身 key 唯一，此处验证派生后仍可用且路径有效
	if p, ok := m.ZipmodDB["g"]; !ok || p == "" {
		t.Error("派生后 GUID 索引应保留")
	}
}

// TestRemoveDepDoesNotTouchReverseMap 验证 removeDep 只删正向数据。
//
// 反向图由 derive() 现算，removeDep 不应直接改它 ——
// 否则就退回到增量修补的老路子上。
func TestRemoveDepDoesNotTouchReverseMap(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	card := env.femaleCard("a.png")
	m.FileDeps[card] = []string{"g"}
	m.derive()

	m.removeDep(card)

	// 正向索引已删
	if _, ok := m.FileDeps[card]; ok {
		t.Error("removeDep 应删除正向索引条目")
	}
	// 反向图仍是旧值（未重建），这正是"不增量修补"的体现
	if refs := m.GlobalDepMap["g"]; len(refs) != 1 {
		t.Errorf("removeDep 不应直接修改反向图，实际 %v", refs)
	}
	// 重新派生后应一致
	m.derive()
	if refs := m.GlobalDepMap["g"]; len(refs) != 0 {
		t.Errorf("派生后不应再有引用，实际 %v", refs)
	}
}

// TestNormalizePathAcceptsBothForms 验证隐藏路径归一化（对应 §3.3）。
//
// 绝对路径与相对路径都必须能匹配到同一条隐藏记录。
func TestNormalizePathAcceptsBothForms(t *testing.T) {
	env := newTestEnv(t)
	inv := env.manager.invisible

	target := filepath.Join(env.femaleCard("hiddenFolder"))
	writeFile(t, filepath.Join(target, "sub"), "")
	os.RemoveAll(filepath.Join(target, "sub"))

	if res := inv.HideFolders([]string{target}, SideKKS); res.Hidden != 1 {
		t.Fatalf("隐藏失败: %v", res.Logs)
	}

	abs := target
	rel := "UserData/chara/female/hiddenFolder"

	// 绝对路径
	r1, side1, ok1 := inv.normalizePath(abs)
	if !ok1 || r1 != rel || side1 != SideKKS {
		t.Errorf("绝对路径归一化错误: rel=%q side=%q ok=%v", r1, side1, ok1)
	}
	// 相对路径
	r2, side2, ok2 := inv.normalizePath(rel)
	if !ok2 || r2 != rel || side2 != SideKKS {
		t.Errorf("相对路径归一化错误: rel=%q side=%q ok=%v", r2, side2, ok2)
	}
}

// TestRestoreFoldersAcceptsAbsolutePath 验证用绝对路径也能恢复（§3.3 的核心回归测试）。
func TestRestoreFoldersAcceptsAbsolutePath(t *testing.T) {
	env := newTestEnv(t)
	inv := env.manager.invisible

	target := filepath.Join(env.femaleCard("restoreMe"))
	writeFile(t, filepath.Join(target, "sub"), "")
	os.RemoveAll(filepath.Join(target, "sub"))

	if res := inv.HideFolders([]string{target}, SideKKS); res.Hidden != 1 {
		t.Fatalf("隐藏失败: %v", res.Logs)
	}

	// 传绝对路径（这是前端原先的做法，会导致「未找到隐藏记录」）
	res := inv.RestoreFolders([]string{target})
	if res.Restored != 1 {
		t.Fatalf("用绝对路径应能恢复，实际恢复 %d 个，日志 %v", res.Restored, res.Logs)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("目录未被重建: %v", err)
	}
}
