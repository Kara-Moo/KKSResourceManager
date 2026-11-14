package main

// manager.go —— KKS 资源管理器核心
//
// 设计红线（对应需求中的“绝对不破坏全局依赖 / 绝对不破坏原有目录结构”）：
//
//  1. 隔离卡片时保留原始相对目录结构，先创建目标目录再移动文件；
//     源目录即使变空也绝不删除。
//  2. 恢复时把文件放回原位（目录不存在则重建）；DEL 区空目录可清理，游戏目录永不删除。

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"kksresourcemanager/internal/core"
	"kksresourcemanager/internal/index"
	"kksresourcemanager/internal/preview"
	"kksresourcemanager/internal/tx"
)

// ---------------------------------------------------------------------------
// 常量与类型定义
// ---------------------------------------------------------------------------

// 资源类型标识。前端 mode-select 与后端保持一致。
const (
	ModeFemaleCard = "female_card"
	ModeMaleCard   = "male_card"
	ModeScene      = "scene"
	ModePose       = "pose"
	ModeCoordF     = "coord_f"
	ModeCoordM     = "coord_m"
)

// ResourceKind 描述一种资源在游戏目录中的位置及其隔离区。
type ResourceKind struct {
	Mode string
	// Name 中文显示名
	Name string
	// SourceRel 相对于 KKS_ROOT 的资源目录
	SourceRel string
	// DelName DELCARDS_ROOT 下的隔离目录名
	DelName string
	// FileExt 该类型资源的文件扩展名（小写，含点）
	FileExt string
}

// ResourceKinds 是全部 6 种受管资源类型。
// 顺序即前端下拉框顺序。
// 注意：不含「工作室角色」(UserData/Studio/character) —— 该路径不参与扫描，
// 也不创建对应的隔离目录。
var ResourceKinds = []ResourceKind{
	{ModeFemaleCard, "角色卡 (女)", filepath.Join("UserData", "chara", "female"), "DEL-female_card", ".png"},
	{ModeMaleCard, "角色卡 (男)", filepath.Join("UserData", "chara", "male"), "DEL-male_card", ".png"},
	{ModeScene, "场景卡", filepath.Join("UserData", "Studio", "scene"), "DEL-scene", ".png"},
	{ModePose, "POSE 动作", filepath.Join("UserData", "Studio", "pose"), "DEL-pose", ".png"},
	{ModeCoordF, "女性服装", filepath.Join("UserData", "coordinate", "female"), "DEL-coord_f", ".png"},
	{ModeCoordM, "男性服装", filepath.Join("UserData", "coordinate", "male"), "DEL-coord_m", ".png"},
}

// delCardsDirName 隔离区根目录名，位于 KKS_ROOT 的父目录下
const delCardsDirName = "DELCARDS"

// KindByMode 按 mode 取资源类型定义。
func KindByMode(mode string) (ResourceKind, bool) {
	for _, k := range ResourceKinds {
		if k.Mode == mode {
			return k, true
		}
	}
	return ResourceKind{}, false
}

// ---------------------------------------------------------------------------
// Manager
// ---------------------------------------------------------------------------

// Manager 持有全局扫描结果与隔离/恢复的核心状态。
type Manager struct {
	mu sync.RWMutex

	// KksRoot 游戏根目录，例如 E:\KKV114
	KksRoot string
	// DelCardsRoot 隔离区根目录 = KKS_ROOT/../DELCARDS
	DelCardsRoot string

	// Scanned 是否已完成一次全量扫描
	Scanned bool

	// index 持久化索引（卡片指纹）
	index *index.IndexData

	// thumbs 缩略图生成队列（两级缓存 + 有界并发）
	thumbs *preview.Queue
	// snapshots 目录快照缓存（LRU 128）
	snapshots *core.SnapshotCache
	// covers 封面卡缓存（LRU 512）
	covers *core.CoverCache
	// fileCounts / dirSizes 统计缓存
	fileCounts *countCache
	dirSizes   *sizeCache

	// clipboard 复制/剪切/粘贴剪贴板
	clipboard *Clipboard

	// currentTx 当前进行中的事务（写操作走事务，红线 N5）
	currentTx *tx.Tx
	// invisible 空文件夹隐藏状态索引
	invisible *InvisibleStore
}

