package main

// transfer_test.go —— 阶段 4：文件夹迁移引擎回归测试。
//
// 覆盖规格 §6.6 的不变量与 §6.8 的 T1–T9。
// 最关键的是 T9：先红后绿，锁死「空文件夹只建结构、不搬内容」的旧 bug。

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// listFiles 返回 root 下所有普通文件的相对路径（POSIX），用于对比迁移前后。
func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s 失败: %v", root, err)
	}
	sort.Strings(files)
	return files
}

// assertNotExist 断言路径不存在。
func assertNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("路径不应存在: %s (err=%v)", path, err)
	}
}

// assertFileContent 断言文件内容。
func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("%s 内容应为 %q，实际 %q", path, want, string(data))
	}
}

// ---------------------------------------------------------------------------
// T9：空文件夹（B1 回归，必须先红后绿）
// ---------------------------------------------------------------------------

// TestT9EmptyFolderIsolateRejected 是 B1 的回归测试。
//
// 旧实现：collectFilesStrict 收到 0 个卡片 → 当成空文件夹 →
// os.MkdirAll(DEL/.../空文件夹) → 计为成功（history 里 affected=0 committed）。
// 期望：明确报错，且 DEL 侧不产生任何目录。
func TestT9EmptyFolderIsolateRejected(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager
	m.Scanned = true

	empty := env.femaleCard("emptyGroup")
	if err := os.MkdirAll(empty, 0755); err != nil {
		t.Fatalf("创建空文件夹失败: %v", err)
	}

	res := m.Isolate([]string{empty})
	if res.Success {
		t.Fatalf("空文件夹隔离应失败，实际 Success=true，日志: %v", res.Logs)
	}
	if res.FailedCount == 0 {
		t.Errorf("空文件夹隔离应计入失败项，实际 FailedCount=0，日志: %v", res.Logs)
	}

	dstDir := filepath.Join(m.DelDir(ResourceKinds[0]), "emptyGroup")
	if _, err := os.Stat(dstDir); !os.IsNotExist(err) {
		t.Fatalf("DEL 侧不得为空文件夹创建目录 %s（err=%v）", dstDir, err)
	}
}

// TestT9EmptyFolderRestoreRejected 对称验证恢复方向。
func TestT9EmptyFolderRestoreRejected(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	empty := filepath.Join(m.DelDir(ResourceKinds[0]), "emptyGroup")
	if err := os.MkdirAll(empty, 0755); err != nil {
		t.Fatal(err)
	}

	res := m.Restore([]string{empty})
	if res.Success {
		t.Fatalf("隔离区空文件夹恢复应失败，实际 Success=true，日志: %v", res.Logs)
	}
	assertNotExist(t, env.femaleCard("emptyGroup"))
}

// ---------------------------------------------------------------------------
// T1 / T2：整棵子树迁移、空子目录保留
// ---------------------------------------------------------------------------

// TestT1FolderTransferMovesAllAndCleansSource 验证 3+2 个文件全部迁移、
// 目录结构一致、源侧空壳被清理（I4/I5）。
func TestT1FolderTransferMovesAllAndCleansSource(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager
	m.Scanned = true

	folder := env.femaleCard("group")
	writeFile(t, filepath.Join(folder, "a.png"), "a")
	writeFile(t, filepath.Join(folder, "b.png"), "b")
	writeFile(t, filepath.Join(folder, "c.png"), "ccc")
	writeFile(t, filepath.Join(folder, "sub", "d.png"), "dd")
	writeFile(t, filepath.Join(folder, "sub", "e.png"), "eee")

	res := m.Isolate([]string{folder})
	if !res.Success || res.MovedCards != 5 {
		t.Fatalf("应迁移 5 个文件，实际 moved=%d success=%v，日志: %v",
			res.MovedCards, res.Success, res.Logs)
	}

	base := filepath.Join(m.DelDir(ResourceKinds[0]), "group")
	if got := listFiles(t, base); len(got) != 5 {
		t.Fatalf("DEL 侧应有 5 个文件，实际 %v", got)
	}
	assertFileContent(t, filepath.Join(base, "sub", "d.png"), "dd")
	assertFileContent(t, filepath.Join(base, "sub", "e.png"), "eee")

	// I4：源侧该子树内文件数为 0，空壳被清理，父目录保留
	assertNotExist(t, folder)
	if _, err := os.Stat(filepath.Dir(folder)); err != nil {
		t.Errorf("父目录（资源根）必须保留: %v", err)
	}
}

