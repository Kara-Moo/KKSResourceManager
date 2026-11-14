package ops

// crud.go —— 自由模式的文件操作
//
// 所有写操作都通过 tx.Tx 执行，保证可撤销（红线 N5）。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kksresourcemanager/internal/fsx"
	"kksresourcemanager/internal/tx"
)

// OpResult 操作结果。
type OpResult struct {
	OK      bool   `json:"ok"`
	Name    string `json:"name"`
	Rel     string `json:"rel"`
	Path    string `json:"path"`
	Message string `json:"message"`
	Code    string `json:"code"`
}

// MkdirResult 新建文件夹结果。
type MkdirResult struct {
	OK      bool   `json:"ok"`
	Name    string `json:"name"` // 实际创建名（可能带 (2)）
	Rel     string `json:"rel"`
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Mkdir 在 parentDir 下新建文件夹。
//
// 返回实际创建的名字（默认名冲突时自动加 (2)）。
func (s *TrashStore) Mkdir(txn *tx.Tx, parentDir, baseName string) MkdirResult {
	res := MkdirResult{}

	if parentDir == "" {
		res.Code = fsx.ErrIOError
		res.Message = "未指定目标目录。"
		return res
	}
	if err := os.MkdirAll(parentDir, 0755); err != nil {
		res.Code = fsx.ErrIOError
		res.Message = "创建目录失败：" + err.Error()
		return res
	}

	name, err := fsx.UniqueFolderName(parentDir, baseName)
	if err != nil {
		res.Code = fsx.CodeOf(err)
		res.Message = err.Error()
		return res
	}

	target := filepath.Join(parentDir, name)
	if txn != nil {
		if err := txn.Mkdir(target); err != nil {
			res.Code = fsx.ErrIOError
			res.Message = err.Error()
			return res
		}
	} else {
		if err := os.MkdirAll(target, 0755); err != nil {
			res.Code = fsx.ErrIOError
			res.Message = err.Error()
			return res
		}
	}

	res.OK = true
	res.Name = name
	res.Path = target
	return res
}

// Rename 重命名一个条目。
//
// 返回是否需要确认扩展名变更。
func (s *TrashStore) Rename(txn *tx.Tx, path, newName string) OpResult {
	res := OpResult{}
	dir := filepath.Dir(path)
	oldName := filepath.Base(path)

	if err := fsx.ValidateName(dir, newName, oldName); err != nil {
		res.Code = fsx.CodeOf(err)
		res.Message = err.Error()
		return res
	}

	target := filepath.Join(dir, newName)
	if txn != nil {
		if err := txn.Rename(path, target); err != nil {
			res.Code = fsx.ErrIOError
			res.Message = err.Error()
			return res
		}
	} else {
		if err := os.Rename(path, target); err != nil {
			res.Code = fsx.ErrIOError
			res.Message = err.Error()
			return res
		}
	}

	res.OK = true
	res.Name = newName
	res.Path = target
	return res
}

// DeletePlan 删除计划（两阶段：先出计划，确认后执行）。
type DeletePlan struct {
	PlanID    string        `json:"planId"`
	Items     []DeleteItem  `json:"items"`
	TotalBytes int64        `json:"totalBytes"`
	Conflicts []Conflict    `json:"conflicts"`
}

// DeleteItem 计划中的一个待删除项。
type DeleteItem struct {
	// Src 源绝对路径
	Src string `json:"src"`
	// TrashRel DELED 下的相对路径
	TrashRel string `json:"trashRel"`
	// Spec 种类
	Spec string `json:"spec"`
	// Size 大小
	Size int64 `json:"size"`
	// IsDir 是否为目录
	IsDir bool `json:"isDir"`
	// Name 显示名
	Name string `json:"name"`
}

// Conflict 冲突项。
type Conflict struct {
	Src    string `json:"src"`
	Dst    string `json:"dst"`
	Reason string `json:"reason"`
}

// PlanDelete 生成删除计划（零副作用）。
//
// specOf 返回 (种类, 该种类的根目录, 是否受管)。
// 根目录用于计算原相对路径，使回收站保留原有目录结构（v2.2 FIX-07）。
func (s *TrashStore) PlanDelete(paths []string, specOf func(abs string) (string, string, bool)) DeletePlan {
	plan := DeletePlan{Items: make([]DeleteItem, 0, len(paths))}

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			plan.Conflicts = append(plan.Conflicts, Conflict{
				Src: p, Reason: "无法访问：" + err.Error(),
			})
			continue
		}

		spec, root, ok := specOf(p)
		if !ok {
			plan.Conflicts = append(plan.Conflicts, Conflict{
				Src: p, Reason: "不属于任何受管资源类型，无法删除",
			})
			continue
		}

		name := filepath.Base(p)
		size := info.Size()
		if info.IsDir() {
			size = DirSize(p)
		}

		// 目标：DELED/<spec>/<原相对路径>（含文件名本身）。
		//
		// 早前单文件用 UniqueTrashPath(spec, name) —— 只取文件名，
		// 丢掉了 22.7.27/ 这类子目录；不同目录的同名文件只能靠 __2 区分，
		// 回收站里完全看不出原本在哪。
		relSrc, relErr := filepath.Rel(root, p)
		if relErr != nil || relSrc == "" || strings.HasPrefix(relSrc, "..") {
			relSrc = name
		}
		trashAbs := filepath.Join(s.SpecDir(spec), filepath.FromSlash(filepath.ToSlash(relSrc)))
		// 只有真的撞上了才加后缀，不再无条件 flatten
		if _, statErr := os.Stat(trashAbs); statErr == nil {
			trashAbs = uniqueTrashSuffix(trashAbs)
		}

		rel, relErr := filepath.Rel(s.dir, trashAbs)
		if relErr != nil {
			rel = filepath.Base(trashAbs)
		}
		plan.Items = append(plan.Items, DeleteItem{
			Src:      p,
			TrashRel: filepath.ToSlash(rel),
			Spec:     spec,
			Size:     size,
			IsDir:    info.IsDir(),
			Name:     name,
		})
		plan.TotalBytes += size
	}

	return plan
}

