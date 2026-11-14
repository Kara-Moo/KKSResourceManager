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
		"DEL-coord_f", "DEL-coord_m",
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
	// v2.4：不再管理 zipmod，因此不再创建 DEL-zipmod 目录
	if _, err := os.Stat(filepath.Join(env.delRoot, "DEL-zipmod")); !os.IsNotExist(err) {
		t.Errorf("DEL-zipmod 不应被创建")
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
	// v2.4 阶段 4：全部冲突 = 已经隔离过，按幂等成功处理（规格 I6/T6），
	// 但不能移动、不能覆盖、不能丢失源文件。
	if !res.Success {
		t.Errorf("目标已存在时应按「已存在」幂等成功处理，日志: %v", res.Logs)
	}
	if res.FailedCount != 0 {
		t.Errorf("已存在不应计为失败，实际 FailedCount=%d", res.FailedCount)
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
