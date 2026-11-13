package main

// manager_test.go —— 核心管理器单元测试
//
// 重点验证需求中的三条红线：
//  1. 共享 zipmod 绝不被误移（仍有其它文件引用时必须原地保留）；
//  2. 资源隔离必须保留原有目录结构，且不删除游戏目录；
//  3. 恢复必须把文件放回原位。

import (
	"os"
	"path/filepath"
	"testing"
)

// testEnv 构造一套临时的 KKS 目录结构。
type testEnv struct {
	manager *Manager
	root    string
	delRoot string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	// 游戏根目录放在 <temp>/KKV114，隔离区应位于 <temp>/DELCARDS
	base := t.TempDir()
	root := filepath.Join(base, "KKV114")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatalf("创建根目录失败: %v", err)
	}
	m, err := NewManager(root)
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}
	return &testEnv{manager: m, root: root, delRoot: filepath.Join(base, "DELCARDS")}
}

// writeFile 写文件并自动创建父目录。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
}

// fakeCardPath 返回女卡目录下指定相对路径的绝对路径。
func (e *testEnv) femaleCard(rel string) string {
	return filepath.Join(e.root, "UserData", "chara", "female", rel)
}

func TestNewManagerCreatesDelDirs(t *testing.T) {
	env := newTestEnv(t)

	expected := []string{
		"DEL-female_card", "DEL-male_card", "DEL-scene", "DEL-pose",
		"DEL-coord_f", "DEL-coord_m", "DEL-zipmod",
	}
	for _, name := range expected {
		p := filepath.Join(env.delRoot, name)
		info, err := os.Stat(p)
		if err != nil {
			t.Errorf("隔离目录 %s 未创建: %v", name, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s 应为目录", name)
		}
	}
}

func TestClassifyResource(t *testing.T) {
	env := newTestEnv(t)
	p := env.femaleCard(filepath.Join("subA", "subB", "card.png"))

	kind, relDir, ok := env.manager.classifyResource(p)
	if !ok {
		t.Fatal("应识别为受管资源")
	}
	if kind.Mode != ModeFemaleCard {
		t.Errorf("类型应为 %s，实际 %s", ModeFemaleCard, kind.Mode)
	}
	if relDir != filepath.Join("subA", "subB") {
		t.Errorf("相对目录应为 subA\\subB，实际 %q", relDir)
	}

	// 游戏目录之外的文件必须被拒绝
	_, _, ok = env.manager.classifyResource(filepath.Join(env.root, "UserData", "chara", "other.png"))
	if ok {
		t.Error("非受管目录的文件不应被识别")
	}
}

// ---------------------------------------------------------------------------
// 依赖图与隔离
// ---------------------------------------------------------------------------

func TestIsolateKeepsSharedMod(t *testing.T) {
	env := newTestEnv(t)

	cardA := env.femaleCard("a.png")
	cardB := env.femaleCard("b.png")
	writeFile(t, cardA, "card A")
	writeFile(t, cardB, "card B")

	// 手工构造依赖图：两个文件都依赖同一个 GUID
	m := env.manager
	m.Scanned = true
	m.GlobalDepMap["shared.guid"] = []string{cardA, cardB}
	m.FileDeps[cardA] = []string{"shared.guid"}
	m.FileDeps[cardB] = []string{"shared.guid"}

	// 模拟一个 mods 目录中的 zipmod
	modPath := filepath.Join(m.ModsDir(), "shared.zipmod")
	writeFile(t, modPath, "mod")
	m.ZipmodDB["shared.guid"] = modPath

	// 只隔离 cardA
	res := m.Isolate([]string{cardA})

	if res.MovedCards != 1 {
		t.Errorf("应移动 1 个资源，实际 %d，日志: %v", res.MovedCards, res.Logs)
	}
	if res.MovedMods != 0 {
		t.Errorf("共享 MOD 不应被移动，实际移动了 %d 个", res.MovedMods)
	}
	if res.KeptMods != 1 {
		t.Errorf("共享 MOD 应被保留，实际保留 %d 个", res.KeptMods)
	}
	// zipmod 必须仍在原处
	if _, err := os.Stat(modPath); err != nil {
		t.Errorf("共享 zipmod 不应被移走: %v", err)
	}
	// cardA 应已移入隔离区且保留目录结构
	isolated := filepath.Join(m.DelDir(ResourceKinds[0]), "a.png")
	if _, err := os.Stat(isolated); err != nil {
		t.Errorf("资源未正确移入隔离区: %v", err)
	}
}

func TestIsolateMovesOrphanMod(t *testing.T) {
	env := newTestEnv(t)

	card := env.femaleCard("only.png")
	writeFile(t, card, "card")

	m := env.manager
	m.Scanned = true
	m.GlobalDepMap["orphan.guid"] = []string{card}
	m.FileDeps[card] = []string{"orphan.guid"}

	modPath := filepath.Join(m.ModsDir(), "orphan.zipmod")
	writeFile(t, modPath, "mod")
	m.ZipmodDB["orphan.guid"] = modPath

	res := m.Isolate([]string{card})

	if res.MovedMods != 1 {
		t.Errorf("孤立 MOD 应被移动，实际移动 %d 个，日志: %v", res.MovedMods, res.Logs)
	}
	// 原位置应不存在
	if _, err := os.Stat(modPath); !os.IsNotExist(err) {
		t.Error("孤立 zipmod 应从 mods 目录移走")
	}
	// 应出现在隔离区（扁平化）
	isolated := filepath.Join(m.DelZipmodDir(), "orphan.zipmod")
	if _, err := os.Stat(isolated); err != nil {
		t.Errorf("zipmod 未移入隔离区: %v", err)
	}
}

func TestIsolatePreservesDirectoryStructure(t *testing.T) {
	env := newTestEnv(t)

	card := env.femaleCard(filepath.Join("subA", "subB", "deep.png"))
	writeFile(t, card, "card")

	m := env.manager
	m.Scanned = true

	res := m.Isolate([]string{card})
	if res.MovedCards != 1 {
		t.Fatalf("应移动 1 个资源，实际 %d，日志: %v", res.MovedCards, res.Logs)
	}

	// 隔离区中必须保留 subA/subB 结构
	expected := filepath.Join(m.DelDir(ResourceKinds[0]), "subA", "subB", "deep.png")
	if _, err := os.Stat(expected); err != nil {
		t.Errorf("隔离区未保留目录结构: %v", err)
	}

	// 游戏原目录必须保留（空文件夹不删除）
	if _, err := os.Stat(filepath.Dir(card)); err != nil {
		t.Errorf("游戏目录中的空文件夹不应被删除: %v", err)
	}
}

func TestIsolateMultipleKinds(t *testing.T) {
	env := newTestEnv(t)

	female := env.femaleCard("f.png")
	male := filepath.Join(env.root, "UserData", "chara", "male", "m.png")
	scene := filepath.Join(env.root, "UserData", "Studio", "scene", "s.png")
	writeFile(t, female, "f")
	writeFile(t, male, "m")
	writeFile(t, scene, "s")

	m := env.manager
	m.Scanned = true

	res := m.Isolate([]string{female, male, scene})
	if res.MovedCards != 3 {
		t.Fatalf("应移动 3 个资源，实际 %d，日志: %v", res.MovedCards, res.Logs)
	}

	// 每个类型都应进入各自的隔离目录
	checks := []string{
		filepath.Join(m.DelDir(ResourceKinds[0]), "f.png"),
		filepath.Join(m.DelDir(ResourceKinds[1]), "m.png"),
		filepath.Join(m.DelDir(ResourceKinds[2]), "s.png"),
	}
	for _, p := range checks {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("未移入正确的隔离目录: %s (%v)", p, err)
		}
	}
}

