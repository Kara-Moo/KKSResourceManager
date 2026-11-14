// Package tx 实现文件操作的事务引擎。
//
// 重构规格 §8 的核心约束（红线 N5）：
//
//	每个写操作必须可回溯 —— 没有 journal 的写操作一律不允许合并。
//	撤销失败必须说明**具体哪一步**、**哪个文件**。
//
// 设计要点：
//
//   - 每个 Move/Copy/Mkdir/Rename 的**第一步**就把 intent 整体写盘
//     （marshal 整个 Tx + f.Sync()），然后才执行。
//     这样进程被杀时，journal 里留有「意图」，启动时可自动回滚。
//
//   - journal 目录：DELCARDS/_journal/<txId>/journal.json
//     txId 格式：20261113-180233-a3f2（本地时间 + 4 位随机 hex），
//     字典序即时间序。
//
//   - 步数上限 500，单次写入 < 200KB，可接受。
package tx

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// JournalDirName journal 目录名（位于 DELCARDS_ROOT 下）。
const JournalDirName = "_journal"

// StateDirName 状态目录名（位于 DELCARDS_ROOT 下）。
const StateDirName = "_state"

// HistoryFileName 历史文件名。
const HistoryFileName = "history.json"

// MaxSteps 单个事务的最大步数。
const MaxSteps = 500

// MaxHistory 历史记录保留条数。
const MaxHistory = 200

// Step 事务中的一个步骤。
type Step struct {
	// Kind move | copy | mkdir | rename | trash
	Kind string `json:"kind"`
	// Src 源路径
	Src string `json:"src"`
	// Dst 目标路径
	Dst string `json:"dst"`
	// SrcFP 执行前的指纹（撤销校验用）
	SrcFP FP `json:"srcFp"`
	// DstFP 执行后的指纹
	DstFP FP `json:"dstFp"`
	// State intent | done | rolledback
	State string `json:"state"`
	// Err 失败原因
	Err string `json:"err,omitempty"`
}

// FP 文件指纹。
type FP struct {
	Size    int64 `json:"size"`
	ModTime int64 `json:"mtime"`
}

// Equal 比较两个指纹。
func (f FP) Equal(o FP) bool {
	return f.Size == o.Size && f.ModTime == o.ModTime
}

// Tx 一个事务。
type Tx struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Title     string   `json:"title"`
	StartedAt int64    `json:"startedAt"`
	Steps     []Step   `json:"steps"`
	State     string   `json:"state"` // open | committed | rolledback
	Logs      []string `json:"logs"`

	// root journal 根目录（DELCARDS/_journal）
	root string
	// mu 保护并发访问
	mu sync.Mutex
}

// Begin 开启一个事务。
//
// root 为 DELCARDS_ROOT。
func Begin(root, kind, title string) (*Tx, error) {
	id := newTxID()
	t := &Tx{
		ID:        id,
		Kind:      kind,
		Title:     title,
		StartedAt: time.Now().UnixMilli(),
		State:     "open",
		Logs:      make([]string, 0, 8),
		root:      filepath.Join(root, JournalDirName),
	}
	if err := os.MkdirAll(filepath.Join(t.root, id), 0755); err != nil {
		return nil, fmt.Errorf("创建事务目录失败: %w", err)
	}
	// 先落一次盘，保证事务目录存在时就有可恢复的记录
	if err := t.persist(); err != nil {
		return nil, err
	}
	return t, nil
}

// newTxID 生成事务 ID：本地时间 + 4 位随机 hex。
func newTxID() string {
	return time.Now().Format("20060102-150405") + "-" + randomHex(4)
}

// randomHex 生成 n 位十六进制随机串。
func randomHex(n int) string {
	const hexdigits = "0123456789abcdef"
	b := make([]byte, n)
	buf := make([]byte, n)
	if _, err := os.ReadFile("/dev/urandom"); err == nil {
		// 非 Windows
		for i := range b {
			b[i] = hexdigits[int(buf[i])%16]
		}
		return string(b)
	}
	// 退化方案：用时间纳秒
	ns := time.Now().UnixNano()
	for i := range b {
		b[i] = hexdigits[int(ns>>(i*4))%16]
	}
	return string(b)
}

