package main

// crudops.go —— 自由模式操作接入层（规格 §6）
//
// 把 internal/ops 的 CRUD 能力挂到 Manager 上，并提供 Wails 绑定。
//
// 红线 N4：删除只进 DELED，永不 os.RemoveAll 资源。
// 唯一例外：回收站「彻底删除」（二次确认 + 输入名称）。

import (
	"fmt"
	"path/filepath"
	"strings"

	"kksresourcemanager/internal/fsx"
	"kksresourcemanager/internal/ops"
	"kksresourcemanager/internal/tx"
)

// trashStore 回收站存储（延迟初始化）。
func (m *Manager) trashStore() (*ops.TrashStore, error) {
	return ops.NewTrashStore(m.DelCardsRoot)
}

// filterHistoricalZipmodTrash 过滤历史遗留的 zipmod 回收站条目。
//
// v2.4 起不再管理 zipmod：这些条目只允许展示，
// 禁止还原 / 彻底删除，由用户手动处理。
func filterHistoricalZipmodTrash(rels []string) (kept []string, blocked int) {
	kept = make([]string, 0, len(rels))
	for _, rel := range rels {
		r := filepath.ToSlash(rel)
		if r == "zipmod" || strings.HasPrefix(r, "zipmod/") {
			blocked++
			continue
		}
		kept = append(kept, rel)
	}
	return kept, blocked
}

// specOf 判断路径所属的资源种类与该种类的根目录。
//
// root 用于计算「原相对路径」—— 回收站必须保留这个结构，
// 否则删两个不同子目录里的同名 2.png 会变成 2.png / 2__2.png，
// 用户完全看不出原本在哪（v2.2 FIX-07）。
func (m *Manager) specOf(abs string) (string, string, bool) {
	for _, kind := range ResourceKinds {
		if r, err := filepath.Rel(m.SourceDir(kind), abs); err == nil && !strings.HasPrefix(r, "..") {
			return kind.Mode, m.SourceDir(kind), true
		}
		if r, err := filepath.Rel(m.DelDir(kind), abs); err == nil && !strings.HasPrefix(r, "..") {
			return kind.Mode, m.DelDir(kind), true
		}
	}
	return "", "", false
}

// CmdMkdir 在指定目录下新建文件夹。
//
// 返回实际创建名（默认名冲突时自动加 (2)）。
func (m *Manager) CmdMkdir(side, mode, parentRel string) ops.MkdirResult {
	dir := m.resolveRelDir(mode, side, parentRel)
	if dir == "" {
		return ops.MkdirResult{Code: fsx.ErrIOError, Message: "无法确定目标目录。"}
	}

	txn, err := m.beginTx("mkdir", "新建文件夹")
	if err != nil {
		return ops.MkdirResult{Code: fsx.ErrIOError, Message: err.Error()}
	}

	store, err := m.trashStore()
	if err != nil {
		m.rollbackTx(txn)
		return ops.MkdirResult{Code: fsx.ErrIOError, Message: err.Error()}
	}
	result := store.Mkdir(txn, dir, fsx.DefaultFolderName)
	if !result.OK {
		m.rollbackTx(txn)
		return result
	}
	if err := m.commitTx(txn); err != nil {
		result.Message = "已创建，但提交事务失败：" + err.Error()
	}
	return result
}

// CmdRename 重命名一个条目。
func (m *Manager) CmdRename(pathAbs, newName string) ops.OpResult {
	if strings.TrimSpace(pathAbs) == "" || strings.TrimSpace(newName) == "" {
		return ops.OpResult{Code: fsx.ErrEmptyName, Message: "名称不能为空。"}
	}
	abs, err := filepath.Abs(pathAbs)
	if err != nil {
		return ops.OpResult{Code: fsx.ErrIOError, Message: err.Error()}
	}
	// 路径必须受管
	if !m.pathAllowed(abs) {
		return ops.OpResult{Code: fsx.ErrIOError, Message: "该路径不在允许的范围内。"}
	}

	txn, err := m.beginTx("rename", "重命名 "+filepath.Base(abs))
	if err != nil {
		return ops.OpResult{Code: fsx.ErrIOError, Message: err.Error()}
	}

	store, err := m.trashStore()
	if err != nil {
		m.rollbackTx(txn)
		return ops.OpResult{Code: fsx.ErrIOError, Message: err.Error()}
	}
	res := store.Rename(txn, abs, newName)
	if !res.OK {
		m.rollbackTx(txn)
		return res
	}
	if err := m.commitTx(txn); err != nil {
		res.Message = "已重命名，但提交事务失败：" + err.Error()
	}
	m.invalidateCaches()
	return res
}