// TestT2EmptySubdirPreserved 验证非空文件夹里的空子目录被保留。
func TestT2EmptySubdirPreserved(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager
	m.Scanned = true

	folder := env.femaleCard("group")
	writeFile(t, filepath.Join(folder, "a.png"), "a")
	if err := os.MkdirAll(filepath.Join(folder, "emptySub"), 0755); err != nil {
		t.Fatal(err)
	}

	dstDir := filepath.Join(m.DelDir(ResourceKinds[0]), "group")
	plan, err := m.planFolderTransfer(folder, dstDir, m.SourceDir(ResourceKinds[0]))
	if err != nil {
		t.Fatalf("规划失败: %v", err)
	}
	if plan.Dirs != 2 { // group 本身 + emptySub
		t.Errorf("Dirs 应为 2，实际 %d", plan.Dirs)
	}
	// I1：规划阶段零副作用
	assertNotExist(t, dstDir)

	res := m.Isolate([]string{folder})
	if !res.Success || res.MovedCards != 1 {
		t.Fatalf("应迁移 1 个文件，实际 moved=%d，日志: %v", res.MovedCards, res.Logs)
	}
	if info, err := os.Stat(filepath.Join(dstDir, "emptySub")); err != nil || !info.IsDir() {
		t.Errorf("DEL 侧应保留空子目录 emptySub: %v", err)
	}
}

// ---------------------------------------------------------------------------
// T3：混合内容（B2 回归）
// ---------------------------------------------------------------------------

// TestT3MixedContentMovesAll 验证非卡片文件不会被静默丢弃。
func TestT3MixedContentMovesAll(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager
	m.Scanned = true

	folder := env.femaleCard("mixed")
	writeFile(t, filepath.Join(folder, "a.png"), "png")
	writeFile(t, filepath.Join(folder, "note.txt"), "txt")
	writeFile(t, filepath.Join(folder, "addon.zipmod"), "zipmod")
	writeFile(t, filepath.Join(folder, "backup.bak"), "bak")

	res := m.Isolate([]string{folder})
	if !res.Success || res.MovedCards != 4 {
		t.Fatalf("4 个文件都应迁走，实际 moved=%d，日志: %v", res.MovedCards, res.Logs)
	}

	base := filepath.Join(m.DelDir(ResourceKinds[0]), "mixed")
	for name, content := range map[string]string{
		"a.png":        "png",
		"note.txt":     "txt",
		"addon.zipmod": "zipmod",
		"backup.bak":   "bak",
	} {
		assertFileContent(t, filepath.Join(base, name), content)
	}
	assertNotExist(t, folder)
}

// ---------------------------------------------------------------------------
// T4：目标冲突（整批中止，零改动）
// ---------------------------------------------------------------------------

// TestT4ConflictAbortsWithoutChanges 验证部分冲突时整批中止。
func TestT4ConflictAbortsWithoutChanges(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager
	m.Scanned = true

	folder := env.femaleCard("group")
	writeFile(t, filepath.Join(folder, "a.png"), "new-a")
	writeFile(t, filepath.Join(folder, "b.png"), "new-b")

	delDir := m.DelDir(ResourceKinds[0])
	writeFile(t, filepath.Join(delDir, "group", "a.png"), "existing")

	res := m.Isolate([]string{folder})
	if res.Success || res.FailedCount == 0 {
		t.Fatalf("部分冲突应整批失败，实际 success=%v failed=%d", res.Success, res.FailedCount)
	}
	if res.MovedCards != 0 {
		t.Errorf("冲突时不得移动任何文件，实际 %d", res.MovedCards)
	}

	// 源侧零改动
	assertFileContent(t, filepath.Join(folder, "a.png"), "new-a")
	assertFileContent(t, filepath.Join(folder, "b.png"), "new-b")
	// 目标侧不覆盖、不新增
	assertFileContent(t, filepath.Join(delDir, "group", "a.png"), "existing")
	assertNotExist(t, filepath.Join(delDir, "group", "b.png"))
}

