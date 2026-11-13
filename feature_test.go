package main

// feature_test.go —— 新增功能的单元测试
//
// 覆盖：文件夹整体隔离/恢复、隐藏空文件夹、路径重建、复制剪切粘贴、
// zipmod 依赖保护。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 文件夹整体隔离 / 恢复
// ---------------------------------------------------------------------------

func TestIsolateFolderKeepsStructure(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	// 构造 group/sub/a.png 与 group/b.png
	folder := filepath.Join(env.femaleCard("group"))
	writeFile(t, filepath.Join(folder, "sub", "a.png"), "a")
	writeFile(t, filepath.Join(folder, "b.png"), "b")

	m.Scanned = true
	res := m.Isolate([]string{folder})

	if res.MovedCards != 2 {
		t.Fatalf("应移动 2 个文件，实际 %d，日志 %v", res.MovedCards, res.Logs)
	}

	// 隔离区必须保留 group/sub/a.png 与 group/b.png 的层级
	base := m.DelDir(ResourceKinds[0])
	for _, rel := range []string{
		filepath.Join("group", "sub", "a.png"),
		filepath.Join("group", "b.png"),
	} {
		p := filepath.Join(base, rel)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("隔离区未保留目录结构 %s: %v", rel, err)
		}
	}
}

func TestRestoreFolderKeepsStructure(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	folder := filepath.Join(env.femaleCard("group"))
	writeFile(t, filepath.Join(folder, "sub", "a.png"), "a")
	writeFile(t, filepath.Join(folder, "b.png"), "b")

	m.Scanned = true
	m.Isolate([]string{folder})

	// 恢复整个文件夹
	delFolder := filepath.Join(m.DelDir(ResourceKinds[0]), "group")
	res := m.Restore([]string{delFolder})
	if res.RestoredCard != 2 {
		t.Fatalf("应恢复 2 个文件，实际 %d，日志 %v", res.RestoredCard, res.Logs)
	}

	// 原路径必须完全还原
	for _, rel := range []string{
		filepath.Join("group", "sub", "a.png"),
		filepath.Join("group", "b.png"),
	} {
		p := filepath.Join(env.femaleCard(""), rel)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("未恢复到原位置 %s: %v", p, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 隐藏空文件夹
// ---------------------------------------------------------------------------

func TestHideEmptyFolder(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	// 空文件夹
	empty := filepath.Join(env.femaleCard("emptyFolder"))
	writeFile(t, filepath.Join(empty, "keep.txt"), "") // 占位，稍后删
	if err := os.Remove(filepath.Join(empty, "keep.txt")); err != nil {
		t.Fatalf("准备空文件夹失败: %v", err)
	}

	res := m.invisible.HideFolders([]string{empty}, SideKKS)
	if res.Hidden != 1 {
		t.Fatalf("应隐藏 1 个文件夹，实际 %d，日志 %v", res.Hidden, res.Logs)
	}
	// 源文件夹必须已删除
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Error("隐藏后源文件夹应被删除")
	}
	// 状态文件必须存在
	stateFile := filepath.Join(m.DelCardsRoot, invisibleDirName, fileKKSinvisible)
	if _, err := os.Stat(stateFile); err != nil {
		t.Errorf("隐藏状态文件未创建: %v", err)
	}
	// 列表中应能读到
	folders, err := m.invisible.LoadKKSFolders()
	if err != nil {
		t.Fatalf("读取隐藏列表失败: %v", err)
	}
	if len(folders) != 1 {
		t.Errorf("应记录 1 个隐藏文件夹，实际 %d", len(folders))
	}
}

func TestHideNonEmptyFolderRejected(t *testing.T) {
	env := newTestEnv(t)

	notEmpty := filepath.Join(env.femaleCard("withFiles"))
	writeFile(t, filepath.Join(notEmpty, "a.png"), "x")

	res := env.manager.invisible.HideFolders([]string{notEmpty}, SideKKS)
	if res.Hidden != 0 {
		t.Errorf("非空文件夹不应被隐藏，实际隐藏 %d 个", res.Hidden)
	}
	if res.Skipped != 1 {
		t.Errorf("应记录 1 个跳过项，实际 %d", res.Skipped)
	}
	// 日志必须包含提示语
	found := false
	for _, l := range res.Logs {
		if strings.Contains(l.Message, "无法删除，不是空文件") {
			found = true
		}
	}
	if !found {
		t.Errorf("日志应包含「无法删除，不是空文件」提示，实际: %v", res.Logs)
	}
	// 文件夹必须还在
	if _, err := os.Stat(notEmpty); err != nil {
		t.Error("非空文件夹不应被删除")
	}
}

func TestHideAndRestoreFolder(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	// 含空子目录的文件夹
	target := filepath.Join(env.femaleCard("tree"))
	writeFile(t, filepath.Join(target, "sub1", "sub2", "x"), "")
	writeFile(t, filepath.Join(target, "sub1", "y"), "")

	// 清空文件使其成为「空文件夹树」
	os.RemoveAll(filepath.Join(target, "sub1", "sub2", "x"))
	os.RemoveAll(filepath.Join(target, "sub1", "y"))

	res := m.invisible.HideFolders([]string{target}, SideKKS)
	if res.Hidden != 1 {
		t.Fatalf("应隐藏 1 个文件夹，实际 %d，日志 %v", res.Hidden, res.Logs)
	}

	// 恢复
	folders, _ := m.invisible.LoadKKSFolders()
	if len(folders) != 1 {
		t.Fatalf("应有 1 条隐藏记录，实际 %d", len(folders))
	}
	restoreRes := m.invisible.RestoreFolders(folders)
	if restoreRes.Restored != 1 {
		t.Fatalf("应恢复 1 个文件夹，实际 %d，日志 %v", restoreRes.Restored, restoreRes.Logs)
	}

	// 子目录结构必须重建
	for _, rel := range []string{"sub1", filepath.Join("sub1", "sub2")} {
		p := filepath.Join(target, rel)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("恢复后子目录 %s 不存在: %v", rel, err)
		}
	}
	// 隐藏列表应清空
	folders, _ = m.invisible.LoadKKSFolders()
	if len(folders) != 0 {
		t.Errorf("恢复后隐藏列表应为空，实际 %d 项", len(folders))
	}
}

func TestInvisibleStateFilesLocation(t *testing.T) {
	env := newTestEnv(t)

	// 四个状态文件都应位于 DELCARDS/DEL-invisible 下
	base := filepath.Join(env.delRoot, invisibleDirName)
	for _, name := range []string{fileKKSinvisible, fileDELinvisible, fileOriginKKS, fileOriginDEL} {
		p := filepath.Join(base, name)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("状态文件 %s 不存在: %v", name, err)
		}
	}
	// 隐藏目录本身不能被空目录清理掉
	env.manager.cleanEmptyDirs(env.delRoot)
	if _, err := os.Stat(base); err != nil {
		t.Error("DEL-invisible 目录不应被清理")
	}
}

