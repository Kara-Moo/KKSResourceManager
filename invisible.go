package main

// invisible.go —— 空文件夹隐藏与路径重建
//
// 需求背景：
//  - 用户希望「隐藏」KKS 主目录或 DEL-XXX 下的空文件夹，让列表更整洁；
//  - 隐藏不是删除，原始目录树必须可还原；
//  - 隐藏后的文件夹若恰好是某个待隔离/启用文件的父目录，
//    执行文件操作前必须先依据隐藏索引把父路径重建出来。
//
// 状态文件全部存放在 DELCARDS/DEL-invisible/ 下：
//
//	KKSinvisible.json  被隐藏的 KKS 主目录下的文件夹（相对 KKS_ROOT 的路径）
//	DELinvisible.json  被隐藏的 DEL-XXX 下的文件夹（相对 DELCARDS_ROOT 的路径）
//	OriginKKS.json     隐藏 KKS 侧文件夹时记录的原始目录树
//	OriginDEL.json     隐藏 DEL 侧文件夹时记录的原始目录树
//
// 目录树只记录文件夹，不记录文件：文件的位置由 DEL-XXX 的目录结构自然体现。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// invisibleDirName 隐藏状态目录名（位于 DELCARDS_ROOT 下）
const invisibleDirName = "DEL-invisible"

// 隐藏状态文件名
const (
	fileKKSinvisible = "KKSinvisible.json"
	fileDELinvisible = "DELinvisible.json"
	fileOriginKKS    = "OriginKKS.json"
	fileOriginDEL    = "OriginDEL.json"
)

// FolderTree 记录一个被隐藏文件夹的原始目录结构。
//
// Key   被隐藏文件夹的路径（KKS 侧相对 KKS_ROOT，DEL 侧相对 DELCARDS_ROOT）
// Value 该文件夹下的子目录列表（相对 Key 的路径，POSIX 分隔符）
type FolderTree map[string][]string

// InvisibleState 保存全部隐藏状态。
//
// KKS 侧与 DEL 侧分别写入各自的 JSON 文件，因此这里的字段名可以各自独立。
type InvisibleState struct {
	// KKSFolders 被隐藏的 KKS 侧文件夹（相对 KKS_ROOT）
	KKSFolders []string `json:"kksFolders"`
	// DELFolders 被隐藏的 DEL 侧文件夹（相对 DELCARDS_ROOT）
	DELFolders []string `json:"delFolders"`
}

// InvisibleStore 管理隐藏状态的读写。
//
// 使用独立的互斥锁：隐藏状态与 Manager 的其它状态耦合度低，
// 独立加锁可避免与扫描/隔离操作互相阻塞。
type InvisibleStore struct {
	mu    sync.RWMutex
	dir   string // DELCARDS_ROOT/DEL-invisible
	origK string // KKS_ROOT
	origD string // DELCARDS_ROOT
}

// NewInvisibleStore 创建隐藏状态存储并确保目录与四个状态文件存在。
func NewInvisibleStore(delCardsRoot, kksRoot string) (*InvisibleStore, error) {
	dir := filepath.Join(delCardsRoot, invisibleDirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建隐藏状态目录失败: %w", err)
	}
	s := &InvisibleStore{
		dir:   dir,
		origK: kksRoot,
		origD: delCardsRoot,
	}
	// 四个状态文件在程序启动时就创建好，避免用户查看时目录为空
	if err := s.ensureStateFiles(); err != nil {
		return nil, err
	}
	return s, nil
}

// ensureStateFiles 确保四个状态文件存在（已存在则保持原内容）。
func (s *InvisibleStore) ensureStateFiles() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 隐藏列表
	for _, name := range []string{fileKKSinvisible, fileDELinvisible} {
		path := filepath.Join(s.dir, name)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := s.writeJSON(name, InvisibleState{}); err != nil {
			return err
		}
	}
	// 原始目录树
	for _, name := range []string{fileOriginKKS, fileOriginDEL} {
		path := filepath.Join(s.dir, name)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := s.writeJSON(name, FolderTree{}); err != nil {
			return err
		}
	}
	return nil
}

// Dir 返回隐藏状态目录路径。
func (s *InvisibleStore) Dir() string { return s.dir }