// ---------------------------------------------------------------------------
// T5：中途失败 → 回滚文件并回收已建目录（B3 回归）
// ---------------------------------------------------------------------------

// TestT5FailureRollsBackFilesAndDirs 在规划后删除一个源文件，制造执行期失败。
func TestT5FailureRollsBackFilesAndDirs(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager
	m.Scanned = true

	folder := env.femaleCard("group")
	writeFile(t, filepath.Join(folder, "a.png"), "a")
	writeFile(t, filepath.Join(folder, "b.png"), "b")
	writeFile(t, filepath.Join(folder, "c.png"), "c")

	dstDir := filepath.Join(m.DelDir(ResourceKinds[0]), "group")
	plan, err := m.planFolderTransfer(folder, dstDir, m.SourceDir(ResourceKinds[0]))
	if err != nil {
		t.Fatalf("规划失败: %v", err)
	}

	// 规划完成后删掉最后一个文件 → 执行到它时 stat 失败
	if err := os.Remove(filepath.Join(folder, "c.png")); err != nil {
		t.Fatal(err)
	}

	txn, err := m.beginTx("test", "rollback")
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	moved, _, _, execErr := m.execTransferPlan(txn, plan, nil)
	if execErr == nil {
		t.Fatal("应因源文件消失而失败")
	}
	if moved == 0 {
		t.Errorf("失败前应已移动部分文件（用于验证回滚），实际 0")
	}

	// I3：源侧已移动的文件全部回来
	assertFileContent(t, filepath.Join(folder, "a.png"), "a")
	assertFileContent(t, filepath.Join(folder, "b.png"), "b")
	// 目标侧不得留下任何新建目录
	assertNotExist(t, dstDir)
}

// ---------------------------------------------------------------------------
// T6：重复隔离（全部冲突 = 幂等成功，不动盘）
// ---------------------------------------------------------------------------

// TestT6RepeatIsolateIsIdempotent 验证重复隔离返回「已存在」而不是失败。
func TestT6RepeatIsolateIsIdempotent(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager
	m.Scanned = true

	folder := env.femaleCard("group")
	writeFile(t, filepath.Join(folder, "a.png"), "a")
	writeFile(t, filepath.Join(folder, "sub", "b.png"), "b")

	first := m.Isolate([]string{folder})
	if !first.Success || first.MovedCards != 2 {
		t.Fatalf("首次隔离应成功，实际 %+v", first)
	}

	// 源侧已被清理，重建同样的源文件夹（内容与 DEL 侧同名）
	writeFile(t, filepath.Join(folder, "a.png"), "a2")
	writeFile(t, filepath.Join(folder, "sub", "b.png"), "b2")

	second := m.Isolate([]string{folder})
	if !second.Success {
		t.Fatalf("重复隔离应按已存在处理（Success=true），日志: %v", second.Logs)
	}
	if second.MovedCards != 0 {
		t.Errorf("重复隔离不应移动文件，实际 %d", second.MovedCards)
	}
	// 不能覆盖 DEL 侧已有内容
	assertFileContent(t, filepath.Join(m.DelDir(ResourceKinds[0]), "group", "a.png"), "a")
	assertFileContent(t, filepath.Join(m.DelDir(ResourceKinds[0]), "group", "sub", "b.png"), "b")
}

// ---------------------------------------------------------------------------
// T7 / T8：多选与父子重叠
// ---------------------------------------------------------------------------