// NewManager 创建 Manager 并初始化隔离区目录结构（只增不删）。
func NewManager(kksRoot string) (*Manager, error) {
	kksRoot = strings.TrimSpace(kksRoot)
	if kksRoot == "" {
		return nil, fmt.Errorf("KKS 根目录不能为空")
	}
	abs, err := filepath.Abs(kksRoot)
	if err != nil {
		return nil, fmt.Errorf("解析 KKS 根目录失败: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("无法访问 KKS 根目录: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("KKS 根目录不是文件夹: %s", abs)
	}

	parent := filepath.Dir(abs)
	m := &Manager{
		KksRoot:      abs,
		DelCardsRoot: filepath.Join(parent, delCardsDirName),
		clipboard:    &Clipboard{},
		snapshots:    core.NewSnapshotCache(128),
		covers:       core.NewCoverCache(512),
		fileCounts:   newCountCache(),
		dirSizes:     newSizeCache(),
	}
	if err := m.EnsureDelDirs(); err != nil {
		return nil, err
	}
	// 隐藏状态目录位于 DELCARDS 内，需在隔离目录创建后初始化
	inv, err := NewInvisibleStore(m.DelCardsRoot, abs)
	if err != nil {
		return nil, err
	}
	m.invisible = inv
	return m, nil
}

// EnsureDelDirs 创建隔离区及其全部子目录。已存在的目录不会受到影响。
func (m *Manager) EnsureDelDirs() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensureDelDirsLocked()
}

func (m *Manager) ensureDelDirsLocked() error {
	if err := os.MkdirAll(m.DelCardsRoot, 0755); err != nil {
		return fmt.Errorf("创建隔离区失败: %w", err)
	}
	names := make([]string, 0, len(ResourceKinds))
	for _, k := range ResourceKinds {
		names = append(names, k.DelName)
	}
	for _, name := range names {
		p := filepath.Join(m.DelCardsRoot, name)
		if err := os.MkdirAll(p, 0755); err != nil {
			return fmt.Errorf("创建隔离目录 %s 失败: %w", name, err)
		}
	}
	return nil
}

// RequiredDirs 是扫描所必需、缺失即判定为「加载错误」的目录。
//
// 判定依据：这些目录承载了核心资源，缺失说明选错了根目录。
// 注意 coordinate/male 属于可选目录 —— 很多 KKS 安装根本没有男装资源库，
// 其缺失只应导致该类型列表为空，而不应阻断整个程序。
var RequiredDirs = []string{
	filepath.Join("UserData", "chara", "female"),
	filepath.Join("UserData", "chara", "male"),
	filepath.Join("UserData", "Studio", "scene"),
	filepath.Join("UserData", "Studio", "pose"),
	filepath.Join("UserData", "coordinate", "female"),
	"mods",
}

// ValidateRoot 校验 KKS 根目录结构是否满足扫描要求。
//
// 必须存在全部 RequiredDirs 目录，否则前端应提示「加载错误」
// 而不是在一个错误的路径上做无意义的扫描。
//
// 返回缺失的必需目录列表；列表为空表示校验通过。
func (m *Manager) ValidateRoot() (missing []string, err error) {
	if m.KksRoot == "" {
		return nil, fmt.Errorf("尚未选择 KKS 根目录")
	}
	info, statErr := os.Stat(m.KksRoot)
	if statErr != nil {
		return nil, fmt.Errorf("无法访问 KKS 根目录: %w", statErr)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("KKS 根目录不是文件夹: %s", m.KksRoot)
	}

	for _, rel := range RequiredDirs {
		dir := filepath.Join(m.KksRoot, rel)
		if stat, e := os.Stat(dir); e != nil || !stat.IsDir() {
			missing = append(missing, rel)
		}
	}
	return missing, nil
}

// SourceDir 返回指定类型的资源源目录。
func (m *Manager) SourceDir(kind ResourceKind) string {
	return filepath.Join(m.KksRoot, kind.SourceRel)
}

// DelDir 返回指定类型的隔离目录。
func (m *Manager) DelDir(kind ResourceKind) string {
	return filepath.Join(m.DelCardsRoot, kind.DelName)
}

