package tx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestRoot 构造一个测试用的 DELCARDS 根目录。
func newTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, StateDirName), 0755); err != nil {
		t.Fatalf("创建状态目录失败: %v", err)
	}
	return root
}

// TestTxMoveAndRollback 移动后回滚应恢复原状。
func TestTxMoveAndRollback(t *testing.T) {
	root := newTestRoot(t)
	src := filepath.Join(t.TempDir(), "a.txt")
	dst := filepath.Join(t.TempDir(), "b.txt")
	if err := os.WriteFile(src, []byte("hello"), 0644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	txn, err := Begin(root, "paste", "测试移动")
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	if err := txn.Move(src, dst); err != nil {
		t.Fatalf("移动失败: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("移动后源应不存在")
	}
	if _, err := os.Stat(dst); err != nil {
		t.Error("移动后目标应存在")
	}

	if err := txn.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	// 源恢复
	if _, err := os.Stat(src); err != nil {
		t.Errorf("回滚后源应恢复: %v", err)
	}
	// 目标消失
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("回滚后目标应消失")
	}
}

// TestTxMoveAndCommit 提交后 journal 被删除，历史有记录。
func TestTxMoveAndCommit(t *testing.T) {
	root := newTestRoot(t)
	src := filepath.Join(t.TempDir(), "a.txt")
	dst := filepath.Join(t.TempDir(), "b.txt")
	os.WriteFile(src, []byte("hello"), 0644)

	txn, err := Begin(root, "paste", "测试提交")
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	if err := txn.Move(src, dst); err != nil {
		t.Fatalf("移动失败: %v", err)
	}
	if err := txn.Commit(root); err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	// journal 目录应被删除
	journalDir := filepath.Join(root, JournalDirName, txn.ID)
	if _, err := os.Stat(journalDir); !os.IsNotExist(err) {
		t.Error("提交后 journal 目录应被删除")
	}
	// 历史应有 1 条
	list, err := LoadHistory(root)
	if err != nil {
		t.Fatalf("读取历史失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("应有 1 条历史，实际 %d", len(list))
	}
	if list[0].Status != "committed" {
		t.Errorf("状态应为 committed，实际 %s", list[0].Status)
	}
	if list[0].Affected != 1 {
		t.Errorf("影响数应为 1，实际 %d", list[0].Affected)
	}
}

// TestTxCopyRollbackDeletesCopy 复制的回滚应删除副本、保留源。
func TestTxCopyRollbackDeletesCopy(t *testing.T) {
	root := newTestRoot(t)
	src := filepath.Join(t.TempDir(), "a.txt")
	dst := filepath.Join(t.TempDir(), "b.txt")
	os.WriteFile(src, []byte("hello"), 0644)

	txn, _ := Begin(root, "copy", "测试复制")
	if err := txn.Copy(src, dst); err != nil {
		t.Fatalf("复制失败: %v", err)
	}
	if err := txn.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Error("回滚后源应保留")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("回滚后副本应被删除")
	}
}

// TestTxMkdirRollback 目录创建的回滚应删掉空目录。
func TestTxMkdirRollback(t *testing.T) {
	root := newTestRoot(t)
	dir := filepath.Join(t.TempDir(), "newdir")

	txn, _ := Begin(root, "mkdir", "测试建目录")
	if err := txn.Mkdir(dir); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Error("目录应存在")
	}
	if err := txn.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("回滚后目录应被删除")
	}
}

// TestTxRenameRollback 重命名的回滚应改回原名。
func TestTxRenameRollback(t *testing.T) {
	root := newTestRoot(t)
	src := filepath.Join(t.TempDir(), "old.txt")
	dst := filepath.Join(t.TempDir(), "new.txt")
	os.WriteFile(src, []byte("hello"), 0644)

	txn, _ := Begin(root, "rename", "测试重命名")
	if err := txn.Rename(src, dst); err != nil {
		t.Fatalf("重命名失败: %v", err)
	}
	if err := txn.Rollback(); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Error("回滚后原名应恢复")
	}
}

// TestTxIntentPersistedBeforeExecution 意图必须先落盘。
func TestTxIntentPersistedBeforeExecution(t *testing.T) {
	root := newTestRoot(t)
	dir := filepath.Join(t.TempDir(), "crash")

	// 开启事务后直接模拟进程被杀（不 Commit）
	txn, err := Begin(root, "paste", "模拟崩溃")
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	if err := txn.Move(filepath.Join(dir, "nonexist"), filepath.Join(dir, "x")); err == nil {
		t.Error("移动不存在的源应报错")
	}

	// journal 应存在（因为 Begin 就落盘了）
	journal := filepath.Join(root, JournalDirName, txn.ID, "journal.json")
	if _, err := os.Stat(journal); err != nil {
		t.Errorf("journal 应存在: %v", err)
	}
}

