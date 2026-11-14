package main

// transfer.go —— 文件夹迁移引擎（v2.4 阶段 4 / 规格 §6）
//
// 核心原则：先算清、再动盘。
//
//   - planFolderTransfer 是纯只读规划：只做 WalkDir + Stat，
//     绝不创建、移动、删除任何东西（不变量 I1）。
//   - execTransferPlan 按计划执行，每一步都通过 tx.Tx 记录
//     （mkdir / move / rmdir），任一步失败即整体回滚 —— 包括回收已建目录（B3）。
//   - Isolate / Restore 共用同一套规划与执行逻辑，只在目标根上不同。
//
// 这套引擎修复 T9 的三条根因：
//   B1 空文件夹被当资源：返回 errEmptyFolder，不生成任何 Step，绝不建目录；
//   B2 非卡片文件被丢弃：WalkDir 不过滤扩展名，所有普通文件都迁；
//   B3 回滚不回收目录：目录的创建/删除都进事务，回滚逆序撤销。

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"kksresourcemanager/internal/tx"
)

// errEmptyFolder 表示选中的文件夹没有任何可迁移的文件。
//
// 注意：只有空子目录、没有文件的文件夹也归入此类 ——
// 迁移它只会得到「一堆空目录」，正是用户报告的「只生成结构」。
var errEmptyFolder = errors.New("空文件夹")

// ErrConflicts 表示目标侧存在同名项，整批迁移被拒绝。
type ErrConflicts struct {
	Count int
}

func (e *ErrConflicts) Error() string {
	return fmt.Sprintf("目标位置已存在 %d 个同名文件", e.Count)
}

// StepOp 迁移步骤类型。
type StepOp int

const (
	// OpMkdir 建目录（含空目录，保留结构）
	OpMkdir StepOp = iota
	// OpMove 移动文件
	OpMove
	// OpRmdir 清理源侧空目录（自底向上，仅空目录）
	OpRmdir
)

// TransferStep 一条迁移步骤。
type TransferStep struct {
	Op   StepOp
	Src  string // OpMove 时为源文件
	Dst  string // OpMkdir/OpRmdir 为目标目录，OpMove 为目标文件
	Size int64  // OpMove 时的源文件大小（执行前复核）
	// ModTime 规划时的源文件 mtime（UnixNano），执行前复核，
	// 防止规划后被外部修改导致「搬走的是另一份内容」。
	ModTime int64
}

// Conflict 目标侧冲突。
type Conflict struct {
	Src    string
	Dst    string
	Reason string
}

// TransferPlan 一次文件夹迁移的完整计划。
type TransferPlan struct {
	// Steps 顺序：全部 OpMkdir（父先子后）→ OpMove → OpRmdir（子先父后）
	Steps     []TransferStep
	Files     int
	Dirs      int
	Bytes     int64
	Conflicts []Conflict
	Skipped   []string // 符号链接等不处理的项
}