func TestIsolateSkipsDuplicateTarget(t *testing.T) {
	env := newTestEnv(t)

	card := env.femaleCard("dup.png")
	writeFile(t, card, "card")

	m := env.manager
	m.Scanned = true

	// 预先在隔离区放置同名文件
	target := filepath.Join(m.DelDir(ResourceKinds[0]), "dup.png")
	writeFile(t, target, "existing")

	res := m.Isolate([]string{card})
	if res.MovedCards != 0 {
		t.Errorf("目标已存在时不应移动，实际移动 %d 个", res.MovedCards)
	}
	if res.FailedCount == 0 {
		t.Error("应记录至少一个失败项")
	}
	// 原文件必须还在
	if _, err := os.Stat(card); err != nil {
		t.Errorf("移动失败时原文件不应丢失: %v", err)
	}
	// 隔离区已有文件内容不应被覆盖
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "existing" {
		t.Errorf("隔离区已有文件不应被覆盖，实际内容 %q", string(data))
	}
}

func TestIsolateWithoutScan(t *testing.T) {
	env := newTestEnv(t)
	card := env.femaleCard("x.png")
	writeFile(t, card, "card")

	res := env.manager.Isolate([]string{card})
	if res.MovedCards != 0 {
		t.Error("未扫描时不应执行隔离")
	}
	if _, err := os.Stat(card); err != nil {
		t.Error("未扫描时原文件必须保留")
	}
}

