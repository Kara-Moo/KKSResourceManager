package main

// txwrap_test.go —— 事务包装的端到端验证（§11.6）

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIsolateCreatesHistory 隔离操作应写入历史。
func TestIsolateCreatesHistory(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	card := env.femaleCard("h1.png")
	writeFile(t, card, "card")
	m.Scanned = true

	res := m.Isolate([]string{card})
	if res.FailedCount != 0 {
		t.Fatalf("隔离失败: %v", res.Logs)
	}

	list := m.GetHistory(0, 10)
	if len(list) != 1 {
		t.Fatalf("应有 1 条历史，实际 %d", len(list))
	}
	if list[0].Kind != "isolate" {
		t.Errorf("历史 kind 应为 isolate，实际 %s", list[0].Kind)
	}
	if list[0].Status != "committed" {
		t.Errorf("状态应为 committed，实际 %s", list[0].Status)
	}
}

// TestUndoIsolateRestoresFile 撤销隔离应把文件放回原位。
func TestUndoIsolateRestoresFile(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	card := env.femaleCard("u1.png")
	writeFile(t, card, "card content")
	m.Scanned = true

	res := m.Isolate([]string{card})
	if res.FailedCount != 0 {
		t.Fatalf("隔离失败: %v", res.Logs)
	}
	// 隔离后文件在 DEL 区
	delCard := filepath.Join(m.DelDir(ResourceKinds[0]), "u1.png")
	if _, err := os.Stat(delCard); err != nil {
		t.Fatalf("隔离区应有文件: %v", err)
	}

	list := m.GetHistory(0, 10)
	if len(list) == 0 {
		t.Fatal("无历史记录")
	}

	undoRes, err := m.CmdUndo(list[0].TxID)
	if err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if !undoRes.OK {
		t.Errorf("撤销应成功: %+v", undoRes)
	}

	// 文件应回到原位，内容一致
	data, err := os.ReadFile(card)
	if err != nil {
		t.Fatalf("撤销后原路径应有文件: %v", err)
	}
	if string(data) != "card content" {
		t.Errorf("内容应一致，实际 %q", string(data))
	}
	// 隔离区不应残留
	if _, err := os.Stat(delCard); !os.IsNotExist(err) {
		t.Error("撤销后隔离区不应残留文件")
	}
}

// TestUndoRejectsModifiedTarget 外部修改后撤销应被拒绝。
func TestUndoRejectsModifiedTarget(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	card := env.femaleCard("m1.png")
	writeFile(t, card, "original")
	m.Scanned = true

	m.Isolate([]string{card})
	list := m.GetHistory(0, 10)

	// 外部修改隔离区里的文件
	delCard := filepath.Join(m.DelDir(ResourceKinds[0]), "m1.png")
	writeFile(t, delCard, "externally modified")

	_, err := m.CmdUndo(list[0].TxID)
	if err == nil {
		t.Fatal("外部修改后撤销应被拒绝")
	}
	// 文件内容不应被覆盖
	data, _ := os.ReadFile(delCard)
	if string(data) != "externally modified" {
		t.Errorf("外部修改的内容不应被覆盖，实际 %q", string(data))
	}
}

// TestRestoreCreatesHistory 恢复操作应写入历史。
func TestRestoreCreatesHistory(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	card := env.femaleCard("r1.png")
	writeFile(t, card, "x")
	m.Scanned = true
	m.Isolate([]string{card})

	delCard := filepath.Join(m.DelDir(ResourceKinds[0]), "r1.png")
	res := m.Restore([]string{delCard})
	if res.FailedCount != 0 {
		t.Fatalf("恢复失败: %v", res.Logs)
	}

	list := m.GetHistory(0, 10)
	if len(list) != 2 {
		t.Fatalf("应有 2 条历史（隔离+恢复），实际 %d", len(list))
	}
	// 最新的是恢复
	if list[0].Kind != "restore" {
		t.Errorf("最新历史应为 restore，实际 %s", list[0].Kind)
	}
}

// TestFailedIsolateRollsBack 有失败项时应回滚。
func TestFailedIsolateRollsBack(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	// 两个卡片，其中一个目标已存在（会失败）
	cardA := env.femaleCard("a.png")
	cardB := env.femaleCard("b.png")
	writeFile(t, cardA, "a")
	writeFile(t, cardB, "b")
	m.Scanned = true

	// 预先在目标位置放同名文件，制造冲突
	writeFile(t, filepath.Join(m.DelDir(ResourceKinds[0]), "a.png"), "conflict")

	res := m.Isolate([]string{cardA, cardB})
	if res.FailedCount == 0 {
		t.Fatal("应有失败项")
	}

	// 回滚后 a.png 应回到原位（尽管它本身移动失败了，
	// 但 b.png 成功移动了，应被回滚）
	if _, err := os.Stat(cardB); err != nil {
		t.Errorf("回滚后 b.png 应回到原位: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.DelDir(ResourceKinds[0]), "b.png")); !os.IsNotExist(err) {
		t.Error("回滚后隔离区不应残留 b.png")
	}
}

// TestCrashRecoveryViaManager 模拟崩溃后重启应自动还原。
func TestCrashRecoveryViaManager(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	card := env.femaleCard("c1.png")
	writeFile(t, card, "x")
	m.Scanned = true

	// 开启事务并移动，但不提交（模拟崩溃）
	txn, err := m.beginTx("paste", "崩溃模拟")
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	delCard := filepath.Join(m.DelDir(ResourceKinds[0]), "c1.png")
	if err := txn.Move(card, delCard); err != nil {
		t.Fatalf("移动失败: %v", err)
	}

	// 恢复
	recovered, errs := m.RecoverPendingTx()
	if recovered != 1 {
		t.Errorf("应恢复 1 个事务，实际 %d，错误 %v", recovered, errs)
	}
	if _, err := os.Stat(card); err != nil {
		t.Errorf("崩溃恢复后原路径应有文件: %v", err)
	}
	if _, err := os.Stat(delCard); !os.IsNotExist(err) {
		t.Error("崩溃恢复后隔离区不应残留")
	}
}