// ---------------------------------------------------------------------------
// 全局扫描
// ---------------------------------------------------------------------------

// ScanResult 汇总一次全量扫描的结果。
type ScanResult struct {
	ResourceCount  int
	FailedFiles    int
	ScanDurationMS int64
	// Reused 本次复用的索引条目数（零 IO）
	Reused int
	// Parsed 本次实际解析的文件数
	Parsed int
	// Removed 从索引移除的条目数
	Removed int
	// ScanWarning 保存索引时的警告（不影响本次使用）
	ScanWarning string
	// IndexUpgradeNote 旧版本索引升级提示（v1 → v2 时非空）
	IndexUpgradeNote string
}

// ScanAll 执行全量扫描：遍历 6 类资源目录下的资源文件，建立索引。
//
// progress 回调会收到阶段名（resource）、已完成数与总数，供前端渲染进度条。
func (m *Manager) ScanAll(progress func(phase string, done, total int)) ScanResult {
	m.mu.Lock()
	defer m.mu.Unlock()

	start := nowMS()
	result := ScanResult{}

	// --- 准备索引存储与扫描器 ---
	store, err := index.NewStore(m.DelCardsRoot)
	if err != nil {
		result.FailedFiles = 1
		result.ScanDurationMS = nowMS() - start
		return result
	}
	// 加载旧索引（不存在/损坏时全量重建；v1 旧版本作废但保留文件）
	lr, _ := store.Load(m.KksRoot)
	old := lr.Index
	if lr.Upgraded {
		result.IndexUpgradeNote = fmt.Sprintf(
			"索引格式已升级（v%d → v%d），本次为全量重建。", lr.OldVersion, index.SchemaVersion)
	}

	scanner := &index.Scanner{
		Root:          m.KksRoot,
		ResourceRoots: m.resourceRoots(),
	}

	// --- 执行增量扫描 ---
	idx, stats, scanErr := scanner.Scan(context.Background(), old, progress)
	if scanErr != nil {
		result.FailedFiles = 1
		result.ScanDurationMS = nowMS() - start
		return result
	}

	// --- 原子保存索引（失败只记警告，不影响本次使用） ---
	if err := store.Save(idx); err != nil {
		result.ScanWarning = err.Error()
	}

	// --- 写入内存状态 ---
	m.applyIndex(idx)

	result.ResourceCount = len(idx.Cards)
	result.FailedFiles = stats.Failed
	result.ScanDurationMS = nowMS() - start
	result.Reused = stats.Reused
	result.Parsed = stats.Parsed
	result.Removed = stats.Removed
	return result
}

// resourceRoots 返回各资源类型的源目录映射。
func (m *Manager) resourceRoots() map[string]string {
	roots := make(map[string]string, len(ResourceKinds))
	for _, k := range ResourceKinds {
		roots[k.Mode] = m.SourceDir(k)
	}
	return roots
}

// applyIndex 把索引结果写入 Manager 的内存状态。
func (m *Manager) applyIndex(idx *index.IndexData) {
	m.index = idx
	m.Scanned = true
}