// TestIsolateAcrossBatches 验证分批隔离时的依赖计数仍然准确：
// 第一批隔离后，共享 mod 因仍被第二批的文件引用而保留；
// 第二批隔离后 mod 才彻底孤立并被移走。
func TestIsolateAcrossBatches(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	cardA := env.femaleCard("a.png")
	cardB := env.femaleCard("b.png")
	writeFile(t, cardA, "a")
	writeFile(t, cardB, "b")

	m.Scanned = true
	m.GlobalDepMap["shared.guid"] = []string{cardA, cardB}
	m.FileDeps[cardA] = []string{"shared.guid"}
	m.FileDeps[cardB] = []string{"shared.guid"}

	modPath := filepath.Join(m.ModsDir(), "shared.zipmod")
	writeFile(t, modPath, "mod")
	m.ZipmodDB["shared.guid"] = modPath

	// 第一批：只隔离 cardA
	res1 := m.Isolate([]string{cardA})
	if res1.MovedMods != 0 {
		t.Errorf("第一批后共享 mod 不应移动，实际移动 %d 个", res1.MovedMods)
	}
	// cardA 已从依赖图中移除，cardB 仍在
	if refs := m.GlobalDepMap["shared.guid"]; len(refs) != 1 || refs[0] != cardB {
		t.Errorf("依赖图应只剩 cardB 的引用，实际 %v", refs)
	}

	// 第二批：隔离 cardB —— 此时 mod 才彻底孤立
	res2 := m.Isolate([]string{cardB})
	if res2.MovedMods != 1 {
		t.Errorf("第二批后孤立 mod 应被移动，实际移动 %d 个，日志 %v", res2.MovedMods, res2.Logs)
	}
	if _, err := os.Stat(modPath); !os.IsNotExist(err) {
		t.Error("孤立 mod 应已从 mods 目录移走")
	}
}

// TestIsolateMultipleSelectedSharingMod 验证同批次内多选时的判断：
// 若 mod 被本次勾选的两个文件共同引用，且没有第三方引用，则应被移走。
func TestIsolateMultipleSelectedSharingMod(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	cardA := env.femaleCard("a.png")
	cardB := env.femaleCard("b.png")
	writeFile(t, cardA, "a")
	writeFile(t, cardB, "b")

	m.Scanned = true
	m.GlobalDepMap["shared.guid"] = []string{cardA, cardB}
	m.FileDeps[cardA] = []string{"shared.guid"}
	m.FileDeps[cardB] = []string{"shared.guid"}

	modPath := filepath.Join(m.ModsDir(), "shared.zipmod")
	writeFile(t, modPath, "mod")
	m.ZipmodDB["shared.guid"] = modPath

	// 同时勾选两个文件，它们是该 GUID 的全部引用者
	res := m.Isolate([]string{cardA, cardB})
	if res.MovedCards != 2 {
		t.Errorf("应移动 2 个资源，实际 %d", res.MovedCards)
	}
	if res.MovedMods != 1 {
		t.Errorf("无外部引用时 mod 应被移动，实际移动 %d 个，日志 %v", res.MovedMods, res.Logs)
	}
}

// ---------------------------------------------------------------------------
// 恢复
// ---------------------------------------------------------------------------

func TestRestoreCard(t *testing.T) {
	env := newTestEnv(t)

	card := env.femaleCard(filepath.Join("subA", "subB", "card.png"))
	writeFile(t, card, "card")

	m := env.manager
	m.Scanned = true
	m.Isolate([]string{card})

	// 找到隔离区中的文件并恢复
	isolated := filepath.Join(m.DelDir(ResourceKinds[0]), "subA", "subB", "card.png")
	if _, err := os.Stat(isolated); err != nil {
		t.Fatalf("隔离区文件不存在: %v", err)
	}

	res := m.Restore([]string{isolated})
	if res.RestoredCard != 1 {
		t.Fatalf("应恢复 1 个资源，实际 %d，日志: %v", res.RestoredCard, res.Logs)
	}
	// 必须回到原路径
	if _, err := os.Stat(card); err != nil {
		t.Errorf("资源未回到原位置: %v", err)
	}
	// 隔离区的空目录应被清理
	if _, err := os.Stat(filepath.Join(m.DelDir(ResourceKinds[0]), "subA")); !os.IsNotExist(err) {
		t.Error("隔离区空目录应被清理")
	}
}