// ---------------------------------------------------------------------------
// 状态读写
// ---------------------------------------------------------------------------

// readJSON 读取 JSON 文件；文件不存在时返回零值与 nil。
func (s *InvisibleStore) readJSON(name string, target interface{}) error {
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取 %s 失败: %w", name, err)
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("解析 %s 失败: %w", name, err)
	}
	return nil
}

// writeJSON 原子写入 JSON 文件（先写临时文件再替换，避免中途崩溃导致状态损坏）。
func (s *InvisibleStore) writeJSON(name string, value interface{}) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 %s 失败: %w", name, err)
	}
	target := filepath.Join(s.dir, name)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", name, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("保存 %s 失败: %w", name, err)
	}
	return nil
}

// LoadKKSFolders 读取被隐藏的 KKS 侧文件夹列表。
func (s *InvisibleStore) LoadKKSFolders() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var st InvisibleState
	if err := s.readJSON(fileKKSinvisible, &st); err != nil {
		return nil, err
	}
	return st.KKSFolders, nil
}

// LoadDELFolders 读取被隐藏的 DEL 侧文件夹列表。
func (s *InvisibleStore) LoadDELFolders() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var st InvisibleState
	if err := s.readJSON(fileDELinvisible, &st); err != nil {
		return nil, err
	}
	return st.DELFolders, nil
}

// ---------------------------------------------------------------------------
// 隐藏
// ---------------------------------------------------------------------------

// HideResult 隐藏操作结果。
type HideResult struct {
	Success    bool           `json:"success"`
	Hidden     int            `json:"hidden"`
	Skipped    int            `json:"skipped"`
	FailedCount int           `json:"failedCount"`
	Logs       []OperationLog `json:"logs"`
}

// HideFolders 隐藏指定的空文件夹。
//
// side 取值 "kks" 或 "del"，决定记录到哪一份状态文件。
// 只允许隐藏空文件夹（含空子目录）；若文件夹内存在文件则跳过并记录原因。
func (s *InvisibleStore) HideFolders(paths []string, side string) HideResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	res := HideResult{Logs: make([]OperationLog, 0, 8)}
	if len(paths) == 0 {
		res.Logs = append(res.Logs, OperationLog{"warn", "没有选中任何文件夹。"})
		return res
	}

	// 读取现有状态
	var kksState, delState InvisibleState
	if err := s.readJSON(fileKKSinvisible, &kksState); err != nil {
		res.Logs = append(res.Logs, OperationLog{"error", err.Error()})
		return res
	}
	if err := s.readJSON(fileDELinvisible, &delState); err != nil {
		res.Logs = append(res.Logs, OperationLog{"error", err.Error()})
		return res
	}
	var originKKS, originDEL FolderTree
	if err := s.readJSON(fileOriginKKS, &originKKS); err != nil {
		res.Logs = append(res.Logs, OperationLog{"error", err.Error()})
		return res
	}
	if err := s.readJSON(fileOriginDEL, &originDEL); err != nil {
		res.Logs = append(res.Logs, OperationLog{"error", err.Error()})
		return res
	}
	if originKKS == nil {
		originKKS = FolderTree{}
	}
	if originDEL == nil {
		originDEL = FolderTree{}
	}

	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("路径无效: %s", p)})
			continue
		}

		// 判断归属侧并计算用于记录的相对路径（归一化，容忍相对路径入参）
		rel, sideName, ok := s.classify(abs)
		if !ok {
			// 可能是相对路径，尝试归一化后再判断
			if r, sd, k := s.normalizePath(p); k {
				rel, sideName, ok = r, sd, true
				abs = filepath.Join(rootOf(sideName, s), filepath.FromSlash(rel))
			}
		}
		if !ok {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("跳过：%s 不在可管理范围内。", abs)})
			continue
		}
		if sideName != side {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("跳过：%s 不属于当前操作侧。", abs)})
			continue
		}

		// 必须是文件夹
		info, err := os.Stat(abs)
		if err != nil {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("无法访问 %s: %v", abs, err)})
			continue
		}
		if !info.IsDir() {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("跳过：%s 不是文件夹。", abs)})
			continue
		}

		// 只允许隐藏空文件夹
		files, err := countFilesIn(abs)
		if err != nil {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("无法检查 %s: %v", abs, err)})
			continue
		}
		if files > 0 {
			res.Skipped++
			res.Logs = append(res.Logs, OperationLog{
				"warn",
				fmt.Sprintf("文件夹不是空的（含 %d 个文件，角色卡也会计入），只有空文件夹才能隐藏：%s", files, abs),
			})
			continue
		}

		// 记录原始目录树（只记文件夹）
		tree, err := captureFolderTree(abs)
		if err != nil {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("记录目录树失败 %s: %v", abs, err)})
			continue
		}

		// 写入状态
		if sideName == "kks" {
			originKKS[rel] = tree
			kksState.KKSFolders = appendUnique(kksState.KKSFolders, rel)
		} else {
			originDEL[rel] = tree
			delState.DELFolders = appendUnique(delState.DELFolders, rel)
		}

		// 删除源文件夹（含其空的子目录树）
		if err := os.RemoveAll(abs); err != nil {
			// 删除失败则回滚状态记录
			if sideName == "kks" {
				delete(originKKS, rel)
				kksState.KKSFolders = removeString(kksState.KKSFolders, rel)
			} else {
				delete(originDEL, rel)
				delState.DELFolders = removeString(delState.DELFolders, rel)
			}
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("删除文件夹失败 %s: %v", abs, err)})
			continue
		}

		res.Hidden++
		res.Logs = append(res.Logs, OperationLog{"success", fmt.Sprintf("[隐藏] %s", abs)})
	}

	// 持久化
	if err := s.writeJSON(fileKKSinvisible, kksState); err != nil {
		res.FailedCount++
		res.Logs = append(res.Logs, OperationLog{"error", err.Error()})
	}
	if err := s.writeJSON(fileDELinvisible, delState); err != nil {
		res.FailedCount++
		res.Logs = append(res.Logs, OperationLog{"error", err.Error()})
	}
	if err := s.writeJSON(fileOriginKKS, originKKS); err != nil {
		res.FailedCount++
		res.Logs = append(res.Logs, OperationLog{"error", err.Error()})
	}
	if err := s.writeJSON(fileOriginDEL, originDEL); err != nil {
		res.FailedCount++
		res.Logs = append(res.Logs, OperationLog{"error", err.Error()})
	}

	res.Success = res.FailedCount == 0
	return res
}