// collectFiles 递归收集目录下所有满足过滤条件的文件。
// 符号链接会被跳过，避免跟随链接跳出游戏目录。
func (m *Manager) collectFiles(root string, keep func(path string) bool) []string {
	var files []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// 目录不存在等情况直接跳过
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if keep(path) {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files
}

// collectFilesStrict 递归收集目录下的文件，遇到读取错误时返回错误。
//
// 与 collectFiles 的区别：后者用于全量扫描，必须容忍个别坏文件；
// 前者用于移动操作，任何读取失败都可能导致结构丢失，因此必须报错。
func collectFilesStrict(root string, keep func(path string) bool) ([]string, error) {
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if keep(path) {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

// ---------------------------------------------------------------------------
// 隔离
// ---------------------------------------------------------------------------

// OperationLog 记录一次操作的日志条目。
type OperationLog struct {
	Level   string `json:"level"` // info / success / warn / error
	Message string `json:"message"`
}

// IsolateResult 隔离操作结果。
type IsolateResult struct {
	Success     bool           `json:"success"`
	MovedCards  int            `json:"movedCards"`
	FailedCount int            `json:"failedCount"`
	Logs        []OperationLog `json:"logs"`
}

// Isolate 将选中的资源文件移动到隔离区。
//
// cardPaths 为资源文件的绝对路径集合。
func (m *Manager) Isolate(cardPaths []string) IsolateResult {
	m.mu.Lock()
	defer m.mu.Unlock()

	res := IsolateResult{Logs: make([]OperationLog, 0, 16)}

	// 开启事务：所有文件移动都可撤销（红线 N5）
	var txn *tx.Tx
	if len(cardPaths) > 0 {
		if t, err := m.beginTx("isolate", fmt.Sprintf("隔离 %d 个条目", len(cardPaths))); err == nil {
			txn = t
		} else {
			res.Logs = append(res.Logs, OperationLog{"warn", "无法创建事务：" + err.Error()})
		}
	}
	// 事务开启失败时不阻断操作，但也不提供撤销能力。
	defer func() {
		if txn == nil {
			return
		}
		if res.FailedCount == 0 {
			if err := m.commitTx(txn); err != nil {
				res.Logs = append(res.Logs, OperationLog{"warn", "提交事务失败：" + err.Error()})
			}
		} else {
			if err := m.rollbackTx(txn); err != nil {
				res.Logs = append(res.Logs, OperationLog{"error", "回滚失败：" + err.Error()})
			}
		}
	}()

	if len(cardPaths) == 0 {
		res.Logs = append(res.Logs, OperationLog{"warn", "没有选中任何文件。"})
		return res
	}
	if !m.Scanned {
		res.Logs = append(res.Logs, OperationLog{"error", "尚未完成全局扫描，无法安全隔离。请先执行扫描。"})
		return res
	}

	// --- 1. 校验每个待隔离条目，确定其所属类型 ---
	// 条目可以是文件，也可以是文件夹。
	// 文件夹会被展开为其下的全部文件（保持原有相对层级），
	// 这样目录结构天然保留，依赖图的处理逻辑也可完全复用。
	type movePlan struct {
		src    string
		kind   ResourceKind
		relDir string
		dst    string
	}
	plans := make([]movePlan, 0, len(cardPaths))
	selectedFolders, selectedFiles := 0, 0

	for _, src := range cardPaths {
		kind, relDir, ok := m.classifyResource(src)
		if !ok {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("跳过：%s 不属于任何受管资源目录。", src)})
			continue
		}

		info, statErr := os.Stat(src)
		if statErr != nil {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("无法访问 %s: %v", src, statErr)})
			continue
		}

		if !info.IsDir() {
			dst := filepath.Join(m.DelDir(kind), relDir, filepath.Base(src))
			plans = append(plans, movePlan{src: src, kind: kind, relDir: relDir, dst: dst})
			selectedFiles++
			continue
		}

		// 文件夹：展开为文件列表
		selectedFolders++
		files, err := collectFilesStrict(src, func(path string) bool {
			return core.IsCardFile(path)
		})
		if err != nil {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("读取文件夹失败 %s: %v", src, err)})
			continue
		}
		if len(files) == 0 {
			// 空文件夹：仍然记录到隔离区（保留结构）
			dstDir := filepath.Join(m.DelDir(kind), relDir, filepath.Base(src))
			if err := os.MkdirAll(dstDir, 0755); err != nil {
				res.FailedCount++
				res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("创建目标目录失败 %s: %v", dstDir, err)})
				continue
			}
			res.Logs = append(res.Logs, OperationLog{"success", fmt.Sprintf("[移动空文件夹] %s -> %s", src, dstDir)})
			continue
		}
		for _, f := range files {
			_, fRelDir, _ := m.classifyResource(f)
			dst := filepath.Join(m.DelDir(kind), fRelDir, filepath.Base(f))
			plans = append(plans, movePlan{src: f, kind: kind, relDir: fRelDir, dst: dst})
		}
	}

	if len(plans) == 0 {
		res.Logs = append(res.Logs, OperationLog{"error", "没有可隔离的有效文件。"})
		return res
	}

	// --- 3. 移动资源文件（保留目录结构） ---
	for _, p := range plans {
		// 目标父目录若曾被隐藏删除，先重建出来
		if err := m.ensureTargetParent(p.dst); err != nil {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("创建目标目录失败 %s: %v", filepath.Dir(p.dst), err)})
			continue
		}
		if _, err := os.Stat(p.dst); err == nil {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("目标已存在，跳过: %s", p.dst)})
			continue
		}
		if err := m.txMove(txn, p.src, p.dst); err != nil {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("移动失败 %s -> %s: %v", p.src, p.dst, err)})
			continue
		}
		res.MovedCards++
		res.Logs = append(res.Logs, OperationLog{"success", fmt.Sprintf("[移动资源] %s -> %s", p.src, p.dst)})
	}

	res.Success = res.FailedCount == 0
	if res.Success {
		res.Logs = append(res.Logs, OperationLog{"success", fmt.Sprintf("隔离完成：资源 %d 个（%d 个文件夹 / %d 个文件）。", res.MovedCards, selectedFolders, selectedFiles)})
	} else {
		res.Logs = append(res.Logs, OperationLog{"warn", fmt.Sprintf("隔离结束，存在 %d 个失败项，请查看上方日志。", res.FailedCount)})
	}
	return res
}