// uniqueTrashSuffix 只在目标路径真的已存在时生成 __2/__3… 后缀。
//
// 与 UniqueTrashPath 的区别：它作用于**完整相对路径**（含子目录），
// 不再把不同目录的同名文件 flatten 到同一层。
func uniqueTrashSuffix(path string) string {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 2; i <= 99; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s__%d%s", stem, i, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return filepath.Join(dir, fmt.Sprintf("%s__%d%s", stem, time.Now().UnixNano()%100000, ext))
}

// CommitDelete 执行删除：把条目移入回收站。
//
// txn 为 nil 时直接移动（不提供撤销能力）。
func (s *TrashStore) CommitDelete(txn *tx.Tx, plan DeletePlan) (int, []string) {
	var errs []string
	moved := 0

	for _, item := range plan.Items {
		dst := filepath.Join(s.dir, filepath.FromSlash(item.TrashRel))
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			errs = append(errs, fmt.Sprintf("%s: 创建目标目录失败 %v", item.Name, err))
			continue
		}
		var err error
		if txn != nil {
			err = txn.Move(item.Src, dst)
		} else {
			err = os.Rename(item.Src, dst)
			if err != nil {
				err = copyAndRemove(item.Src, dst)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", item.Name, err))
			continue
		}
		// 记录元数据（可还原的必需信息）
		meta := TrashMeta{
			OriginalAbs: item.Src,
			Spec:        item.Spec,
			DeletedAt:   time.Now().UnixMilli(),
			Size:        item.Size,
			IsDir:       item.IsDir,
		}
		if txn != nil {
			meta.TxID = txn.ID
		}
		if err := s.Put(item.TrashRel, meta); err != nil {
			errs = append(errs, fmt.Sprintf("%s: 写入回收站元数据失败 %v", item.Name, err))
		}
		moved++
	}

	return moved, errs
}

// copyAndRemove 跨设备移动的降级方案。
func copyAndRemove(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		if err := copyFileTo(src, dst, info.Mode().Perm()); err != nil {
			return err
		}
		return os.Remove(src)
	}
	if err := copyTreeTo(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

// copyFileTo 复制单个文件（不覆盖）。
func copyFileTo(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
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

// copyTreeTo 复制整个目录树。
func copyTreeTo(src, dst string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyTreeTo(s, d); err != nil {
				return err
			}
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if err := copyFileTo(s, d, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// RestoreResult 还原结果。
type RestoreResult struct {
	Restored  int        `json:"restored"`
	Skipped   int        `json:"skipped"`
	Conflicts []Conflict `json:"conflicts"`
}

// Restore 把回收站条目移回原位。
//
// 原父目录不存在则创建；目标已存在则记为冲突。
func (s *TrashStore) Restore(txn *tx.Tx, trashRels []string) RestoreResult {
	res := RestoreResult{Conflicts: make([]Conflict, 0)}

	meta, err := s.Load()
	if err != nil {
		res.Conflicts = append(res.Conflicts, Conflict{Reason: err.Error()})
		return res
	}

	for _, rel := range trashRels {
		m, ok := meta[rel]
		if !ok {
			res.Skipped++
			res.Conflicts = append(res.Conflicts, Conflict{
				Src: rel, Reason: "未找到回收站记录",
			})
			continue
		}

		src := filepath.Join(s.dir, filepath.FromSlash(rel))
		if _, err := os.Stat(src); err != nil {
			res.Skipped++
			res.Conflicts = append(res.Conflicts, Conflict{
				Src: src, Reason: "回收站中的文件不存在",
			})
			continue
		}

		dst := m.OriginalAbs
		if _, err := os.Stat(dst); err == nil {
			// 目标已存在 → 冲突
			res.Skipped++
			res.Conflicts = append(res.Conflicts, Conflict{
				Src: src, Dst: dst, Reason: "目标已存在",
			})
			continue
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			res.Skipped++
			res.Conflicts = append(res.Conflicts, Conflict{
				Src: src, Dst: dst, Reason: "创建原目录失败：" + err.Error(),
			})
			continue
		}

		var mvErr error
		if txn != nil {
			mvErr = txn.Move(src, dst)
		} else {
			mvErr = os.Rename(src, dst)
			if mvErr != nil {
				mvErr = copyAndRemove(src, dst)
			}
		}
		if mvErr != nil {
			res.Skipped++
			res.Conflicts = append(res.Conflicts, Conflict{
				Src: src, Dst: dst, Reason: mvErr.Error(),
			})
			continue
		}

		s.Remove(rel)
		res.Restored++
	}

	return res
}

// PurgeResult 彻底删除结果。
type PurgeResult struct {
	Purged int        `json:"purged"`
	Errors []string  `json:"errors"`
}

// Purge 彻底删除回收站条目。
//
// 这是全项目**唯一**允许真删的地方（红线 N4 的唯一例外）。
// 调用方必须已完成二次确认 + 输入名称校验。
func (s *TrashStore) Purge(trashRels []string) PurgeResult {
	res := PurgeResult{Errors: make([]string, 0)}

	for _, rel := range trashRels {
		src := filepath.Join(s.dir, filepath.FromSlash(rel))
		if _, err := os.Stat(src); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: 文件不存在", rel))
			continue
		}
		if err := os.RemoveAll(src); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", rel, err))
			continue
		}
		s.Remove(rel)
		res.Purged++
	}
	return res
}