// classify 判断绝对路径属于哪一侧，并返回用于记录的相对路径。
func (s *InvisibleStore) classify(abs string) (rel string, side string, ok bool) {
	if r, err := filepath.Rel(s.origK, abs); err == nil && !strings.HasPrefix(r, "..") {
		return filepath.ToSlash(r), "kks", true
	}
	if r, err := filepath.Rel(s.origD, abs); err == nil && !strings.HasPrefix(r, "..") {
		return filepath.ToSlash(r), "del", true
	}
	return "", "", false
}

// normalizePath 把外部传入的路径归一化为「侧 + 相对路径」。
//
// 历史 bug：隐藏树对外暴露的 ID 是绝对路径，而 RestoreFolders 按相对路径查表，
// 两者约定不一致 → 永远匹配不上 → 「未找到隐藏记录」。
// 修法：入口统一归一化，无论前端传绝对路径还是相对路径都能正确匹配。
//
// 教训：同一个概念在一处用绝对路径、另一处用相对路径，是这类 bug 的唯一来源。
// 全项目约定：对外 ID 用绝对路径，索引 key 用「侧 + 相对路径」。
func (s *InvisibleStore) normalizePath(p string) (rel string, side string, ok bool) {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" {
		return "", "", false
	}

	if filepath.IsAbs(trimmed) {
		return s.classify(trimmed)
	}

	// 相对路径：两侧都尝试
	norm := filepath.ToSlash(trimmed)
	for _, root := range []string{s.origK, s.origD} {
		cand := filepath.Join(root, filepath.FromSlash(norm))
		if _, err := os.Stat(cand); err == nil {
			return s.classify(cand)
		}
	}
	// 文件已不存在（隐藏后源目录被删）时，按字符串归属判断
	if r, sd, k := s.classify(filepath.Join(s.origK, filepath.FromSlash(norm))); k {
		return r, sd, true
	}
	if r, sd, k := s.classify(filepath.Join(s.origD, filepath.FromSlash(norm))); k {
		return r, sd, true
	}
	return norm, "", false
}