// TestRecoverPending 崩溃恢复应自动回滚未完成事务。
func TestRecoverPending(t *testing.T) {
	root := newTestRoot(t)
	src := filepath.Join(t.TempDir(), "a.txt")
	dst := filepath.Join(t.TempDir(), "b.txt")
	os.WriteFile(src, []byte("hello"), 0644)

	// 开启事务 + 移动，但不提交（模拟崩溃）
	txn, _ := Begin(root, "paste", "崩溃模拟")
	if err := txn.Move(src, dst); err != nil {
		t.Fatalf("移动失败: %v", err)
	}

	// 模拟进程重启：加载 pending
	pending, err := LoadPending(root)
	if err != nil {
		t.Fatalf("加载 pending 失败: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("应有 1 个未完成事务，实际 %d", len(pending))
	}
	if pending[0].ID != txn.ID {
		t.Errorf("事务 ID 不匹配")
	}

	// 自动恢复
	recovered, errs := RecoverPending(root)
	if recovered != 1 {
		t.Errorf("应恢复 1 个事务，实际 %d，错误 %v", recovered, errs)
	}
	if len(errs) > 0 {
		t.Errorf("不应有错误: %v", errs)
	}
	// 源应恢复
	if _, err := os.Stat(src); err != nil {
		t.Errorf("崩溃恢复后源应还原: %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("崩溃恢复后目标应消失")
	}
}

// TestUndoRejectsExternallyModified 外部修改过目标文件时应拒绝撤销。
func TestUndoRejectsExternallyModified(t *testing.T) {
	root := newTestRoot(t)
	src := filepath.Join(t.TempDir(), "a.txt")
	dst := filepath.Join(t.TempDir(), "b.txt")
	os.WriteFile(src, []byte("hello"), 0644)

	txn, _ := Begin(root, "paste", "撤销测试")
	if err := txn.Move(src, dst); err != nil {
		t.Fatalf("移动失败: %v", err)
	}
	if err := txn.Commit(root); err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	// 外部修改目标文件
	os.WriteFile(dst, []byte("modified by user"), 0644)

	_, err := Undo(root, txn.ID)
	if err == nil {
		t.Fatal("外部修改后撤销应被拒绝")
	}
	if !strings.Contains(err.Error(), "无法撤销") {
		t.Errorf("错误信息应说明无法撤销，实际 %q", err.Error())
	}
	// 目标应保持修改后的内容（没被覆盖）
	data, _ := os.ReadFile(dst)
	if string(data) != "modified by user" {
		t.Errorf("外部修改的内容不应被覆盖，实际 %q", string(data))
	}
}

// TestUndoSuccess 正常撤销应还原文件并记录历史。
func TestUndoSuccess(t *testing.T) {
	root := newTestRoot(t)
	src := filepath.Join(t.TempDir(), "a.txt")
	dst := filepath.Join(t.TempDir(), "b.txt")
	os.WriteFile(src, []byte("hello"), 0644)

	txn, _ := Begin(root, "paste", "撤销成功测试")
	if err := txn.Move(src, dst); err != nil {
		t.Fatalf("移动失败: %v", err)
	}
	if err := txn.Commit(root); err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	res, err := Undo(root, txn.ID)
	if err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if !res.OK || res.Reverted != 1 {
		t.Errorf("撤销结果异常: %+v", res)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("撤销后源应恢复: %v", err)
	}
	// 历史应有 2 条：原操作 + undo 记录
	list, _ := LoadHistory(root)
	if len(list) != 2 {
		t.Fatalf("应有 2 条历史，实际 %d", len(list))
	}
	// 原记录应标记 reverted
	for _, e := range list {
		if e.TxID == txn.ID && !e.Reverted {
			t.Error("原记录应标记为已撤销")
		}
	}
}

// TestUndoTwiceFails 重复撤销应报错。
func TestUndoTwiceFails(t *testing.T) {
	root := newTestRoot(t)
	src := filepath.Join(t.TempDir(), "a.txt")
	dst := filepath.Join(t.TempDir(), "b.txt")
	os.WriteFile(src, []byte("hello"), 0644)

	txn, _ := Begin(root, "paste", "重复撤销测试")
	txn.Move(src, dst)
	txn.Commit(root)

	if _, err := Undo(root, txn.ID); err != nil {
		t.Fatalf("首次撤销失败: %v", err)
	}
	if _, err := Undo(root, txn.ID); err == nil {
		t.Error("重复撤销应报错")
	}
}

// TestHistoryLimit 历史只保留 200 条。
func TestHistoryLimit(t *testing.T) {
	root := newTestRoot(t)
	for i := 0; i < MaxHistory+20; i++ {
		AppendHistory(root, HistoryEntry{
			TxID:  newTxID(),
			Kind:  "test",
			Title: "entry",
			At:    int64(i),
			Status: "committed",
		})
	}
	list, err := LoadHistory(root)
	if err != nil {
		t.Fatalf("读取历史失败: %v", err)
	}
	if len(list) != MaxHistory {
		t.Errorf("应保留 %d 条，实际 %d", MaxHistory, len(list))
	}
}

// TestCommitTwiceFails 重复提交应报错（幂等保护）。
func TestCommitTwiceFails(t *testing.T) {
	root := newTestRoot(t)
	txn, _ := Begin(root, "paste", "重复提交")
	if err := txn.Commit(root); err != nil {
		t.Fatalf("首次提交失败: %v", err)
	}
	if err := txn.Commit(root); err == nil {
		t.Error("重复提交应报错")
	}
}

// TestRollbackAfterCommitFails 提交后回滚应报错。
func TestRollbackAfterCommitFails(t *testing.T) {
	root := newTestRoot(t)
	txn, _ := Begin(root, "paste", "提交后回滚")
	txn.Commit(root)
	if err := txn.Rollback(); err == nil {
		t.Error("提交后回滚应报错")
	}
}
