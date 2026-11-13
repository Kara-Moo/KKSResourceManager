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
//  1. zipmod 的复制/剪切/粘贴等价于「启用 / 隔离」，
//     且必须严格保证不会有 zipmod 缺失：
//     把 zipmod 从游戏 mods 目录移走之前，要先确认没有任何仍存在的
//     资源文件引用它，否则拒绝该操作。
//
//  2. PNG 的复制/粘贴不受「保留路径相同」约束，可任意操作；
//     但若目标位置已存在同名文件，则取消该次复制并报「无法复制」。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// Mode 来源的资源类型（female_card 等），用于判断 zipmod
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

	// --- 预检：目标同名冲突与 zipmod 依赖 ---
	// 预检全部通过才执行，避免执行到一半才发现冲突。
	type planItem struct {
		src      string
		dst      string
		isZipmod bool
		guid     string
	}
	var plans []planItem
	var conflict error
	var modBlocked *planItem

	for _, src := range state.Paths {
		abs, err := filepath.Abs(src)
		if err != nil {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("路径无效: %s", src)})
			continue
		}
		if _, err := os.Stat(abs); err != nil {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("源不存在: %s", abs)})
			continue
		}

		// 是否是 zipmod？以扩展名判定（而非 GUID 是否解析成功），
		// 否则解析失败的 zipmod 会绕过依赖检查被移走，造成依赖缺失。
		isZipmod := strings.EqualFold(filepath.Ext(abs), zipmodExt)
		guid := ""
		if isZipmod {
			guid = m.zipmodGUID(abs)
		}

		dst := filepath.Join(targetDir, filepath.Base(abs))
		// 若源就在目标目录内且为复制操作，视为无意义（同名冲突）
		if _, err := os.Stat(dst); err == nil {
			conflict = fmt.Errorf("无法复制：目标已存在同名文件 %s", filepath.Base(abs))
			break
		}

		// zipmod 安全校验：从 mods 侧移出时不能造成依赖缺失
		if isZipmod && state.SourceSide == SideKKS && targetSide == SideDEL {
			// GUID 解析不出来时无法判断依赖归属，保守起见拒绝移出
			if guid == "" {
				item := planItem{src: abs, dst: dst, isZipmod: true, guid: guid}
				modBlocked = &item
				res.Logs = append(res.Logs, OperationLog{
					"warn",
					fmt.Sprintf("无法隔离：%s 无法识别 GUID，无法确认依赖关系，已保留原文件。", filepath.Base(abs)),
				})
				break
			}
			if blockers := m.zipmodStillReferenced(guid, abs); len(blockers) > 0 {
				item := planItem{src: abs, dst: dst, isZipmod: true, guid: guid}
				modBlocked = &item
				res.Logs = append(res.Logs, OperationLog{
					"warn",
					fmt.Sprintf("无法隔离：%s (GUID %s) 仍被 %d 个资源引用，例如: %s",
						filepath.Base(abs), guid, len(blockers), firstN(blockers, 3)),
				})
				break
			}
		}

		plans = append(plans, planItem{src: abs, dst: dst, isZipmod: isZipmod, guid: guid})
	}

	// 冲突时取消整个复制操作
	if conflict != nil {
		res.FailedCount++
		res.Logs = append(res.Logs, OperationLog{"error", conflict.Error()})
		res.Success = false
		return res
	}
	// zipmod 依赖阻塞时同样取消
	if modBlocked != nil {
		res.FailedCount++
		res.Success = false
		return res
	}

	// --- 执行 ---
	for _, p := range plans {
		// zipmod 的语义是「启用 / 隔离」而非普通复制：
		//   从 mods 侧到隔离区 = 隔离（源必须移走，否则等于没隔离）
		//   从隔离区到 mods 侧 = 启用（源必须移走，否则会重复）
		// 因此 zipmod 无论复制还是剪切，都执行移动。
		effectiveOp := state.Op
		if p.isZipmod {
			effectiveOp = ClipboardCut
		}

		if effectiveOp == ClipboardCut {
			if err := moveFile(p.src, p.dst); err != nil {
				res.FailedCount++
				res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("移动失败 %s -> %s: %v", p.src, p.dst, err)})
				continue
			}
			m.afterResourceMoved(p.src, p.dst, p.isZipmod, p.guid)
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
// 依赖与状态维护
// ---------------------------------------------------------------------------

// zipmodGUID 读取 zipmod 的 GUID。
func (m *Manager) zipmodGUID(path string) string {
	manifest, err := ReadModManifest(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(manifest.GUID)
}

// zipmodStillReferenced 判断 zipmod 是否仍被资源文件引用。
//
// excludePath 为正在被移动的 zipmod 自身（移动后它就不再位于原位置）。
func (m *Manager) zipmodStillReferenced(guid, excludePath string) []string {
	var blockers []string
	for _, ref := range m.GlobalDepMap[guid] {
		if ref == excludePath {
			continue
		}
		// 只统计真实存在的文件
		if _, err := os.Stat(ref); err != nil {
			continue
		}
		blockers = append(blockers, ref)
	}
	sort.Strings(blockers)
	return blockers
}

// afterResourceMoved 在文件移动/复制后同步维护全局依赖图。
func (m *Manager) afterResourceMoved(src, dst string, isZipmod bool, guid string) {
	if isZipmod {
		// zipmod 移动：更新或移除 GUID 索引
		if guid != "" {
			if _, err := os.Stat(dst); err == nil {
				m.ZipmodDB[guid] = dst
			} else {
				delete(m.ZipmodDB, guid)
			}
		}
		// 源已不存在，从依赖图中移除其引用
		m.removeDep(src)
		return
	}

	// 资源文件：解析新位置的依赖并更新**正向**索引
	guids, err := ParseCardGUIDs(dst)
	if err != nil || len(guids) == 0 {
		m.removeDep(src)
		m.rebuildDepIndex()
		return
	}
	m.FileDeps[dst] = guids
	m.removeDep(src)
	// 反向图由正向数据派生，不做增量 append（避免重复累积）
	m.rebuildDepIndex()
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