// ---------------------------------------------------------------------------
// 路径重建
// ---------------------------------------------------------------------------

func TestRestoreRebuildsHiddenParent(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	// 父目录 empty/sub 先被隐藏
	parent := filepath.Join(env.femaleCard("empty"))
	writeFile(t, filepath.Join(parent, "sub", "keep"), "")
	os.RemoveAll(filepath.Join(parent, "sub", "keep"))

	if res := m.invisible.HideFolders([]string{parent}, SideKKS); res.Hidden != 1 {
		t.Fatalf("隐藏失败: %v", res.Logs)
	}

	// 现在放一个文件到已被隐藏的目录下（模拟用户手动放回）
	if err := os.MkdirAll(filepath.Join(parent, "sub"), 0755); err != nil {
		t.Fatalf("准备失败: %v", err)
	}
	writeFile(t, filepath.Join(parent, "sub", "card.png"), "card")

	// 隔离该文件 —— 目标父目录在 DEL 侧是新建的，应成功
	m.Scanned = true
	card := filepath.Join(parent, "sub", "card.png")
	res := m.Isolate([]string{card})
	if res.MovedCards != 1 {
		t.Fatalf("应移动 1 个文件，实际 %d，日志 %v", res.MovedCards, res.Logs)
	}
}

func TestEnsurePathForOperationRebuilds(t *testing.T) {
	env := newTestEnv(t)
	inv := env.manager.invisible

	parent := filepath.Join(env.femaleCard("hidden"))
	writeFile(t, filepath.Join(parent, "sub"), "")
	os.RemoveAll(filepath.Join(parent, "sub"))

	if res := inv.HideFolders([]string{parent}, SideKKS); res.Hidden != 1 {
		t.Fatalf("隐藏失败: %v", res.Logs)
	}

	// 目标文件的父目录已被删除
	target := filepath.Join(parent, "sub", "card.png")
	rebuilt, err := inv.EnsurePathForOperation(target)
	if err != nil {
		t.Fatalf("路径重建失败: %v", err)
	}
	if !rebuilt {
		t.Error("应报告发生了重建")
	}
	if _, err := os.Stat(filepath.Dir(target)); err != nil {
		t.Errorf("父目录未被重建: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 复制 / 剪切 / 粘贴
// ---------------------------------------------------------------------------

func TestPasteCopyCreatesDuplicate(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	src := env.femaleCard("a.png")
	writeFile(t, src, "hello")

	dstDir := filepath.Join(env.femaleCard("target"))
	writeFile(t, filepath.Join(dstDir, "x"), "")

	m.clipboard.Set(ClipboardCopy, []string{src}, SideKKS, ModeFemaleCard)
	res := m.Paste(dstDir, SideKKS, ModeFemaleCard)

	if res.FailedCount != 0 {
		t.Fatalf("复制应成功，日志 %v", res.Logs)
	}
	// 源保留
	if _, err := os.Stat(src); err != nil {
		t.Error("复制后源文件应保留")
	}
	// 目标存在
	copied := filepath.Join(dstDir, "a.png")
	if _, err := os.Stat(copied); err != nil {
		t.Errorf("目标文件未生成: %v", err)
	}
}

func TestPasteConflictCancelsWholeOperation(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	src1 := env.femaleCard("a.png")
	src2 := env.femaleCard("b.png")
	writeFile(t, src1, "1")
	writeFile(t, src2, "2")

	dstDir := filepath.Join(env.femaleCard("target"))
	writeFile(t, filepath.Join(dstDir, "b.png"), "existing")

	m.clipboard.Set(ClipboardCopy, []string{src1, src2}, SideKKS, ModeFemaleCard)
	res := m.Paste(dstDir, SideKKS, ModeFemaleCard)

	if res.FailedCount == 0 {
		t.Error("存在同名冲突时应报失败")
	}
	// 整个操作必须取消：a.png 也不应被复制
	if _, err := os.Stat(filepath.Join(dstDir, "a.png")); err == nil {
		t.Error("冲突时整个复制操作应被取消")
	}
	// 已有文件不得被覆盖
	data, _ := os.ReadFile(filepath.Join(dstDir, "b.png"))
	if string(data) != "existing" {
		t.Errorf("已有文件不应被覆盖，实际 %q", string(data))
	}
	// 日志应包含「无法复制」
	found := false
	for _, l := range res.Logs {
		if strings.Contains(l.Message, "无法复制") {
			found = true
		}
	}
	if !found {
		t.Errorf("日志应包含「无法复制」，实际 %v", res.Logs)
	}
}

func TestPasteCutMovesSource(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	src := env.femaleCard("a.png")
	writeFile(t, src, "hello")
	dstDir := filepath.Join(env.femaleCard("target"))
	writeFile(t, filepath.Join(dstDir, "x"), "")

	m.clipboard.Set(ClipboardCut, []string{src}, SideKKS, ModeFemaleCard)
	res := m.Paste(dstDir, SideKKS, ModeFemaleCard)

	if res.FailedCount != 0 {
		t.Fatalf("剪切应成功，日志 %v", res.Logs)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("剪切后源文件应被移除")
	}
	if _, err := os.Stat(filepath.Join(dstDir, "a.png")); err != nil {
		t.Errorf("目标文件未生成: %v", err)
	}
	// 剪切完成后剪贴板应清空
	if !m.clipboard.Get().IsEmpty() {
		t.Error("剪切完成后剪贴板应清空")
	}
}

func TestPasteZipmodBlockedWhenReferenced(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	// 一个 zipmod 被某个资源引用
	modPath := filepath.Join(m.ModsDir(), "used.zipmod")
	makeZipmod(t, modPath, "used.guid")
	m.ZipmodDB["used.guid"] = modPath

	card := env.femaleCard("a.png")
	writeFile(t, card, "card")
	m.GlobalDepMap["used.guid"] = []string{card}

	// 粘贴到隔离区 —— 应被依赖检查拦截
	dstDir := m.DelZipmodDir()
	m.clipboard.Set(ClipboardCopy, []string{modPath}, SideKKS, ModeZipmod)
	res := m.Paste(dstDir, SideDEL, ModeZipmod)

	if res.FailedCount == 0 {
		t.Error("仍被引用的 zipmod 不应允许移出")
	}
	// zipmod 必须仍在原处
	if _, err := os.Stat(modPath); err != nil {
		t.Error("被拦截后 zipmod 不应被移动")
	}
}

func TestPasteZipmodAllowedWhenOrphan(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	modPath := filepath.Join(m.ModsDir(), "orphan.zipmod")
	makeZipmod(t, modPath, "orphan.guid")
	m.ZipmodDB["orphan.guid"] = modPath

	dstDir := m.DelZipmodDir()
	m.clipboard.Set(ClipboardCopy, []string{modPath}, SideKKS, ModeZipmod)
	res := m.Paste(dstDir, SideDEL, ModeZipmod)

	if res.FailedCount != 0 {
		t.Fatalf("孤立 zipmod 应允许移出，日志 %v", res.Logs)
	}
	// zipmod 的复制等价于「隔离」：源必须移走，否则等于没隔离
	if _, err := os.Stat(modPath); !os.IsNotExist(err) {
		t.Error("孤立 zipmod 应从 mods 目录移走")
	}
	if _, err := os.Stat(filepath.Join(dstDir, "orphan.zipmod")); err != nil {
		t.Errorf("zipmod 未进入隔离区: %v", err)
	}
}

// TestPasteZipmodFromDelToModsEnables 验证从隔离区把 zipmod 移回 mods。
func TestPasteZipmodFromDelToModsEnables(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	// 模拟已在隔离区的 zipmod
	delPath := filepath.Join(m.DelZipmodDir(), "back.zipmod")
	makeZipmod(t, delPath, "back.guid")

	m.clipboard.Set(ClipboardCopy, []string{delPath}, SideDEL, ModeZipmod)
	res := m.Paste(m.ModsDir(), SideKKS, ModeZipmod)

	if res.FailedCount != 0 {
		t.Fatalf("应能启用 zipmod，日志 %v", res.Logs)
	}
	if _, err := os.Stat(filepath.Join(m.ModsDir(), "back.zipmod")); err != nil {
		t.Errorf("zipmod 未回到 mods 目录: %v", err)
	}
	if _, err := os.Stat(delPath); !os.IsNotExist(err) {
		t.Error("启用后隔离区不应残留 zipmod")
	}
}

// TestPasteZipmodWithoutGUIDIsBlocked 验证无法识别 GUID 的 zipmod 不会被移走。
//
// 这是一条安全底线：GUID 解析失败意味着无法确认依赖归属，
// 此时必须保守处理，绝不能让 zipmod 脱离 mods 目录。
func TestPasteZipmodWithoutGUIDIsBlocked(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	// 不是合法 zip 的 .zipmod 文件
	modPath := filepath.Join(m.ModsDir(), "broken.zipmod")
	writeFile(t, modPath, "not a real zipmod")

	dstDir := m.DelZipmodDir()
	m.clipboard.Set(ClipboardCopy, []string{modPath}, SideKKS, ModeZipmod)
	res := m.Paste(dstDir, SideDEL, ModeZipmod)

	if res.FailedCount == 0 {
		t.Error("无法识别 GUID 的 zipmod 不应允许移出")
	}
	if _, err := os.Stat(modPath); err != nil {
		t.Error("被拦截后 zipmod 不应被移动")
	}
}

func TestCopyFolderTree(t *testing.T) {
	env := newTestEnv(t)

	src := filepath.Join(env.femaleCard("tree"))
	writeFile(t, filepath.Join(src, "sub", "a.png"), "a")
	writeFile(t, filepath.Join(src, "b.png"), "b")

	dst := filepath.Join(env.femaleCard("copy"))
	if err := copyPath(src, dst); err != nil {
		t.Fatalf("复制目录失败: %v", err)
	}

	for _, rel := range []string{filepath.Join("sub", "a.png"), "b.png"} {
		if _, err := os.Stat(filepath.Join(dst, rel)); err != nil {
			t.Errorf("复制的目录缺少 %s: %v", rel, err)
		}
	}
	// 源必须保留
	if _, err := os.Stat(src); err != nil {
		t.Error("复制目录后源应保留")
	}
}
