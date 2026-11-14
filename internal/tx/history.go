package tx

// history.go —— 操作历史（规格 §8.3）

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// HistoryEntry 一条历史记录。
type HistoryEntry struct {
	TxID     string `json:"txId"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	At       int64  `json:"at"`
	Affected int    `json:"affected"`
	Bytes    int64  `json:"bytes"`
	// Status committed | rolledback | reverted | partial
	Status string `json:"status"`
	// Reverted 是否已被撤销
	Reverted bool `json:"reverted"`
	// RevertErr 撤销失败原因
	RevertErr string `json:"revertErr,omitempty"`
	// Steps 步骤明细（用于撤销）
	Steps []Step `json:"steps,omitempty"`
}

// historyPath 返回历史文件路径。
func historyPath(root string) string {
	return filepath.Join(root, StateDirName, HistoryFileName)
}

// 存储约定：**文件里按时间正序（最旧在前）**。
// 读取时反转成倒序（最新在前）供 UI 使用。
//
// 早前版本两处约定混用（存储正序、读取也正序），
// 导致「最新一条」总显示在最后一行。这里统一约定，避免再错。

// LoadHistory 读取历史记录（按时间倒序，最新在前）。
func LoadHistory(root string) ([]HistoryEntry, error) {
	data, err := os.ReadFile(historyPath(root))
	if err != nil {
		if os.IsNotExist(err) {
			return []HistoryEntry{}, nil
		}
		return nil, fmt.Errorf("读取历史失败: %w", err)
	}
	if len(data) == 0 {
		return []HistoryEntry{}, nil
	}
	var stored []HistoryEntry
	if err := json.Unmarshal(data, &stored); err != nil {
		// 损坏则丢弃，不阻断启动
		return []HistoryEntry{}, nil
	}
	// 反转：最新的在前
	list := make([]HistoryEntry, len(stored))
	for i := range stored {
		list[len(stored)-1-i] = stored[i]
	}
	return list, nil
}

// AppendHistory 追加一条历史记录。
//
// 只保留最近 MaxHistory 条，超出按时间淘汰。
// 被淘汰的条目其 journal 已删除，因此**无法再撤销**。
func AppendHistory(root string, entry HistoryEntry) error {
	// 读出倒序（最新在前）
	list, err := LoadHistory(root)
	if err != nil {
		return err
	}
	// 新记录放最前
	list = append([]HistoryEntry{entry}, list...)

	// 超出上限时淘汰最旧的（尾部）
	if len(list) > MaxHistory {
		list = list[:MaxHistory]
	}

	// 反转回正序存储（最旧在前）
	stored := make([]HistoryEntry, len(list))
	for i := range list {
		stored[len(list)-1-i] = list[i]
	}

	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化历史失败: %w", err)
	}
	dir := filepath.Dir(historyPath(root))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp := historyPath(root) + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, historyPath(root))
}

// UpdateHistoryEntry 更新一条历史记录（撤销后标记 reverted）。
func UpdateHistoryEntry(root string, txID string, mutate func(*HistoryEntry)) error {
	list, err := LoadHistory(root)
	if err != nil {
		return err
	}
	found := false
	for i := range list {
		if list[i].TxID == txID {
			mutate(&list[i])
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("未找到历史记录: %s", txID)
	}

	// 反转回正序存储（最旧在前）
	stored := make([]HistoryEntry, len(list))
	for i := range list {
		stored[len(list)-1-i] = list[i]
	}
	data, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return err
	}
	tmp := historyPath(root) + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, historyPath(root))
}

// UndoResult 撤销结果。
type UndoResult struct {
	OK       bool   `json:"ok"`
	Reverted int    `json:"reverted"`
	Conflict string `json:"conflict,omitempty"`
}

// Undo 撤销一个已提交的事务。
//
// 前置校验（逐步骤）：Dst 当前指纹必须与记录的 DstFP 一致。
// 任一不符 → 拒绝整个撤销，指出第一个冲突文件。
func Undo(root, txID string) (UndoResult, error) {
	list, err := LoadHistory(root)
	if err != nil {
		return UndoResult{}, err
	}

	var target *HistoryEntry
	for i := range list {
		if list[i].TxID == txID {
			target = &list[i]
			break
		}
	}
	if target == nil {
		return UndoResult{}, fmt.Errorf("未找到历史记录: %s", txID)
	}
	if target.Status != "committed" {
		return UndoResult{}, fmt.Errorf("该操作状态为 %s，不能撤销", target.Status)
	}
	if target.Reverted {
		return UndoResult{}, fmt.Errorf("该操作已被撤销")
	}
	if len(target.Steps) == 0 {
		return UndoResult{}, fmt.Errorf("该操作没有可撤销的步骤")
	}

	// 前置校验：逐个检查目标文件指纹
	for i := len(target.Steps) - 1; i >= 0; i-- {
		step := target.Steps[i]
		if step.State != "done" {
			continue
		}
		if step.Kind == "mkdir" || step.Kind == "rmdir" {
			// 目录步骤没有可校验的文件指纹：mkdir 的目标仍在，
			// rmdir 的目标已按设计消失。撤销时由 rollbackStep 重建/回收目录。
			continue
		}
		fp, err := fingerprintOf(step.Dst)
		if err != nil {
			return UndoResult{}, fmt.Errorf("无法撤销：%s 已被外部修改或删除", step.Dst)
		}
		if !fp.Equal(step.DstFP) {
			return UndoResult{}, fmt.Errorf("无法撤销：%s 已被外部修改", step.Dst)
		}
	}

	// 逆序回滚
	reverted := 0
	for i := len(target.Steps) - 1; i >= 0; i-- {
		step := target.Steps[i]
		if step.State != "done" {
			continue
		}
		if err := rollbackStep(&step); err != nil {
			// 部分回滚：保留记录并标注
			errMsg := err.Error()
			UpdateHistoryEntry(root, txID, func(e *HistoryEntry) {
				e.Status = "partial"
				e.RevertErr = errMsg
			})
			return UndoResult{
				OK:       false,
				Reverted: reverted,
				Conflict: errMsg,
			}, fmt.Errorf("撤销失败（已回滚 %d 步）：%v", reverted, err)
		}
		reverted++
	}

	// 成功：标记 reverted，并新增一条 kind=undo 的历史（便于审计）
	UpdateHistoryEntry(root, txID, func(e *HistoryEntry) {
		e.Reverted = true
		e.Status = "reverted"
	})
	AppendHistory(root, HistoryEntry{
		TxID:     newTxID(),
		Kind:     "undo",
		Title:    "撤销：" + target.Title,
		At:       time.Now().UnixMilli(),
		Affected: reverted,
		Status:   "committed",
	})

	return UndoResult{OK: true, Reverted: reverted}, nil
}