// journalPath 返回 journal 文件路径。
func (t *Tx) journalPath() string {
	return filepath.Join(t.root, t.ID, "journal.json")
}

// persist 把当前事务状态原子写盘。
func (t *Tx) persist() error {
	dir := filepath.Dir(t.journalPath())
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化事务失败: %w", err)
	}
	tmp := t.journalPath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, t.journalPath())
}

// logf 记录一条事务日志。
func (t *Tx) logf(format string, args ...interface{}) {
	t.Logs = append(t.Logs, fmt.Sprintf(format, args...))
}

// fingerprintOf 取文件指纹。
func fingerprintOf(path string) (FP, error) {
	info, err := os.Stat(path)
	if err != nil {
		return FP{}, err
	}
	return FP{Size: info.Size(), ModTime: info.ModTime().UnixNano()}, nil
}

// addStep 追加一个步骤（intent 状态）。
func (t *Tx) addStep(kind, src, dst string) *Step {
	t.Steps = append(t.Steps, Step{
		Kind:  kind,
		Src:   src,
		Dst:   dst,
		State: "intent",
	})
	return &t.Steps[len(t.Steps)-1]
}

// markDone 把最后一个步骤标记为 done 并记录目标指纹。
func (t *Tx) markDone(srcFP FP) {
	if len(t.Steps) == 0 {
		return
	}
	step := &t.Steps[len(t.Steps)-1]
	step.State = "done"
	step.SrcFP = srcFP
	if fp, err := fingerprintOf(step.Dst); err == nil {
		step.DstFP = fp
	}
}

// markFailed 把最后一个步骤标记为失败。
func (t *Tx) markFailed(err error) {
	if len(t.Steps) == 0 {
		return
	}
	step := &t.Steps[len(t.Steps)-1]
	step.State = "done"
	if err != nil {
		step.Err = err.Error()
	}
}

// Move 移动文件或目录。
//
// 流程：写 intent → 执行 → 标记 done → 再写盘。
func (t *Tx) Move(src, dst string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.State != "open" {
		return fmt.Errorf("事务已 %s，不能继续操作", t.State)
	}
	if len(t.Steps) >= MaxSteps {
		return fmt.Errorf("事务步数超过上限 %d", MaxSteps)
	}

	srcFP, err := fingerprintOf(src)
	if err != nil {
		return fmt.Errorf("源文件不可访问 %s: %w", src, err)
	}

	t.addStep("move", src, dst)
	// 第一步就落盘 intent —— 进程此刻被杀也能恢复
	if err := t.persist(); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		t.markFailed(err)
		return fmt.Errorf("创建目标目录失败 %s: %w", filepath.Dir(dst), err)
	}
	if _, err := os.Stat(dst); err == nil {
		err := fmt.Errorf("目标已存在: %s", dst)
		t.markFailed(err)
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		// 跨设备：退化为复制 + 删除
		if err := copyTree(src, dst); err != nil {
			t.markFailed(err)
			return fmt.Errorf("移动失败 %s -> %s: %w", src, dst, err)
		}
		if err := os.RemoveAll(src); err != nil {
			t.markFailed(err)
			return fmt.Errorf("移动失败（源删除失败） %s: %w", src, err)
		}
	}

	t.markDone(srcFP)
	if err := t.persist(); err != nil {
		return err
	}
	t.logf("移动 %s -> %s", src, dst)
	return nil
}

// Copy 复制文件或目录。
func (t *Tx) Copy(src, dst string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.State != "open" {
		return fmt.Errorf("事务已 %s，不能继续操作", t.State)
	}
	if len(t.Steps) >= MaxSteps {
		return fmt.Errorf("事务步数超过上限 %d", MaxSteps)
	}

	srcFP, err := fingerprintOf(src)
	if err != nil {
		return fmt.Errorf("源文件不可访问 %s: %w", src, err)
	}

	t.addStep("copy", src, dst)
	if err := t.persist(); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		t.markFailed(err)
		return fmt.Errorf("创建目标目录失败 %s: %w", filepath.Dir(dst), err)
	}
	if _, err := os.Stat(dst); err == nil {
		err := fmt.Errorf("目标已存在: %s", dst)
		t.markFailed(err)
		return err
	}
	if err := copyTree(src, dst); err != nil {
		t.markFailed(err)
		return fmt.Errorf("复制失败 %s -> %s: %w", src, dst, err)
	}

	t.markDone(srcFP)
	if err := t.persist(); err != nil {
		return err
	}
	t.logf("复制 %s -> %s", src, dst)
	return nil
}