// TestT7MultipleFoldersMovedOnce 验证多选 3 个互不包含的文件夹。
func TestT7MultipleFoldersMovedOnce(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager
	m.Scanned = true

	var folders []string
	for _, name := range []string{"f1", "f2", "f3"} {
		folder := env.femaleCard(name)
		writeFile(t, filepath.Join(folder, "a.png"), name+"-a")
		writeFile(t, filepath.Join(folder, "sub", "b.png"), name+"-b")
		folders = append(folders, folder)
	}

	res := m.Isolate(folders)
	if !res.Success || res.MovedCards != 6 {
		t.Fatalf("应迁移 6 个文件，实际 moved=%d，日志: %v", res.MovedCards, res.Logs)
	}
	for _, name := range []string{"f1", "f2", "f3"} {
		assertNotExist(t, env.femaleCard(name))
		assertFileContent(t,
			filepath.Join(m.DelDir(ResourceKinds[0]), name, "sub", "b.png"), name+"-b")
	}
}

// TestT8OverlappingSelectionProcessedOnce 验证父子同时选中时只迁一次。
func TestT8OverlappingSelectionProcessedOnce(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager
	m.Scanned = true

	a := env.femaleCard("A")
	b := filepath.Join(a, "B")
	writeFile(t, filepath.Join(a, "a.png"), "a")
	writeFile(t, filepath.Join(b, "b.png"), "b")

	kept := dedupeSelectionPaths([]string{a, b})
	if len(kept) != 1 || !samePath(kept[0], a) {
		t.Fatalf("父子重叠应只保留祖先 A，实际 %v", kept)
	}

	res := m.Isolate([]string{a, b})
	if !res.Success || res.MovedCards != 2 {
		t.Fatalf("应恰好迁移 2 个文件（不重复），实际 moved=%d，日志: %v",
			res.MovedCards, res.Logs)
	}
	assertFileContent(t, filepath.Join(m.DelDir(ResourceKinds[0]), "A", "B", "b.png"), "b")
}

// ---------------------------------------------------------------------------
// 恢复方向：非卡片文件 + 源侧清理
// ---------------------------------------------------------------------------

// TestRestoreFolderMovesNonCardFiles 验证恢复同样搬走整个文件夹内容。
func TestRestoreFolderMovesNonCardFiles(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager
	m.Scanned = true

	folder := env.femaleCard("mixed")
	writeFile(t, filepath.Join(folder, "a.png"), "png")
	writeFile(t, filepath.Join(folder, "note.txt"), "txt")

	if res := m.Isolate([]string{folder}); !res.Success {
		t.Fatalf("隔离失败: %v", res.Logs)
	}

	delFolder := filepath.Join(m.DelDir(ResourceKinds[0]), "mixed")
	res := m.Restore([]string{delFolder})
	if !res.Success || res.RestoredCard != 2 {
		t.Fatalf("应恢复 2 个文件，实际 %d，日志: %v", res.RestoredCard, res.Logs)
	}
	assertFileContent(t, filepath.Join(folder, "a.png"), "png")
	assertFileContent(t, filepath.Join(folder, "note.txt"), "txt")
	// DEL 侧空壳应被清理
	assertNotExist(t, delFolder)
}

// TestUndoFolderIsolationRestoresEverything 验证历史撤销能处理
// mkdir/move/rmdir 三种步骤（目录型事务）。
func TestUndoFolderIsolationRestoresEverything(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager
	m.Scanned = true

	folder := env.femaleCard("group")
	writeFile(t, filepath.Join(folder, "a.png"), "a")
	writeFile(t, filepath.Join(folder, "sub", "b.png"), "b")

	if res := m.Isolate([]string{folder}); !res.Success {
		t.Fatalf("隔离失败: %v", res.Logs)
	}

	history := m.GetHistory(0, 1)
	if len(history) != 1 {
		t.Fatalf("应有 1 条历史记录，实际 %d", len(history))
	}
	undo, err := m.CmdUndo(history[0].TxID)
	if err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if !undo.OK {
		t.Fatalf("撤销未成功: %+v", undo)
	}

	// 文件回到原位（含子目录）
	assertFileContent(t, filepath.Join(folder, "a.png"), "a")
	assertFileContent(t, filepath.Join(folder, "sub", "b.png"), "b")
	// DEL 侧新建的目录被回收
	assertNotExist(t, filepath.Join(m.DelDir(ResourceKinds[0]), "group"))
}