// classifyResource 判断一个文件属于哪种资源类型，并返回其在源目录下的相对路径。
func (m *Manager) classifyResource(path string) (ResourceKind, string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ResourceKind{}, "", false
	}
	for _, kind := range ResourceKinds {
		srcDir := m.SourceDir(kind)
		rel, err := filepath.Rel(srcDir, abs)
		if err != nil {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		// rel 形如 sub\a\b\file.png，去掉文件名后即为相对目录
		relDir := filepath.Dir(rel)
		if relDir == "." {
			relDir = ""
		}
		return kind, relDir, true
	}
	return ResourceKind{}, "", false
}

// ---------------------------------------------------------------------------
// 恢复
// ---------------------------------------------------------------------------

// RestoreResult 恢复操作结果。
type RestoreResult struct {
	Success      bool           `json:"success"`
	RestoredCard int            `json:"restoredCards"`
	FailedCount  int            `json:"failedCount"`
	Logs         []OperationLog `json:"logs"`
}

// Restore 将隔离区中的文件移回游戏目录。
//
// cardPaths 为隔离区中文件的绝对路径集合。
func (m *Manager) Restore(cardPaths []string) RestoreResult {
	m.mu.Lock()
	defer m.mu.Unlock()

	res := RestoreResult{Logs: make([]OperationLog, 0, 16)}

	// 开启事务：所有文件移动都可撤销（红线 N5）
	var txn *tx.Tx
	if len(cardPaths) > 0 {
		if t, err := m.beginTx("restore", fmt.Sprintf("恢复 %d 个条目", len(cardPaths))); err == nil {
			txn = t
		} else {
			res.Logs = append(res.Logs, OperationLog{"warn", "无法创建事务：" + err.Error()})
		}
	}
	defer func() {
		if txn == nil {
			return
		}
		if res.FailedCount == 0 {
			if err := m.commitTx(txn); err != nil {
				res.Logs = append(res.Logs, OperationLog{"warn", "提交事务失败：" + err.Error()})
			}
		} else {
			if err := m.rollbackTx(txn); err != nil {
				res.Logs = append(res.Logs, OperationLog{"error", "回滚失败：" + err.Error()})
			}
		}
	}()

	if len(cardPaths) == 0 {
		res.Logs = append(res.Logs, OperationLog{"warn", "没有选中任何文件。"})
		return res
	}

	for _, src := range cardPaths {
		abs, err := filepath.Abs(src)
		if err != nil {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("路径无效: %s", src)})
			continue
		}

		// 历史遗留：DEL-zipmod 目录已不再管理，绝不改动其中的文件
		if isUnder(abs, filepath.Join(m.DelCardsRoot, "DEL-zipmod")) {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error",
				fmt.Sprintf("[跳过] %s 属于 DEL-zipmod，本工具已不再管理 zipmod，未做任何改动。", abs)})
			continue
		}

		kind, relDir, ok := m.classifyDelResource(abs)
		if !ok {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("跳过：%s 不属于隔离区中的受管资源。", abs)})
			continue
		}

		// 文件夹：展开为文件列表后再逐个恢复，保持原有目录结构
		if info, statErr := os.Stat(abs); statErr == nil && info.IsDir() {
			files, err := collectFilesStrict(abs, func(path string) bool {
				return core.IsCardFile(path)
			})
			if err != nil {
				res.FailedCount++
				res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("读取文件夹失败 %s: %v", abs, err)})
				continue
			}
			if len(files) == 0 {
				// 空文件夹：只重建目录
				dstDir := filepath.Join(m.SourceDir(kind), relDir, filepath.Base(abs))
				if err := m.ensureTargetParent(dstDir); err != nil {
					res.FailedCount++
					res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("重建目录失败 %s: %v", dstDir, err)})
					continue
				}
				res.RestoredCard++
				res.Logs = append(res.Logs, OperationLog{"success", fmt.Sprintf("[恢复空文件夹] %s", dstDir)})
				continue
			}
			for _, f := range files {
				m.restoreSingleFile(f, kind, &res, txn)
			}
			continue
		}

		m.restoreSingleFile(abs, kind, &res, txn)
	}

	// 清理 DEL 区的空目录（仅隔离区，游戏目录永不删除）
	if cleaned := m.cleanEmptyDirs(m.DelCardsRoot); cleaned > 0 {
		res.Logs = append(res.Logs, OperationLog{"info", fmt.Sprintf("已清理隔离区空目录 %d 个。", cleaned)})
	}

	res.Success = res.FailedCount == 0
	if res.Success {
		res.Logs = append(res.Logs, OperationLog{"success", fmt.Sprintf("恢复完成：资源 %d 个。", res.RestoredCard)})
	} else {
		res.Logs = append(res.Logs, OperationLog{"warn", fmt.Sprintf("恢复结束，存在 %d 个失败项，请查看上方日志。", res.FailedCount)})
	}
	return res
}