// PlanDelete 生成删除计划（零副作用）。
func (m *Manager) PlanDelete(paths []string) ops.DeletePlan {
	store, err := m.trashStore()
	if err != nil {
		return ops.DeletePlan{}
	}
	plan := store.PlanDelete(paths, m.specOf)
	plan.PlanID = fmt.Sprintf("del-%d", nowMS())
	return plan
}

// CommitDelete 执行删除：移入回收站。
func (m *Manager) CommitDelete(paths []string) (int, []string, error) {
	store, err := m.trashStore()
	if err != nil {
		return 0, nil, err
	}
	plan := store.PlanDelete(paths, m.specOf)
	if len(plan.Conflicts) > 0 {
		return 0, nil, fmt.Errorf("有 %d 项无法删除", len(plan.Conflicts))
	}

	txn, err := m.beginTx("delete", fmt.Sprintf("删除 %d 个条目", len(plan.Items)))
	if err != nil {
		return 0, nil, err
	}

	moved, errs := store.CommitDelete(txn, plan)
	if len(errs) > 0 {
		m.rollbackTx(txn)
		return 0, errs, fmt.Errorf("删除失败")
	}
	if err := m.commitTx(txn); err != nil {
		return moved, errs, err
	}
	m.invalidateCaches()
	return moved, errs, nil
}

// GetTrashTree 返回回收站列表。
func (m *Manager) GetTrashTree() []ops.TrashEntry {
	store, err := m.trashStore()
	if err != nil {
		return []ops.TrashEntry{}
	}
	list, err := store.List()
	if err != nil {
		return []ops.TrashEntry{}
	}
	for i := range list {
		list[i].Name = filepath.Base(list[i].Rel)
	}
	return list
}

// CmdTrashRestore 把回收站条目移回原位。
func (m *Manager) CmdTrashRestore(trashRels []string) ops.RestoreResult {
	store, err := m.trashStore()
	if err != nil {
		return ops.RestoreResult{Conflicts: []ops.Conflict{{Reason: err.Error()}}}
	}

	kept, _ := filterHistoricalZipmodTrash(trashRels)
	if len(kept) == 0 {
		return ops.RestoreResult{Conflicts: []ops.Conflict{{
			Reason: "所选条目属于历史遗留 zipmod，本工具已不再管理 zipmod，请手动处理。",
		}}}
	}
	trashRels = kept

	txn, err := m.beginTx("trash-restore", fmt.Sprintf("还原 %d 个条目", len(trashRels)))
	if err != nil {
		return ops.RestoreResult{Conflicts: []ops.Conflict{{Reason: err.Error()}}}
	}

	res := store.Restore(txn, trashRels)
	if res.Restored == 0 && len(res.Conflicts) > 0 {
		m.rollbackTx(txn)
		return res
	}
	if err := m.commitTx(txn); err != nil {
		res.Conflicts = append(res.Conflicts, ops.Conflict{Reason: err.Error()})
		return res
	}
	m.invalidateCaches()
	return res
}

// CmdTrashPurge 彻底删除回收站条目。
//
// 这是全项目唯一允许真删的地方。调用方必须先做二次确认 + 输入名称确认。
func (m *Manager) CmdTrashPurge(trashRels []string, confirmText string) ops.PurgeResult {
	store, err := m.trashStore()
	if err != nil {
		return ops.PurgeResult{Errors: []string{err.Error()}}
	}

	kept, _ := filterHistoricalZipmodTrash(trashRels)
	if len(kept) == 0 {
		return ops.PurgeResult{Errors: []string{
			"所选条目属于历史遗留 zipmod，本工具已不再管理 zipmod，请手动处理。",
		}}
	}
	trashRels = kept

	// 二次确认：必须输入某个待删条目的名称
	if strings.TrimSpace(confirmText) == "" {
		return ops.PurgeResult{Errors: []string{"请输入条目名称以确认彻底删除。"}}
	}
	list, err := store.List()
	if err != nil {
		return ops.PurgeResult{Errors: []string{err.Error()}}
	}
	matched := false
	for _, e := range list {
		if strings.EqualFold(filepath.Base(e.Rel), strings.TrimSpace(confirmText)) {
			matched = true
			break
		}
	}
	if !matched {
		return ops.PurgeResult{Errors: []string{"输入的名称与待删条目不匹配，已取消。"}}
	}

	res := store.Purge(trashRels)
	m.invalidateCaches()
	return res
}

// TrashStoreFor 返回回收站存储（供测试使用）。
func (m *Manager) TrashStoreFor() (*ops.TrashStore, error) {
	return m.trashStore()
}

// txOf 返回当前事务（供外部检查）。
func (m *Manager) txOf() *tx.Tx { return m.currentTx }