// planFolderTransfer 生成「把 srcDir 整棵子树迁移到 dstDir」的完整计划。
//
// cleanupRoot 是源侧允许清理空壳的边界：该目录本身不会被 OpRmdir，
// 防止把资源根目录（如 UserData/chara/female）整个删掉。
//
// 纯只读：不创建、不移动、不删除任何东西。
func (m *Manager) planFolderTransfer(srcDir, dstDir, cleanupRoot string) (*TransferPlan, error) {
	info, err := os.Stat(srcDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s 不是文件夹", srcDir)
	}

	plan := &TransferPlan{}
	mkdirs := make([]TransferStep, 0, 16)
	moves := make([]TransferStep, 0, 64)
	srcDirs := make([]string, 0, 16)

	err = filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(srcDir, path)
		if relErr != nil {
			return relErr
		}

		// 符号链接一律跳过：跟随链接可能跳出游戏目录，也可能把同一份内容搬两次。
		if d.Type()&os.ModeSymlink != 0 {
			plan.Skipped = append(plan.Skipped, path)
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		dst := dstDir
		if rel != "." {
			dst = filepath.Join(dstDir, rel)
		}

		if d.IsDir() {
			srcDirs = append(srcDirs, path)
			plan.Dirs++
			mkdirs = append(mkdirs, TransferStep{Op: OpMkdir, Dst: dst})
			// 目标已存在同名文件 → 冲突；已存在同名目录 → 允许合并
			// （旧版本可能残留空目录，合并后正好被内容填满）。
			if fi, statErr := os.Stat(dst); statErr == nil && !fi.IsDir() {
				plan.Conflicts = append(plan.Conflicts, Conflict{
					Src: path, Dst: dst, Reason: "目标已存在同名文件",
				})
			}
			return nil
		}

		fi, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		plan.Files++
		plan.Bytes += fi.Size()
		moves = append(moves, TransferStep{
			Op: OpMove, Src: path, Dst: dst,
			Size: fi.Size(), ModTime: fi.ModTime().UnixNano(),
		})
		if _, statErr := os.Stat(dst); statErr == nil {
			plan.Conflicts = append(plan.Conflicts, Conflict{
				Src: path, Dst: dst, Reason: "目标已存在同名文件",
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 没有任何文件：这不是「空文件夹迁移」，而是用户 bug 的现场。
	// 不生成任何 Step，尤其不生成 Mkdir（B1）。
	if plan.Files == 0 {
		return nil, errEmptyFolder
	}

	plan.Steps = append(plan.Steps, mkdirs...)
	plan.Steps = append(plan.Steps, moves...)

	// 源侧空壳清理：自底向上，只对「源根以下」的目录生成 OpRmdir。
	// 执行时再确认目录确实为空；非空（如符号链接留下的内容）则跳过。
	sort.Slice(srcDirs, func(i, j int) bool { return len(srcDirs[i]) > len(srcDirs[j]) })
	for _, dir := range srcDirs {
		if cleanupRoot != "" && samePath(dir, cleanupRoot) {
			continue
		}
		plan.Steps = append(plan.Steps, TransferStep{Op: OpRmdir, Dst: dir})
	}

	return plan, nil
}

// planFileTransfer 为单个文件生成迁移计划。
//
// 目标侧的缺失父目录会各自生成 OpMkdir（父先子后），
// 这样失败回滚时能把这些目录一并回收（B3）。
func (m *Manager) planFileTransfer(src, dst, cleanupRoot string) (*TransferPlan, error) {
	fi, err := os.Stat(src)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("%s 是文件夹，不是文件", src)
	}

	plan := &TransferPlan{Files: 1, Bytes: fi.Size()}

	// 收集目标父目录链上缺失的目录
	missing := make([]string, 0, 4)
	for dir := filepath.Dir(dst); ; dir = filepath.Dir(dir) {
		if fi, statErr := os.Stat(dir); statErr == nil {
			if !fi.IsDir() {
				return nil, fmt.Errorf("目标路径中的 %s 不是目录", dir)
			}
			break
		}
		if cleanupRoot != "" && samePath(dir, cleanupRoot) {
			// 边界目录本身不存在时也要建出来（例如可选的资源根）
			missing = append(missing, dir)
			break
		}
		missing = append(missing, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		plan.Steps = append(plan.Steps, TransferStep{Op: OpMkdir, Dst: missing[i]})
		plan.Dirs++
	}

	plan.Steps = append(plan.Steps, TransferStep{
		Op: OpMove, Src: src, Dst: dst,
		Size: fi.Size(), ModTime: fi.ModTime().UnixNano(),
	})
	if _, statErr := os.Stat(dst); statErr == nil {
		plan.Conflicts = append(plan.Conflicts, Conflict{
			Src: src, Dst: dst, Reason: "目标已存在同名文件",
		})
	}
	return plan, nil
}

// mergeTransferPlans 把多个选中条目的计划合并成一个事务计划。
func mergeTransferPlans(plans []*TransferPlan) *TransferPlan {
	merged := &TransferPlan{}
	for _, p := range plans {
		if p == nil {
			continue
		}
		merged.Steps = append(merged.Steps, p.Steps...)
		merged.Files += p.Files
		merged.Dirs += p.Dirs
		merged.Bytes += p.Bytes
		merged.Conflicts = append(merged.Conflicts, p.Conflicts...)
		merged.Skipped = append(merged.Skipped, p.Skipped...)
	}
	return merged
}

// execTransferPlan 按计划执行，全部成功或全部回滚。
//
// 返回实际移动的文件数、确保存在的目录数、清理掉的空目录数。
// 任一步失败：通过 tx 回滚（文件移回 + 目录回收），并返回
// 「迁移中断，已回滚到操作前状态」或「回滚未完成」的明确错误。
func (m *Manager) execTransferPlan(txn *tx.Tx, plan *TransferPlan, onLog func(level, msg string)) (moved, dirs, dirsRemoved int, err error) {
	logf := func(level, msg string) {
		if onLog != nil {
			onLog(level, msg)
		}
	}

	if txn == nil {
		return 0, 0, 0, fmt.Errorf("无法创建事务，已取消迁移")
	}
	if plan == nil || plan.Files == 0 {
		return 0, 0, 0, errEmptyFolder
	}
	if len(plan.Conflicts) > 0 {
		return 0, 0, 0, &ErrConflicts{Count: len(plan.Conflicts)}
	}

	abort := func(cause error) (int, int, int, error) {
		if rbErr := txn.Rollback(); rbErr != nil {
			return moved, dirs, dirsRemoved, fmt.Errorf(
				"回滚未完成，以下项目需要你手动检查：%v（失败原因：%v）", rbErr, cause)
		}
		return moved, dirs, dirsRemoved, fmt.Errorf(
			"迁移中断，已回滚到操作前状态（失败：%v）。", cause)
	}

	for _, st := range plan.Steps {
		switch st.Op {
		case OpMkdir:
			if fi, statErr := os.Stat(st.Dst); statErr == nil {
				if !fi.IsDir() {
					return abort(fmt.Errorf("目标已存在同名文件: %s", st.Dst))
				}
				dirs++ // 已存在，结构已满足（不重复建，也不记事务）
				continue
			} else if !os.IsNotExist(statErr) {
				return abort(fmt.Errorf("无法访问目标目录 %s: %w", st.Dst, statErr))
			}
			// 曾被「隐藏」删除的目录，先按隐藏索引重建，再记入事务。
			if err := m.ensureDirPath(st.Dst); err != nil {
				return abort(err)
			}
			if err := txn.Mkdir(st.Dst); err != nil {
				return abort(err)
			}
			dirs++
			logf("info", "[建目录] "+st.Dst)

		case OpMove:
			fi, statErr := os.Stat(st.Src)
			if statErr != nil {
				return abort(fmt.Errorf("源文件不可访问 %s: %w", st.Src, statErr))
			}
			if fi.Size() != st.Size || fi.ModTime().UnixNano() != st.ModTime {
				return abort(fmt.Errorf("文件在规划后被修改，已取消：%s", st.Src))
			}
			if _, statErr := os.Stat(st.Dst); statErr == nil {
				return abort(fmt.Errorf("目标已存在: %s", st.Dst))
			}
			if err := m.ensureDirPath(filepath.Dir(st.Dst)); err != nil {
				return abort(err)
			}
			if err := txn.Move(st.Src, st.Dst); err != nil {
				return abort(err)
			}
			moved++
			logf("success", fmt.Sprintf("[移动] %s -> %s", st.Src, st.Dst))

		case OpRmdir:
			removed, rmErr := txn.RmdirIfEmpty(st.Dst)
			if rmErr != nil {
				return abort(rmErr)
			}
			if removed {
				dirsRemoved++
				logf("info", "[清理空目录] "+st.Dst)
			}
		}
	}

	return moved, dirs, dirsRemoved, nil
}

// ensureDirPath 确保目录存在；若它曾被「隐藏」删除，按隐藏索引重建。
func (m *Manager) ensureDirPath(dir string) error {
	if info, err := os.Stat(dir); err == nil {
		if info.IsDir() {
			return nil
		}
		return fmt.Errorf("%s 已存在且不是目录", dir)
	}
	if m.invisible != nil {
		// EnsurePathForOperation 以「目标文件的父目录」为口径，
		// 这里传 dir 的子项，等价于要求 dir 本身存在。
		rebuilt, err := m.invisible.EnsurePathForOperation(filepath.Join(dir, ".ensure"))
		if err != nil {
			return err
		}
		if rebuilt {
			return nil
		}
	}
	return os.MkdirAll(dir, 0755)
}

// dedupeSelectionPaths 去掉「是另一个已选路径后代」的条目，
// 避免父子同时选中时同一文件被规划两次（T8）。
func dedupeSelectionPaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	abs := make([]string, 0, len(paths))
	for _, p := range paths {
		a, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		a = filepath.Clean(a)
		if seen[a] {
			continue
		}
		seen[a] = true
		abs = append(abs, a)
	}
	// 短的（祖先）先处理
	sort.Slice(abs, func(i, j int) bool { return len(abs[i]) < len(abs[j]) })
	kept := make([]string, 0, len(abs))
	for _, p := range abs {
		covered := false
		for _, k := range kept {
			if isUnder(p, k) {
				covered = true
				break
			}
		}
		if !covered {
			kept = append(kept, p)
		}
	}
	return kept
}
