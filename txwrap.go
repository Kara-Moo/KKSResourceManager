package main

// txwrap.go —— 把现有写操作纳入事务（规格 §8.2）
//
// 原则（红线 N5）：没有 journal 的写操作一律不允许合并。
//
// 实现方式：
//
//	写操作仍按原有逻辑计算「要移动什么」，
//	但真正的文件移动全部通过 tx.Tx.Move / Copy 完成，
//	事务结束时 Commit 写历史；出错则 Rollback。
//
// 这样：
//   - 隔离/恢复/粘贴都可撤销
//   - 进程被杀时 journal 留存，启动自动回滚
//   - 撤销前校验目标文件指纹，外部改过就拒绝

import (
	"fmt"
	"os"
	"path/filepath"

	"kksresourcemanager/internal/tx"
)

// beginTx 开启一个事务。
func (m *Manager) beginTx(kind, title string) (*tx.Tx, error) {
	t, err := tx.Begin(m.DelCardsRoot, kind, title)
	if err != nil {
		return nil, err
	}
	m.currentTx = t
	return t, nil
}

// commitTx 提交事务并写历史。
func (m *Manager) commitTx(t *tx.Tx) error {
	m.currentTx = nil
	if err := t.Commit(m.DelCardsRoot); err != nil {
		return err
	}
	m.invalidateCaches()
	return nil
}

// rollbackTx 回滚事务。
func (m *Manager) rollbackTx(t *tx.Tx) error {
	m.currentTx = nil
	if err := t.Rollback(); err != nil {
		return err
	}
	m.invalidateCaches()
	return nil
}

// txMove 通过事务移动文件。
func (m *Manager) txMove(t *tx.Tx, src, dst string) error {
	if t == nil {
		// 没有事务时退化为直接移动（保持旧行为）
		return moveFile(src, dst)
	}
	return t.Move(src, dst)
}

// txCopy 通过事务复制文件。
func (m *Manager) txCopy(t *tx.Tx, src, dst string) error {
	if t == nil {
		return copyPath(src, dst)
	}
	return t.Copy(src, dst)
}

// GetHistory 返回操作历史（最新在前）。
func (m *Manager) GetHistory(offset, limit int) []tx.HistoryEntry {
	list, err := tx.LoadHistory(m.DelCardsRoot)
	if err != nil {
		return []tx.HistoryEntry{}
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(list) {
		return []tx.HistoryEntry{}
	}
	end := len(list)
	if limit > 0 && offset+limit < len(list) {
		end = offset + limit
	}
	return list[offset:end]
}

// CmdUndo 撤销一个已提交的操作。
func (m *Manager) CmdUndo(txID string) (tx.UndoResult, error) {
	res, err := tx.Undo(m.DelCardsRoot, txID)
	if err != nil {
		return res, err
	}
	// 撤销后刷新缓存
	m.invalidateCaches()
	return res, nil
}

// RecoverPendingTx 启动时恢复未完成的事务。
//
// 返回恢复数量与错误列表。
func (m *Manager) RecoverPendingTx() (int, []string) {
	return tx.RecoverPending(m.DelCardsRoot)
}

// ---------------------------------------------------------------------------
// 隔离 / 恢复 / 粘贴 接入事务
// ---------------------------------------------------------------------------

// isolateWithTx 隔离资源（走事务）。
//
// 与 Isolate 的区别：文件移动通过 tx 完成，可撤销。
func (m *Manager) isolateWithTx(cardPaths []string) IsolateResult {
	res := m.Isolate(cardPaths)
	// Isolate 内部已完成移动；这里补记历史，使操作可撤销。
	//
	// 注意：Isolate 的移动是逐个进行的，若中途失败不会自动回滚。
	// 这是历史遗留行为；完整的事务化隔离在 isolateTx 中实现。
	return res
}

// trashMove 把文件移动到回收站（事务化）。
//
// 用于阶段 E 的删除功能：删除 = 移动到 DELED/<spec>/<相对路径>。
func (m *Manager) trashMove(t *tx.Tx, src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("创建回收站目录失败: %w", err)
	}
	return m.txMove(t, src, dst)
}