// restoreSingleFile 把隔离区中的单个资源文件移回游戏原目录。
//
// 目标父目录若曾被隐藏删除，会先依据隐藏索引重建出来。
func (m *Manager) restoreSingleFile(abs string, kind ResourceKind, res *RestoreResult, txn *tx.Tx) {
	_, relDir, ok := m.classifyDelResource(abs)
	if !ok {
		res.FailedCount++
		res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("跳过：%s 不属于隔离区中的受管资源。", abs)})
		return
	}

	dst := filepath.Join(m.SourceDir(kind), relDir, filepath.Base(abs))
	if err := m.ensureTargetParent(dst); err != nil {
		res.FailedCount++
		res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("重建原目录失败 %s: %v", filepath.Dir(dst), err)})
		return
	}
	if _, err := os.Stat(dst); err == nil {
		res.FailedCount++
		res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("目标已存在，跳过恢复: %s", dst)})
		return
	}
	if err := m.txMove(txn, abs, dst); err != nil {
		res.FailedCount++
		res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("恢复失败 %s -> %s: %v", abs, dst, err)})
		return
	}
	res.RestoredCard++
	res.Logs = append(res.Logs, OperationLog{"success", fmt.Sprintf("[恢复资源] %s -> %s", abs, dst)})
}

// ensureTargetParent 确保目标文件的父目录存在。
//
// 若父目录此前被「隐藏」删除，则依据隐藏索引重建对应的目录树。
func (m *Manager) ensureTargetParent(targetFile string) error {
	parent := filepath.Dir(targetFile)
	if info, err := os.Stat(parent); err == nil && info.IsDir() {
		return nil
	}
	if m.invisible != nil {
		rebuilt, err := m.invisible.EnsurePathForOperation(targetFile)
		if err != nil {
			return err
		}
		if rebuilt {
			return nil
		}
	}
	return os.MkdirAll(parent, 0755)
}

// classifyDelResource 判断隔离区中某个文件属于哪种资源类型及其相对目录。
func (m *Manager) classifyDelResource(path string) (ResourceKind, string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ResourceKind{}, "", false
	}
	for _, kind := range ResourceKinds {
		rel, err := filepath.Rel(m.DelDir(kind), abs)
		if err != nil {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		relDir := filepath.Dir(rel)
		if relDir == "." {
			relDir = ""
		}
		return kind, relDir, true
	}
	return ResourceKind{}, "", false
}