func TestRestoreZipmod(t *testing.T) {
	env := newTestEnv(t)

	card := env.femaleCard("a.png")
	writeFile(t, card, "card")

	m := env.manager
	m.Scanned = true
	m.GlobalDepMap["lonely.guid"] = []string{card}
	m.FileDeps[card] = []string{"lonely.guid"}
	modPath := filepath.Join(m.ModsDir(), "lonely.zipmod")
	writeFile(t, modPath, "mod")
	m.ZipmodDB["lonely.guid"] = modPath

	m.Isolate([]string{card})

	isolatedMod := filepath.Join(m.DelZipmodDir(), "lonely.zipmod")
	if _, err := os.Stat(isolatedMod); err != nil {
		t.Fatalf("zipmod 未进入隔离区: %v", err)
	}

	res := m.Restore([]string{isolatedMod})
	if res.RestoredMod != 1 {
		t.Fatalf("应恢复 1 个 MOD，实际 %d，日志: %v", res.RestoredMod, res.Logs)
	}
	if _, err := os.Stat(modPath); err != nil {
		t.Errorf("zipmod 未回到 mods 目录: %v", err)
	}
}

func TestRestoreSkipsExistingTarget(t *testing.T) {
	env := newTestEnv(t)

	card := env.femaleCard("a.png")
	writeFile(t, card, "card")

	m := env.manager
	m.Scanned = true
	m.Isolate([]string{card})

	// 原位置已有同名文件
	writeFile(t, card, "new content")

	isolated := filepath.Join(m.DelDir(ResourceKinds[0]), "a.png")
	res := m.Restore([]string{isolated})

	if res.RestoredCard != 0 {
		t.Errorf("目标已存在时不应恢复，实际恢复 %d 个", res.RestoredCard)
	}
	data, err := os.ReadFile(card)
	if err != nil || string(data) != "new content" {
		t.Errorf("原文件不应被覆盖，实际内容 %q", string(data))
	}
}

func TestRestoreZipmodSkipsDuplicateName(t *testing.T) {
	env := newTestEnv(t)

	m := env.manager
	m.Scanned = true

	// 隔离区放一个 zipmod，mods 目录已有同名文件
	isolated := filepath.Join(m.DelZipmodDir(), "same.zipmod")
	writeFile(t, isolated, "isolated")
	writeFile(t, filepath.Join(m.ModsDir(), "same.zipmod"), "existing")

	res := m.Restore([]string{isolated})
	if res.RestoredMod != 0 {
		t.Errorf("mods 中存在同名文件时应跳过，实际恢复 %d 个", res.RestoredMod)
	}
	data, err := os.ReadFile(filepath.Join(m.ModsDir(), "same.zipmod"))
	if err != nil || string(data) != "existing" {
		t.Errorf("mods 中已有文件不应被覆盖，实际内容 %q", string(data))
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func TestIsUnder(t *testing.T) {
	root := filepath.Join("C:", "KKV114")
	if !isUnder(filepath.Join(root, "UserData"), root) {
		t.Error("子目录应判定为在根目录内")
	}
	if !isUnder(root, root) {
		t.Error("根目录本身应判定为在根目录内")
	}
	if isUnder(filepath.Join("C:", "Other"), root) {
		t.Error("无关目录不应判定为在根目录内")
	}
}

func TestMoveFileCreatesParent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	dst := filepath.Join(dir, "x", "y", "a.txt")
	writeFile(t, src, "hello")

	if err := moveFile(src, dst); err != nil {
		t.Fatalf("移动失败: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("源文件应被移走")
	}
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "hello" {
		t.Errorf("目标文件内容不正确: %q", string(data))
	}
}

func TestMoveFileDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	dst := filepath.Join(dir, "b.txt")
	writeFile(t, src, "new")
	writeFile(t, dst, "old")

	if err := moveFile(src, dst); err == nil {
		t.Error("目标已存在时移动应失败")
	}
	data, _ := os.ReadFile(dst)
	if string(data) != "old" {
		t.Errorf("目标文件不应被覆盖，实际 %q", string(data))
	}
}