// ---------------------------------------------------------------------------
// 恢复
// ---------------------------------------------------------------------------

// InvisibleRestoreResult 隐藏文件夹的恢复结果。
type InvisibleRestoreResult struct {
	Success     bool           `json:"success"`
	Restored    int            `json:"restored"`
	FailedCount int            `json:"failedCount"`
	Logs        []OperationLog `json:"logs"`
}

// RestoreFolders 恢复被隐藏的文件夹。
//
// paths 为隐藏状态中记录的相对路径（KKS 侧相对 KKS_ROOT，DEL 侧相对 DELCARDS_ROOT）。
// 前端传入的是「隐藏的文件夹」列表中的条目，因此这里同时接受相对路径。
func (s *InvisibleStore) RestoreFolders(paths []string) InvisibleRestoreResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	res := InvisibleRestoreResult{Logs: make([]OperationLog, 0, 8)}
	if len(paths) == 0 {
		res.Logs = append(res.Logs, OperationLog{"warn", "没有选中任何文件夹。"})
		return res
	}

	var kksState, delState InvisibleState
	if err := s.readJSON(fileKKSinvisible, &kksState); err != nil {
		res.Logs = append(res.Logs, OperationLog{"error", err.Error()})
		return res
	}
	if err := s.readJSON(fileDELinvisible, &delState); err != nil {
		res.Logs = append(res.Logs, OperationLog{"error", err.Error()})
		return res
	}
	var originKKS, originDEL FolderTree
	if err := s.readJSON(fileOriginKKS, &originKKS); err != nil {
		res.Logs = append(res.Logs, OperationLog{"error", err.Error()})
		return res
	}
	if err := s.readJSON(fileOriginDEL, &originDEL); err != nil {
		res.Logs = append(res.Logs, OperationLog{"error", err.Error()})
		return res
	}

	for _, p := range paths {
		// 归一化：无论传入绝对路径还是相对路径，都转成「侧 + 相对路径」
		rel, _, ok := s.normalizePath(p)
		if !ok {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("无法识别的路径: %s", p)})
			continue
		}

		// KKS 侧
		if containsString(kksState.KKSFolders, rel) {
			tree := originKKS[rel]
			abs := filepath.Join(s.origK, filepath.FromSlash(rel))
			if err := recreateFolderTree(abs, tree); err != nil {
				res.FailedCount++
				res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("恢复失败 %s: %v", abs, err)})
				continue
			}
			kksState.KKSFolders = removeString(kksState.KKSFolders, rel)
			delete(originKKS, rel)
			res.Restored++
			res.Logs = append(res.Logs, OperationLog{"success", fmt.Sprintf("[恢复] %s", abs)})
			continue
		}

		// DEL 侧
		if containsString(delState.DELFolders, rel) {
			tree := originDEL[rel]
			abs := filepath.Join(s.origD, filepath.FromSlash(rel))
			if err := recreateFolderTree(abs, tree); err != nil {
				res.FailedCount++
				res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("恢复失败 %s: %v", abs, err)})
				continue
			}
			delState.DELFolders = removeString(delState.DELFolders, rel)
			delete(originDEL, rel)
			res.Restored++
			res.Logs = append(res.Logs, OperationLog{"success", fmt.Sprintf("[恢复] %s", abs)})
			continue
		}

		res.FailedCount++
		res.Logs = append(res.Logs, OperationLog{"error", fmt.Sprintf("未找到隐藏记录: %s", rel)})
	}

	// 持久化
	for name, value := range map[string]interface{}{
		fileKKSinvisible: kksState,
		fileDELinvisible: delState,
		fileOriginKKS:    originKKS,
		fileOriginDEL:    originDEL,
	} {
		if err := s.writeJSON(name, value); err != nil {
			res.FailedCount++
			res.Logs = append(res.Logs, OperationLog{"error", err.Error()})
		}
	}

	res.Success = res.FailedCount == 0
	return res
}

// ---------------------------------------------------------------------------
// 路径重建
// ---------------------------------------------------------------------------

