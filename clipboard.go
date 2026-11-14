package main

// clipboard.go —— 复制 / 剪切 / 粘贴
//
// 交互语义对齐 Windows 文件资源管理器：
//   - 复制：把源文件复制到目标目录，源保留；
//   - 剪切：把源文件移动到目标目录，源移除；
//   - 粘贴：把剪贴板内容写入当前目标目录。
//
// 两条硬性规则：
//
//  1. 本工具不再对 zipmod 执行任何文件操作：粘贴时遇到 .zipmod 直接拒绝。
//
//  2. PNG/JPG 的复制/粘贴不受「保留路径相同」约束，可任意操作；
//     但若目标位置已存在同名文件，则取消该次复制并报「无法复制」。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// 剪贴板操作类型
const (
	ClipboardNone = ""       // 空
	ClipboardCopy = "copy"   // 复制
	ClipboardCut  = "cut"    // 剪切
)

// 剪贴板来源侧
const (
	SideKKS = "kks" // 游戏主目录侧
	SideDEL = "del" // 隔离区侧
)

// ClipboardState 剪贴板当前内容。
type ClipboardState struct {
	// Op 复制或剪切
	Op string `json:"op"`
	// Paths 源文件/文件夹的绝对路径
	Paths []string `json:"paths"`
	// SourceSide 来源侧（kks / del）
	SourceSide string `json:"sourceSide"`
	// Mode 来源的资源类型（female_card 等）
	Mode string `json:"mode"`
}

// IsEmpty 是否为空剪贴板。
func (c ClipboardState) IsEmpty() bool {
	return c.Op == ClipboardNone || len(c.Paths) == 0
}

// Clipboard 管理剪贴板状态。
type Clipboard struct {
	mu    sync.RWMutex
	state ClipboardState
}

// Set 设置剪贴板内容。
func (c *Clipboard) Set(op string, paths []string, side, mode string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = ClipboardState{
		Op:         op,
		Paths:      append([]string(nil), paths...),
		SourceSide: side,
		Mode:       mode,
	}
}

// Get 读取剪贴板内容（返回副本）。
func (c *Clipboard) Get() ClipboardState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return ClipboardState{
		Op:         c.state.Op,
		Paths:      append([]string(nil), c.state.Paths...),
		SourceSide: c.state.SourceSide,
		Mode:       c.state.Mode,
	}
}

// Clear 清空剪贴板。
func (c *Clipboard) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = ClipboardState{}
}

// ---------------------------------------------------------------------------
// 剪贴板结果
// ---------------------------------------------------------------------------

// ClipboardResult 复制/剪切/粘贴结果。
type ClipboardResult struct {
	Success      bool           `json:"success"`
	Op           string         `json:"op"`
	Affected     int            `json:"affected"`
	Skipped      int            `json:"skipped"`
	FailedCount  int            `json:"failedCount"`
	Logs         []OperationLog `json:"logs"`
}

// ---------------------------------------------------------------------------
// 粘贴
// ---------------------------------------------------------------------------

// Paste 执行粘贴：把剪贴板内容写入目标目录。
//
// targetDir  目标目录（绝对路径）
// targetSide 目标侧（kks / del）
// targetMode 目标资源类型
func (m *Manager) Paste(targetDir, targetSide, targetMode string) ClipboardResult {
	state := m.clipboard.Get()
	res := ClipboardResult{Op: state.Op, Logs: make([]OperationLog, 0, 12)}

	if state.IsEmpty() {
		res.Logs = append(res.Logs, OperationLog{"warn", "剪贴板为空，没有可粘贴的内容。"})
		return res
	}
	if strings.TrimSpace(targetDir) == "" {
		res.Logs = append(res.Logs, OperationLog{"error", "未指定粘贴目标目录。"})
		return res
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// 目标目录必须存在（若被隐藏删除则重建）
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		res.FailedCount++
		res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("创建目标目录失败 %s: %v", targetDir, err)})
		return res
	}

	// --- 预检：目标同名冲突 ---
	// 预检全部通过才执行，避免执行到一半才发现冲突。
	type planItem struct {
		src string
		dst string
	}
	var plans []planItem
	var conflict error

	for _, src := range state.Paths {
		abs, err := filepath.Abs(src)
		if err != nil {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("路径无效: %s", src)})
			continue
		}

		// 本工具不再对 zipmod 执行任何文件操作：直接拒绝，不移动、不复制。
		if strings.EqualFold(filepath.Ext(abs), ".zipmod") {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error",
				fmt.Sprintf("[拒绝] %s 是 zipmod，本工具不再对 zipmod 执行任何文件操作。", filepath.Base(abs))})
			continue
		}

		if _, err := os.Stat(abs); err != nil {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("源不存在: %s", abs)})
			continue
		}

		dst := filepath.Join(targetDir, filepath.Base(abs))
		// 若源就在目标目录内且为复制操作，视为无意义（同名冲突）
		if _, err := os.Stat(dst); err == nil {
			conflict = fmt.Errorf("无法复制：目标已存在同名文件 %s", filepath.Base(abs))
			break
		}

		plans = append(plans, planItem{src: abs, dst: dst})
	}

	// 冲突时取消整个复制操作
	if conflict != nil {
		res.FailedCount++
		res.Logs = append(res.Logs, OperationLog{"error", conflict.Error()})
		res.Success = false
		return res
	}

	// --- 执行 ---
	for _, p := range plans {
		if state.Op == ClipboardCut {
			if err := moveFile(p.src, p.dst); err != nil {
				res.FailedCount++
				res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("移动失败 %s -> %s: %v", p.src, p.dst, err)})
				continue
			}
			res.Affected++
			res.Logs = append(res.Logs, OperationLog{"success", fmt.Sprintf("[移动] %s -> %s", p.src, p.dst)})
		} else {
			if err := copyPath(p.src, p.dst); err != nil {
				res.FailedCount++
				res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("复制失败 %s -> %s: %v", p.src, p.dst, err)})
				continue
			}
			res.Affected++
			res.Logs = append(res.Logs, OperationLog{"success", fmt.Sprintf("[复制] %s -> %s", p.src, p.dst)})
		}
	}

	// 剪切完成后清空剪贴板（与资源管理器一致）
	if state.Op == ClipboardCut {
		m.clipboard.Clear()
	}

	res.Success = res.FailedCount == 0
	if res.Success {
		res.Logs = append(res.Logs, OperationLog{"success", fmt.Sprintf("粘贴完成：处理 %d 个条目。", res.Affected)})
	} else {
		res.Logs = append(res.Logs, OperationLog{"warn", fmt.Sprintf("粘贴结束，存在 %d 个失败项。", res.FailedCount)})
	}
	return res
}

// ---------------------------------------------------------------------------
// 文件与目录复制
// ---------------------------------------------------------------------------

// copyPath 复制文件或整个目录树。
//
// 目标已存在时直接报错，绝不覆盖。
func copyPath(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("目标已存在: %s", dst)
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dst, info.Mode().Perm())
	}

	// 目录：递归复制
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		srcChild := filepath.Join(src, e.Name())
		dstChild := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyPath(srcChild, dstChild); err != nil {
				return err
			}
			continue
		}
		childInfo, err := e.Info()
		if err != nil {
			return err
		}
		if err := copyFile(srcChild, dstChild, childInfo.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// copyFile 复制单个文件，保留权限位，绝不覆盖已存在的目标。
func copyFile(src, dst string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := copyAll(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