// Mkdir 创建目录。
func (t *Tx) Mkdir(dir string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.State != "open" {
		return fmt.Errorf("事务已 %s，不能继续操作", t.State)
	}

	t.addStep("mkdir", "", dir)
	if err := t.persist(); err != nil {
		return err
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		t.markFailed(err)
		return fmt.Errorf("创建目录失败 %s: %w", dir, err)
	}

	t.markDone(FP{})
	if err := t.persist(); err != nil {
		return err
	}
	t.logf("创建目录 %s", dir)
	return nil
}

// RmdirIfEmpty 删除空目录，并把这一步记入事务。
//
// 目录非空时不做任何事、不记步骤（返回 false, nil）——
// 源侧可能还有符号链接或用户后来放入的文件，绝不能强删。
// 回滚时该步骤会重建目录。
func (t *Tx) RmdirIfEmpty(dir string) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.State != "open" {
		return false, fmt.Errorf("事务已 %s，不能继续操作", t.State)
	}
	if len(t.Steps) >= MaxSteps {
		return false, fmt.Errorf("事务步数超过上限 %d", MaxSteps)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("读取目录失败 %s: %w", dir, err)
	}
	if len(entries) > 0 {
		return false, nil
	}

	t.addStep("rmdir", "", dir)
	if err := t.persist(); err != nil {
		return false, err
	}

	if err := os.Remove(dir); err != nil {
		t.markFailed(err)
		return false, fmt.Errorf("删除空目录失败 %s: %w", dir, err)
	}

	t.markDone(FP{})
	if err := t.persist(); err != nil {
		return false, err
	}
	t.logf("删除空目录 %s", dir)
	return true, nil
}

// Rename 重命名。
func (t *Tx) Rename(src, dst string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.State != "open" {
		return fmt.Errorf("事务已 %s，不能继续操作", t.State)
	}

	srcFP, err := fingerprintOf(src)
	if err != nil {
		return fmt.Errorf("源文件不可访问 %s: %w", src, err)
	}

	t.addStep("rename", src, dst)
	if err := t.persist(); err != nil {
		return err
	}

	if _, err := os.Stat(dst); err == nil {
		err := fmt.Errorf("目标已存在: %s", dst)
		t.markFailed(err)
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		t.markFailed(err)
		return fmt.Errorf("重命名失败 %s -> %s: %w", src, dst, err)
	}

	t.markDone(srcFP)
	if err := t.persist(); err != nil {
		return err
	}
	t.logf("重命名 %s -> %s", src, dst)
	return nil
}

// Commit 提交事务：删除 journal 目录，写入历史。
func (t *Tx) Commit(root string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.State != "open" {
		return fmt.Errorf("事务已 %s，不能提交", t.State)
	}
	t.State = "committed"

	// 删除 journal
	os.RemoveAll(filepath.Join(t.root, t.ID))

	// 写历史
	if err := AppendHistory(root, HistoryEntry{
		TxID:     t.ID,
		Kind:     t.Kind,
		Title:    t.Title,
		At:       time.Now().UnixMilli(),
		Affected: len(t.Steps),
		Status:   "committed",
		Steps:    append([]Step(nil), t.Steps...),
	}); err != nil {
		return err
	}
	return nil
}