// EnsurePathForOperation 确保某个目标路径的父目录存在。
//
// 若父目录曾被隐藏删除，则依据隐藏索引把它重建出来。
// 返回是否需要重建（true 表示发生了重建）。
func (s *InvisibleStore) EnsurePathForOperation(absTarget string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	parent := filepath.Dir(absTarget)
	if info, err := os.Stat(parent); err == nil && info.IsDir() {
		return false, nil
	}

	// 读取隐藏记录，找出可以重建的祖先
	var kksState, delState InvisibleState
	if err := s.readJSON(fileKKSinvisible, &kksState); err != nil {
		return false, err
	}
	if err := s.readJSON(fileDELinvisible, &delState); err != nil {
		return false, err
	}
	var originKKS, originDEL FolderTree
	if err := s.readJSON(fileOriginKKS, &originKKS); err != nil {
		return false, err
	}
	if err := s.readJSON(fileOriginDEL, &originDEL); err != nil {
		return false, err
	}

	// 找出 parent 的最深层隐藏祖先
	bestRel := ""
	bestSide := ""
	for _, rel := range kksState.KKSFolders {
		if isAncestorPath(rel, parent, s.origK) && len(rel) > len(bestRel) {
			bestRel, bestSide = rel, "kks"
		}
	}
	for _, rel := range delState.DELFolders {
		if isAncestorPath(rel, parent, s.origD) && len(rel) > len(bestRel) {
			bestRel, bestSide = rel, "del"
		}
	}
	if bestRel == "" {
		// 没有隐藏记录，直接创建父目录即可
		if err := os.MkdirAll(parent, 0755); err != nil {
			return false, fmt.Errorf("创建目录失败 %s: %w", parent, err)
		}
		return false, nil
	}

	// 重建隐藏目录树
	if bestSide == "kks" {
		abs := filepath.Join(s.origK, filepath.FromSlash(bestRel))
		if err := recreateFolderTree(abs, originKKS[bestRel]); err != nil {
			return false, err
		}
	} else {
		abs := filepath.Join(s.origD, filepath.FromSlash(bestRel))
		if err := recreateFolderTree(abs, originDEL[bestRel]); err != nil {
			return false, err
		}
	}
	// 重建后仍需确保目标父目录存在（隐藏树可能不含更深层级）
	if err := os.MkdirAll(parent, 0755); err != nil {
		return false, fmt.Errorf("创建目录失败 %s: %w", parent, err)
	}
	return true, nil
}

// isAncestorPath 判断隐藏记录 rel（相对 root）是否是 target（绝对路径）的祖先目录。
func isAncestorPath(rel, target, root string) bool {
	ancestor := filepath.Join(root, filepath.FromSlash(rel))
	relPath, err := filepath.Rel(ancestor, target)
	if err != nil {
		return false
	}
	if relPath == "." {
		return true
	}
	return relPath != ".." && !strings.HasPrefix(relPath, ".."+string(filepath.Separator)) && !filepath.IsAbs(relPath)
}

// ---------------------------------------------------------------------------
// 目录树记录与重建
// ---------------------------------------------------------------------------

// captureFolderTree 记录文件夹下的全部子目录（相对路径，POSIX 分隔符）。
// 不记录文件。
func captureFolderTree(root string) ([]string, error) {
	var dirs []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		dirs = append(dirs, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(dirs)
	return dirs, nil
}

// recreateFolderTree 根据记录的目录树重建文件夹结构。
func recreateFolderTree(root string, tree []string) error {
	if err := os.MkdirAll(root, 0755); err != nil {
		return err
	}
	for _, rel := range tree {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(p, 0755); err != nil {
			return err
		}
	}
	return nil
}

// rootOf 返回某一侧对应的根目录。
func rootOf(side string, s *InvisibleStore) string {
	if side == "kks" {
		return s.origK
	}
	return s.origD
}

// countFilesIn 统计文件夹内的文件数量（递归）。
func countFilesIn(root string) (int, error) {
	count := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		count++
		return nil
	})
	return count, err
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// appendUnique 向列表追加元素（已存在则不重复添加）。
func appendUnique(list []string, value string) []string {
	if containsString(list, value) {
		return list
	}
	return append(list, value)
}

// containsString 判断列表中是否包含指定元素。
func containsString(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

// removeString 从列表中移除指定元素。
func removeString(list []string, value string) []string {
	result := make([]string, 0, len(list))
	for _, v := range list {
		if v != value {
			result = append(result, v)
		}
	}
	return result
}