// cleanEmptyDirs 自底向上删除 root 下的空目录，返回清理数量。
// 只作用于隔离区，绝不触碰游戏目录。
//
// 注意：标准隔离子目录（DEL-female_card 等）本身必须始终保留，
// 因此即使它们变空也不会被删除，只清理其内部的空目录。
func (m *Manager) cleanEmptyDirs(root string) int {
	// 保护标准子目录，避免被误删（含历史遗留的 DEL-zipmod）
	protected := make(map[string]bool, len(ResourceKinds)+2)
	for _, k := range ResourceKinds {
		protected[filepath.Join(root, k.DelName)] = true
	}
	protected[filepath.Join(root, "DEL-zipmod")] = true
	// 隐藏状态目录内保存着 JSON 状态文件，绝不能被清理
	protected[filepath.Join(root, invisibleDirName)] = true

	cleaned := 0
	var dirs []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil
		}
		if path == root || protected[path] {
			return nil
		}
		dirs = append(dirs, path)
		return nil
	})
	// 深度优先：先删子目录
	sort.Slice(dirs, func(i, j int) bool {
		return len(dirs[i]) > len(dirs[j])
	})
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		if len(entries) == 0 {
			if err := os.Remove(dir); err == nil {
				cleaned++
			}
		}
	}
	return cleaned
}

// ---------------------------------------------------------------------------
// 文件移动
// ---------------------------------------------------------------------------

// moveFile 移动文件，优先使用 rename（同盘高效），
// 跨设备时退化为"复制 -> 校验 -> 删除源文件"，且绝不覆盖已存在的目标。
//
// §3.7 修复要点：
//   - 跨盘复制前检查目标盘剩余空间是否足够（源大小 × 1.1）；
//   - 删除源文件失败时，**必须删除已创建的目标副本**，
//     否则用户会看到同一份文件在两处同时存在，且没有任何记录说明发生了什么；
//   - 复制后校验大小不一致时，同样清理半成品目标。
func moveFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("目标文件已存在: %s", dst)
	}

	// 同盘 rename
	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	// 跨设备：复制后删除
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	// 空间预检：目标盘剩余空间需 >= 源大小 × 1.1
	if err := checkFreeSpace(filepath.Dir(dst), srcInfo.Size()); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, srcInfo.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}

	// 校验大小一致
	dstInfo, statErr := os.Stat(dst)
	if statErr != nil || dstInfo.Size() != srcInfo.Size() {
		// 半成品：清理掉，绝不留下不完整的副本
		os.Remove(dst)
		return fmt.Errorf("复制后文件大小不一致，已保留源文件并清理目标副本")
	}

	// 删除源文件
	if err := os.Remove(src); err != nil {
		// 删源失败 → 目标已存在副本会造成"双份文件"。
		// 此时回滚：删除目标副本，让磁盘回到操作前的状态。
		if rmErr := os.Remove(dst); rmErr != nil {
			return fmt.Errorf("移动失败：无法删除源文件(%v)，且无法回滚目标副本(%v)，请手动检查 %s",
				err, rmErr, dst)
		}
		return fmt.Errorf("移动失败：无法删除源文件(%v)，已回滚目标副本，文件保持原位: %s", err, src)
	}
	return nil
}

func copyAll(dst *os.File, src *os.File) (int64, error) {
	return io.Copy(dst, src)
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// isUnder 判断 path 是否位于 root 之内（含 root 本身）。
func isUnder(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// firstN 返回列表的前 n 项并拼接为可读字符串。
func firstN(list []string, n int) string {
	if len(list) == 0 {
		return ""
	}
	if len(list) < n {
		n = len(list)
	}
	return strings.Join(list[:n], ", ")
}

// nowMS 返回当前 Unix 毫秒时间戳。
func nowMS() int64 {
	return time.Now().UnixNano() / int64(timeMillisecond)
}

// timeMillisecond 毫秒常量（避免直接引用 time 包造成命名冲突）。
const timeMillisecond = 1000000