// Rollback 回滚事务：逆序撤销所有已完成的步骤。
//
// 全部成功才标记 rolledback；部分失败时保留 journal 并返回错误，
// 说明具体哪一步、哪个文件失败。
func (t *Tx) Rollback() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.State == "rolledback" {
		return nil
	}
	if t.State == "committed" {
		return fmt.Errorf("事务已提交，不能回滚")
	}

	var failures []string
	for i := len(t.Steps) - 1; i >= 0; i-- {
		step := t.Steps[i]
		if step.State != "done" {
			continue
		}
		if err := rollbackStep(&step); err != nil {
			step.Err = err.Error()
			failures = append(failures, fmt.Sprintf("第 %d 步(%s) %s: %v", i+1, step.Kind, step.Src, err))
			continue
		}
		step.State = "rolledback"
	}

	if len(failures) > 0 {
		// 保留 journal 供人工处理
		t.persist()
		return fmt.Errorf("回滚失败（%d 步）：%s", len(failures), strings.Join(failures, "; "))
	}

	t.State = "rolledback"
	os.RemoveAll(filepath.Join(t.root, t.ID))
	return nil
}

// rollbackStep 回滚单个步骤。
func rollbackStep(step *Step) error {
	switch step.Kind {
	case "move", "rename":
		// 目标移回源
		if _, err := os.Stat(step.Dst); err != nil {
			// 目标已不存在 → 视为已回滚
			return nil
		}
		if _, err := os.Stat(step.Src); err == nil {
			// 源已被占用 → 移到冲突名
			conflict := step.Src + ".conflict-" + shortID()
			if err := os.Rename(step.Dst, conflict); err != nil {
				return fmt.Errorf("源被占用且无法移到 %s: %w", conflict, err)
			}
			return fmt.Errorf("源已被占用，已移到 %s", conflict)
		}
		if err := os.MkdirAll(filepath.Dir(step.Src), 0755); err != nil {
			return err
		}
		return os.Rename(step.Dst, step.Src)

	case "copy":
		// 删除本事务创建的副本
		if _, err := os.Stat(step.Dst); err != nil {
			return nil
		}
		return os.RemoveAll(step.Dst)

	case "mkdir":
		// 仅删空目录；非空则保留并告警（不阻断其它回滚）
		if _, err := os.Stat(step.Dst); err != nil {
			return nil
		}
		if err := os.Remove(step.Dst); err != nil {
			return fmt.Errorf("目录非空，未删除: %w", err)
		}
		return nil

	case "rmdir":
		// 重建被本事务删除的空目录
		if info, err := os.Stat(step.Dst); err == nil && info.IsDir() {
			return nil
		}
		return os.MkdirAll(step.Dst, 0755)

	case "trash":
		// 从回收站移回原位
		if _, err := os.Stat(step.Dst); err != nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(step.Src), 0755); err != nil {
			return err
		}
		return os.Rename(step.Dst, step.Src)
	}
	return nil
}

// shortID 生成 6 位短 ID（用于冲突名）。
func shortID() string {
	return randomHex(6)
}

// copyTree 复制文件或整个目录树。
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dst, info.Mode().Perm())
	}
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
			if err := copyTree(s, d); err != nil {
				return err
			}
			continue
		}
		ci, err := e.Info()
		if err != nil {
			return err
		}
		if err := copyFile(s, d, ci.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// copyFile 复制单个文件（绝不覆盖已存在的目标）。
func copyFile(src, dst string, perm os.FileMode) error {
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

// LoadPending 加载未提交的事务（崩溃恢复用）。
//
// root 为 DELCARDS_ROOT。
func LoadPending(root string) ([]*Tx, error) {
	journalRoot := filepath.Join(root, JournalDirName)
	entries, err := os.ReadDir(journalRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var result []*Tx
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		path := filepath.Join(journalRoot, name, "journal.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var t Tx
		if err := json.Unmarshal(data, &t); err != nil {
			continue
		}
		t.root = journalRoot
		result = append(result, &t)
	}
	return result, nil
}

// RecoverPending 自动回滚所有未完成的事务。
//
// 返回回滚结果摘要与错误列表。
func RecoverPending(root string) (recovered int, errs []string) {
	pending, err := LoadPending(root)
	if err != nil {
		return 0, []string{err.Error()}
	}
	for _, t := range pending {
		if err := t.Rollback(); err != nil {
			errs = append(errs, fmt.Sprintf("%s(%s): %v", t.ID, t.Kind, err))
			continue
		}
		recovered++
	}
	return recovered, errs
}
