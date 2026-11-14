package main

// app.go —— Wails 绑定层
//
// 对外暴露的方法全部返回 JSON 字符串，便于前端直接解析并渲染。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"kksresourcemanager/internal/core"
	"kksresourcemanager/internal/preview"
	"kksresourcemanager/internal/tx"
)

// App 是 Wails 应用主体。
type App struct {
	ctx context.Context

	mu      sync.RWMutex
	manager *Manager

	scanMu    sync.Mutex
	scanning  bool
	scanMsg   string
	scanTotal int
	scanDone  int
	scanPhase string

	// logs 后端日志环形缓冲（前端订阅增量，不自己拼字符串）
	logs *logRing

	// thumbOnce 保证「首张缩略图」只记一次日志
	thumbOnce sync.Once
	// startedAt 进程启动时刻，用于计算首张缩略图耗时
	startedAt time.Time
}

// NewApp 创建 App 实例。
func NewApp() *App {
	return &App{
		logs:      newLogRing(5000),
		startedAt: time.Now(),
	}
}

// emitLog 写一条日志到环形缓冲，并广播给前端。
func (a *App) emitLog(level, scope, msg string) {
	if a.logs == nil {
		return
	}
	entry := a.logs.Append(level, scope, msg)
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "log:append", entry)
	}
}

// GetLogs 返回指定序号之后的日志条目。
//
// sinceSeq 为 0 时返回全部。level 为空时返回所有级别。
func (a *App) GetLogs(sinceSeq int64, level string) string {
	if a.logs == nil {
		return ok(map[string]interface{}{"seq": int64(0), "entries": []LogEntry{}}, "")
	}
	entries := a.logs.Since(sinceSeq)
	if level != "" && level != "all" {
		filtered := make([]LogEntry, 0, len(entries))
		for _, e := range entries {
			if e.Level == level {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	total, errors := a.logs.Stats()
	return ok(map[string]interface{}{
		"seq":     a.logs.CurrentSeq(),
		"entries": entries,
		"total":   total,
		"errors":  errors,
	}, "")
}

// startup 在 Wails 启动时调用。
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	// 启动缩略图队列（红线 N1：解码只在后台 worker 发生）
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()
	if manager != nil {
		// 幂等兜底：真正启动点见 SelectKKSRoot（FIX-01）。
		if err := manager.StartThumbQueue(a.notifyThumbsReady, a.logThumbError); err != nil {
			a.emitLog("warn", "preview", "缩略图队列启动失败："+err.Error())
		}
	}

	// 崩溃恢复（§11.6）：自动回滚上次未完成的操作。
	//
	// 必须在 manager 存在之前也能执行 —— 用户崩溃后重启、
	// 尚未重新选择 KKS 根目录时，未完成的事务同样需要还原。
	a.recoverPendingTx()

	a.ctx = ctx
}

// attachManager 切换当前 Manager，并负责启动缩略图队列。
//
// **唯一入口**：所有对 a.manager 的赋值都必须经过这里。
//
// v2.2 FIX-01 的成因就是「赋值在一处、启动在另一处」：
// startup() 里启动队列时 a.manager 还是 nil（用户尚未选目录），
// 而 SelectKKSRoot 创建了 Manager 却没启动队列 —— 两段代码各自看都合理，
// 合起来就是「队列从未启动」。收敛到一个入口后，这类脱节不可能再发生。
func (a *App) attachManager(m *Manager) {
	a.mu.Lock()
	old := a.manager
	a.manager = m
	a.mu.Unlock()

	// 先停旧队列，避免旧 worker 继续为旧根生成缩略图
	if old != nil && old != m {
		old.StopThumbQueue()
	}
	if m == nil {
		return
	}

	if err := m.StartThumbQueue(a.notifyThumbsReady, a.logThumbError); err != nil {
		a.emitLog("warn", "preview", "缩略图队列启动失败："+err.Error())
		return
	}

	a.emitLog("info", "preview",
		fmt.Sprintf("[preview] 队列已启动，worker=3，缓存目录=%s",
			filepath.Join(m.DelCardsRoot, "_state", ThumbDirName)))

	// 队列启动后让前端重取面板：
	// 队列启动前渲染的格子已被记成 pending，且任务从未入队，
	// 不重新请求它们会永远停在灰块。
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "panel:invalidate",
			map[string]interface{}{"reason": "thumb-queue-started"})
	}
}

// ---------------------------------------------------------------------------
// 通用响应封装
// ---------------------------------------------------------------------------

// APIResponse 是所有绑定方法的统一返回结构。
type APIResponse struct {
	Success bool        `json:"success"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

func ok(data interface{}, message string) string {
	return marshalJSON(APIResponse{Success: true, Message: message, Data: data})
}

func fail(message string) string {
	return marshalJSON(APIResponse{Success: false, Message: message})
}

func marshalJSON(v interface{}) string {
	data, err := json.Marshal(v)
	if err != nil {
		return fail(fmt.Sprintf("序列化失败: %v", err))
	}
	return string(data)
}

// ---------------------------------------------------------------------------
// 1. 选择 KKS 根目录
// ---------------------------------------------------------------------------

// SelectKKSRoot 弹出文件夹选择框，初始化 Manager 并触发全量扫描。
// 返回 JSON：{success, message, data:{kksRoot, delCardsRoot}}。
func (a *App) SelectKKSRoot() string {
	if a.ctx == nil {
		return fail("应用尚未初始化。")
	}

	dir, err := wruntime.OpenDirectoryDialog(a.ctx, wruntime.OpenDialogOptions{
		Title: "选择 Koikatsu Sunshine 游戏根目录（例如 E:\\KKV114）",
	})
	if err != nil {
		return fail(fmt.Sprintf("打开目录选择框失败: %v", err))
	}
	if strings.TrimSpace(dir) == "" {
		return fail("用户取消了选择。")
	}

	manager, err := NewManager(dir)
	if err != nil {
		return fail(err.Error())
	}

	// 校验目录结构：缺少必需的扫描目录时直接报错，不进入扫描流程
	missing, err := manager.ValidateRoot()
	if err != nil {
		return fail(err.Error())
	}
	if len(missing) > 0 {
		return ok(map[string]interface{}{
			"kksRoot": manager.KksRoot,
			"missing": missing,
			"valid":   false,
		}, fmt.Sprintf("加载错误：缺少必需的目录 %s", strings.Join(missing, "、")))
	}

	// 切换根目录：所有给 a.manager 赋值的地方都走 attachManager，
	// 它在同一个入口里负责启动缩略图队列（v2.2 FIX-01）。
	a.attachManager(manager)

	// 异步执行全量扫描，避免阻塞 UI
	a.startScan(manager)

	return ok(map[string]interface{}{
		"kksRoot":      manager.KksRoot,
		"delCardsRoot": manager.DelCardsRoot,
		"valid":        true,
	}, fmt.Sprintf("已选择 KKS 根目录: %s", manager.KksRoot))
}

// startScan 在后台执行全量扫描，并通过事件把进度推送给前端。
func (a *App) startScan(manager *Manager) {
	a.scanMu.Lock()
	if a.scanning {
		a.scanMu.Unlock()
		return
	}
	a.scanning = true
	a.scanMsg = "正在加载"
	a.scanTotal = 0
	a.scanDone = 0
	a.scanPhase = "resource"
	a.scanMu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				a.setScanMsg(fmt.Sprintf("加载错误：%v", r), 0, 0, false)
			}
			a.scanMu.Lock()
			a.scanning = false
			a.scanMu.Unlock()
		}()

		result := manager.ScanAll(func(phase string, done, total int) {
			a.scanMu.Lock()
			a.scanDone = done
			a.scanTotal = total
			a.scanPhase = phase
			a.scanMsg = "正在加载"
			a.scanMu.Unlock()
			a.emitScanProgress(phase, done, total)
		})

		msg := fmt.Sprintf("扫描完成：资源文件 %d 个，耗时 %d ms",
			result.ResourceCount, result.ScanDurationMS)
		if result.FailedFiles > 0 {
			msg += fmt.Sprintf("，%d 个文件解析失败", result.FailedFiles)
		}
		a.setScanMsg(msg, result.ScanDurationMS, result.ResourceCount, true)
		a.emitScanDone(result)

		// §13.1 要求的增量扫描证据日志：
		// 「复用 N，解析 M」是「没有重复扫盘」的直接证据。
		a.emitLog("info", "index", fmt.Sprintf("增量扫描：复用 %d，解析 %d，移除 %d，耗时 %dms。",
			result.Reused, result.Parsed, result.Removed, result.ScanDurationMS))
		if result.ScanWarning != "" {
			a.emitLog("warn", "index", "索引保存失败（不影响本次使用）："+result.ScanWarning)
		}
		if result.IndexUpgradeNote != "" {
			// v1 → v2 迁移是静默全量重建，日志里说一声，避免用户以为「变慢了」。
			a.emitLog("info", "index", result.IndexUpgradeNote)
		}
	}()
}

// setScanMsg 更新扫描状态并广播。
func (a *App) setScanMsg(msg string, durationMS int64, resourceCount int, success bool) {
	a.scanMu.Lock()
	a.scanMsg = msg
	a.scanMu.Unlock()
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "scan:done", map[string]interface{}{
			"message":       msg,
			"durationMs":    durationMS,
			"resourceCount": resourceCount,
			"success":       success,
		})
	}
}

func (a *App) emitScanProgress(phase string, done, total int) {
	if a.ctx == nil {
		return
	}
	wruntime.EventsEmit(a.ctx, "scan:progress", map[string]interface{}{
		"phase": phase,
		"done":  done,
		"total": total,
	})
}

func (a *App) emitScanDone(result ScanResult) {
	if a.ctx == nil {
		return
	}
	wruntime.EventsEmit(a.ctx, "scan:complete", map[string]interface{}{
		"resourceCount": result.ResourceCount,
		"failedFiles":   result.FailedFiles,
		"durationMs":    result.ScanDurationMS,
	})
}

// GetScanStatus 返回当前扫描状态，供前端轮询或事件丢失时兜底。
func (a *App) GetScanStatus() string {
	a.scanMu.Lock()
	defer a.scanMu.Unlock()
	return ok(map[string]interface{}{
		"scanning": a.scanning,
		"message":  a.scanMsg,
		"done":     a.scanDone,
		"total":    a.scanTotal,
		"phase":    a.scanPhase,
	}, "")
}

// Rescan 重新执行全量扫描，重建全局依赖图。
func (a *App) Rescan() string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}

	missing, err := manager.ValidateRoot()
	if err != nil {
		return fail(err.Error())
	}
	if len(missing) > 0 {
		return ok(map[string]interface{}{
			"missing": missing,
			"valid":   false,
		}, fmt.Sprintf("加载错误：缺少必需的目录 %s", strings.Join(missing, "、")))
	}

	a.startScan(manager)
	return ok(nil, "已触发重新扫描。")
}

// GetCardPreview 返回指定卡片文件的缩略图（base64 data URI）。
//
// Wails 的 WebView 无法直接加载 file:// 本地绝对路径，
// 因此必须由后端读取并缩放后以 data URI 返回。
func (a *App) GetCardPreview(path string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}

	data, err := manager.BuildPreview(path)
	if err != nil {
		// 预览失败不视为致命错误：返回 data 中的 message，由前端降级显示文件名
		return ok(PreviewData{Message: err.Error()}, "")
	}
	return ok(data, "")
}

// GetFolderPreview 返回文件夹的预览信息：
// 第一级子文件夹数量、卡片数量，以及最多 3 个子文件夹的首张卡片缩略图。
func (a *App) GetFolderPreview(dir string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}

	abs, err := filepath.Abs(strings.TrimSpace(dir))
	if err != nil {
		return fail("路径无效。")
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return fail("该路径不是文件夹。")
	}

	stats := computeFolderStats(abs)

	// 为每个子文件夹找一张代表性卡片作为预览图
	type previewItem struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		Count   int    `json:"count"`
		DataURI string `json:"dataUri"`
		Message string `json:"message"`
	}
	items := make([]previewItem, 0, len(stats.PreviewFolders))
	for _, f := range stats.PreviewFolders {
		item := previewItem{Name: f.Name, Path: f.Path, Count: f.Count}
		// 取该子文件夹内的第一张卡片
		if sample := findSampleCard(f.Path); sample != "" {
			if data, err := manager.BuildPreview(sample); err == nil {
				item.DataURI = data.DataURI
			} else {
				item.Message = err.Error()
			}
		} else {
			item.Message = "该文件夹内没有卡片"
		}
		items = append(items, item)
	}

	return ok(map[string]interface{}{
		"stats":            stats,
		"previews":         items,
		"remainingFolders": stats.RemainingFolders,
	}, "")
}

// findSampleCard 找到目录下的第一张卡片文件（用于生成文件夹预览图）。
func findSampleCard(dir string) string {
	var found string
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if core.IsCardFile(path) {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// ---------------------------------------------------------------------------
// 2. 树形数据
// ---------------------------------------------------------------------------

// TreeNode 表示前端列表中的一个条目。
// path 为该条目所在目录相对于类型根目录的路径，空串表示根目录。
type TreeNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"` // card / folder / hidden
	Path string `json:"path"`
	// Rel 相对其所属根的 POSIX 路径。索引回查必须用它，不能用 ID（绝对路径）。
	Rel  string `json:"rel,omitempty"`
	Size int64  `json:"size"`
	// FolderStats 文件夹统计信息（仅 folder 类型有值）
	FolderStats *FolderStats `json:"folderStats,omitempty"`
}

// FolderStats 描述一个文件夹的直接子项统计。
type FolderStats struct {
	// SubfolderCount 第一级子文件夹数量
	SubfolderCount int `json:"subfolderCount"`
	// CardCount 第一级卡片数量
	CardCount int `json:"cardCount"`
	// TotalFiles 文件夹内文件总数（递归）
	TotalFiles int `json:"totalFiles"`
	// TotalSize 文件夹内文件总大小
	TotalSize int64 `json:"totalSize"`
	// PreviewFolders 用于预览的前 3 个子文件夹
	PreviewFolders []FolderPreview `json:"previewFolders,omitempty"`
	// RemainingFolders 超出 3 个时剩余的子文件夹名
	RemainingFolders []string `json:"remainingFolders,omitempty"`
}

// FolderPreview 一个子文件夹的预览信息。
type FolderPreview struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Count int    `json:"count"`
}

// TreeData 是某一侧列表的完整数据。
type TreeData struct {
	Mode     string     `json:"mode"`
	Root     string     `json:"root"`
	RootName string     `json:"rootName"`
	Nodes    []TreeNode `json:"nodes"`
}

// GetLeftTreeData 返回游戏原始目录中指定类型的资源列表。
func (a *App) GetLeftTreeData(mode string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}

	kind, found := KindByMode(mode)
	if !found {
		return fail(fmt.Sprintf("未知的资源类型: %s", mode))
	}

	root := manager.SourceDir(kind)
	nodes := manager.buildTree(root, false, kind)
	return ok(TreeData{
		Mode:     mode,
		Root:     root,
		RootName: kind.SourceRel,
		Nodes:    nodes,
	}, "")
}

// GetRightTreeData 返回隔离区中指定类型的资源列表。
func (a *App) GetRightTreeData(mode string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}

	kind, found := KindByMode(mode)
	if !found {
		return fail(fmt.Sprintf("未知的资源类型: %s", mode))
	}

	root := manager.DelDir(kind)
	nodes := manager.buildTree(root, true, kind)
	return ok(TreeData{
		Mode:     mode,
		Root:     root,
		RootName: kind.DelName,
		Nodes:    nodes,
	}, "")
}

// buildTree 递归扫描目录，返回扁平的节点列表（前端按 path 字段做目录匹配）。
//
// 扫描过程在后台协程中进行，可能同时修改 Manager 的索引状态，
// 因此这里必须持有锁，避免数据竞争。
func (m *Manager) buildTree(root string, isDel bool, kind ResourceKind) []TreeNode {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buildTreeLocked(root, isDel, kind)
}

// buildTreeLocked 构建扁平节点列表。
//
// kind 决定哪些文件算「本类型的资源」。
// 早前版本只要不是目录就当成 card 节点 —— 于是 Cache_*.jpg、Halo.ppk
// 之类混进列表；全选隔离还会把它们一起移走，用户根本没想动这些文件。
func (m *Manager) buildTreeLocked(root string, isDel bool, kind ResourceKind) []TreeNode {
	nodes := make([]TreeNode, 0, 64)
	if root == "" {
		return nodes
	}

	// 一次后序遍历收集整棵树的文件夹统计。
	// §5.6：旧实现对每个文件夹单独递归统计，等于遍历 N 次全树（O(n²)）。
	agg := newStatsAggregator(root)

	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		if rel == "." {
			return nil
		}
		parent := filepath.Dir(rel)
		if parent == "." {
			parent = ""
		}

		if info.IsDir() {
			nodes = append(nodes, TreeNode{
				ID:          path,
				Rel:         filepath.ToSlash(rel),
				Name:        info.Name(),
				Type:        "folder",
				Path:        filepath.ToSlash(parent),
				FolderStats: agg.toFolderStats(path),
			})
			return nil
		}

		// 只收录本类型的资源文件（v2.2 FIX-06）。
		// 目录仍要继续 walk，但目录里的非资源文件一律跳过。
		if !core.IsCardFile(path) {
			return nil
		}

		nodes = append(nodes, TreeNode{
			ID:   path,
			Rel:  filepath.ToSlash(rel),
			Name: info.Name(),
			Type: "card",
			Path: filepath.ToSlash(parent),
			Size: info.Size(),
		})
		return nil
	})

	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Path != nodes[j].Path {
			return nodes[i].Path < nodes[j].Path
		}
		// 文件夹排在文件前面
		if (nodes[i].Type == "folder") != (nodes[j].Type == "folder") {
			return nodes[i].Type == "folder"
		}
		return nodes[i].Name < nodes[j].Name
	})
	return nodes
}

// computeFolderStats 统计文件夹的第一级子项与整体规模。
//
// 用于前端在焦点位于文件夹时展示摘要与 2x2 预览窗格。
func computeFolderStats(dir string) *FolderStats {
	stats := &FolderStats{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return stats
	}

	var subfolders []FolderPreview
	for _, e := range entries {
		if e.IsDir() {
			stats.SubfolderCount++
			child := filepath.Join(dir, e.Name())
			subfolders = append(subfolders, FolderPreview{
				Name:  e.Name(),
				Path:  child,
				Count: countFilesInDir(child),
			})
			continue
		}
		stats.CardCount++
	}
	sort.Slice(subfolders, func(i, j int) bool { return subfolders[i].Name < subfolders[j].Name })

	// 最多取 3 个做预览，其余记录名字
	if len(subfolders) > 3 {
		stats.PreviewFolders = subfolders[:3]
		for _, f := range subfolders[3:] {
			stats.RemainingFolders = append(stats.RemainingFolders, f.Name)
		}
	} else {
		stats.PreviewFolders = subfolders
	}

	// 整体规模（递归）
	stats.TotalFiles, stats.TotalSize = dirSize(dir)
	return stats
}

// countFilesInDir 统计目录下的文件数量（递归）。
func countFilesInDir(dir string) int {
	count := 0
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		count++
		return nil
	})
	return count
}

// dirSize 统计目录下的文件数量与总大小（递归）。
func dirSize(dir string) (int, int64) {
	var count int
	var size int64
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		count++
		size += info.Size()
		return nil
	})
	return count, size
}

// ---------------------------------------------------------------------------
// 3. 隔离 / 恢复
// ---------------------------------------------------------------------------

// ExecuteIsolate 执行隔离操作，返回包含操作日志的 JSON。
func (a *App) ExecuteIsolate(cardPaths []string, mode string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	if _, ok := KindByMode(mode); !ok {
		return fail(fmt.Sprintf("未知的资源类型: %s", mode))
	}
	if len(cardPaths) == 0 {
		return fail("没有选中任何文件。")
	}

	result := manager.Isolate(cardPaths)
	// 把操作日志写入后端环形缓冲，前端可随时回看
	for _, entry := range result.Logs {
		a.emitLog(entry.Level, "isolate", entry.Message)
	}
	return ok(result, fmt.Sprintf("隔离完成：%d 个资源。", result.MovedCards))
}

// ExecuteRestore 执行恢复操作，返回包含操作日志的 JSON。
func (a *App) ExecuteRestore(cardPaths []string, mode string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	if len(cardPaths) == 0 {
		return fail("没有选中任何文件。")
	}

	result := manager.Restore(cardPaths)
	for _, entry := range result.Logs {
		a.emitLog(entry.Level, "isolate", entry.Message)
	}
	return ok(result, fmt.Sprintf("恢复完成：%d 个资源。", result.RestoredCard))
}

// ---------------------------------------------------------------------------
// 4. 自由修改模式：复制 / 剪切 / 粘贴
// ---------------------------------------------------------------------------

// SetClipboard 把选中的条目放入剪贴板。
//
// op 为 "copy" 或 "cut"；side 为 "kks"（游戏侧）或 "del"（隔离区侧）。
func (a *App) SetClipboard(op string, paths []string, side string, mode string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	if op != ClipboardCopy && op != ClipboardCut {
		return fail(fmt.Sprintf("未知的剪贴板操作: %s", op))
	}
	if len(paths) == 0 {
		return fail("没有选中任何条目。")
	}

	manager.clipboard.Set(op, paths, side, mode)
	state := manager.clipboard.Get()
	return ok(state, fmt.Sprintf("已%s %d 个条目。", map[string]string{
		ClipboardCopy: "复制", ClipboardCut: "剪切",
	}[op], len(paths)))
}

// GetClipboard 返回当前剪贴板状态。
func (a *App) GetClipboard() string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return ok(ClipboardState{}, "")
	}
	return ok(manager.clipboard.Get(), "")
}

// ClearClipboard 清空剪贴板。
func (a *App) ClearClipboard() string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	manager.clipboard.Clear()
	return ok(nil, "已清空剪贴板。")
}

// PasteClipboard 把剪贴板内容粘贴到目标目录。
func (a *App) PasteClipboard(targetDir string, targetSide string, targetMode string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	if strings.TrimSpace(targetDir) == "" {
		return fail("请先在目标栏中选中一个文件夹作为粘贴位置。")
	}

	result := manager.Paste(targetDir, targetSide, targetMode)
	for _, entry := range result.Logs {
		a.emitLog(entry.Level, "paste", entry.Message)
	}
	return ok(result, fmt.Sprintf("粘贴完成：处理 %d 个条目。", result.Affected))
}

// ---------------------------------------------------------------------------
// 5. 隐藏空文件夹
// ---------------------------------------------------------------------------

// HideFolders 隐藏指定的空文件夹。
func (a *App) HideFolders(paths []string, side string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	if len(paths) == 0 {
		return fail("没有选中任何文件夹。")
	}

	result := manager.invisible.HideFolders(paths, side)
	for _, entry := range result.Logs {
		a.emitLog(entry.Level, "hide", entry.Message)
	}
	return ok(result, fmt.Sprintf("隐藏完成：%d 个文件夹。", result.Hidden))
}

// RestoreInvisibleFolders 恢复被隐藏的文件夹。
func (a *App) RestoreInvisibleFolders(paths []string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	if len(paths) == 0 {
		return fail("没有选中任何文件夹。")
	}

	result := manager.invisible.RestoreFolders(paths)
	return ok(result, fmt.Sprintf("恢复完成：%d 个文件夹。", result.Restored))
}

// GetInvisibleTreeData 返回「隐藏的文件夹」管理界面的数据。
//
// left 为被隐藏的 KKS 侧文件夹，right 为被隐藏的 DEL 侧文件夹。
func (a *App) GetInvisibleTreeData() string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}

	kksFolders, err := manager.invisible.LoadKKSFolders()
	if err != nil {
		return fail(err.Error())
	}
	delFolders, err := manager.invisible.LoadDELFolders()
	if err != nil {
		return fail(err.Error())
	}

	build := func(folders []string, root string) []TreeNode {
		nodes := make([]TreeNode, 0, len(folders))
		for _, rel := range folders {
			abs := filepath.Join(root, filepath.FromSlash(rel))
			// 展示相对路径，便于用户辨认位置
			display := filepath.ToSlash(rel)
			nodes = append(nodes, TreeNode{
				ID: abs,
				// Rel 是索引 key 用的相对路径（POSIX）。
				// ID 绝对路径用于展示与定位，Rel 用于精确回查索引 ——
				// 二者混用会导致「找不到隐藏记录」。
				Rel:  display,
				Name: display,
				Type: "hidden",
				Path: "",
			})
		}
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
		return nodes
	}

	return ok(map[string]interface{}{
		"left": TreeData{
			Mode:     "invisible",
			Root:     manager.KksRoot,
			RootName: "KKS 隐藏的文件夹",
			Nodes:    build(kksFolders, manager.KksRoot),
		},
		"right": TreeData{
			Mode:     "invisible",
			Root:     manager.DelCardsRoot,
			RootName: "DEL 隐藏的文件夹",
			Nodes:    build(delFolders, manager.DelCardsRoot),
		},
	}, "")
}

// ---------------------------------------------------------------------------
// 6. 右栏面板（v2.1 §3.3）
// ---------------------------------------------------------------------------

// GetRightPanel 返回右栏面板数据。
//
// 一次请求出全部（红线 N6）：两个区域、缩略图缓存命中情况、占用统计。
// 全程不做 PNG 解码（红线 N1）。
func (a *App) GetRightPanel(side, mode, rel, focusAbs string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	panel, err := manager.BuildPanel(side, mode, rel, focusAbs)
	if err != nil {
		return fail(err.Error())
	}
	return ok(panel, "")
}

// GetBigPreview 单卡大图（720×960）。
//
// 只查缓存；未命中返回 pending 并把任务入队，
// 生成完成后经 thumb:ready（key 带 -l 后缀）推给前端。
func (a *App) GetBigPreview(path string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	key, uri, pending := manager.requestLargeThumb(path)
	return ok(map[string]interface{}{
		"thumbKey": key,
		"dataUri":  uri,
		"pending":  pending,
	}, "")
}

// GetThumbBatch 批量查询缩略图。
//
// 只查缓存，未命中返回 pending=true（由后台队列生成后经 thumb:ready 事件推送）。
func (a *App) GetThumbBatch(paths []string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	results := make([]preview.Result, 0, len(paths))
	for _, p := range paths {
		key, uri, pending := manager.RequestThumb(p)
		results = append(results, preview.Result{
			ThumbKey: key,
			DataURI:  uri,
			Pending:  pending,
		})
	}
	return ok(results, "")
}

// notifyThumbsReady 批量推送已生成的缩略图。
func (a *App) notifyThumbsReady(results []preview.Result) {
	if a.ctx == nil || len(results) == 0 {
		return
	}

	// 首张出图埋点（§10.3）：确认队列真的在工作，而不是"看起来在等"。
	a.thumbOnce.Do(func() {
		elapsed := time.Since(a.startedAt).Milliseconds()
		a.emitLog("info", "preview",
			fmt.Sprintf("[preview] 首张缩略图生成成功，耗时 %dms（累计 %d 张）", elapsed, len(results)))
	})

	wruntime.EventsEmit(a.ctx, "thumb:ready", results)
}

// logThumbError 记录单张缩略图生成失败。
//
// 不允许静默丢弃 —— 否则以后又是一轮「图片不显示但我不知道为什么」。
func (a *App) logThumbError(path string, err error) {
	a.emitLog("warn", "preview", fmt.Sprintf("[preview] 生成失败 %s: %v", path, err))
}

// GetThumbStats 返回缩略图队列统计（诊断用）。
//
// 验收手段：选完根目录后 misses > 0 且 3 秒内 generated > 0；
// 切走再切回同一目录，generated 不增长（证明缓存生效）。
func (a *App) GetThumbStats() string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	stats := manager.ThumbStats()
	return ok(map[string]interface{}{
		"stats": stats,
		// running=false 说明队列没启动 —— 这正是 FIX-01 的判别依据
		"running": manager.ThumbQueueRunning(),
	}, "")
}

// ---------------------------------------------------------------------------
// 7. 操作历史与撤销（v2.1 §8）
// ---------------------------------------------------------------------------

// GetHistory 返回操作历史（最新在前）。
func (a *App) GetHistory(offset, limit int) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return ok([]tx.HistoryEntry{}, "")
	}
	list := manager.GetHistory(offset, limit)
	return ok(map[string]interface{}{
		"entries": list,
		"total":   len(manager.GetHistory(0, 0)) + len(list),
		"max":     tx.MaxHistory,
	}, "")
}

// CmdUndo 撤销一个已提交的操作。
func (a *App) CmdUndo(txID string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	res, err := manager.CmdUndo(txID)
	if err != nil {
		a.emitLog("warn", "history", "撤销失败："+err.Error())
		return fail(err.Error())
	}
	a.emitLog("success", "history", fmt.Sprintf("已撤销操作 %s（%d 步）", txID, res.Reverted))
	// 通知前端刷新历史窗口
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "history:changed", map[string]interface{}{})
	}
	return ok(res, fmt.Sprintf("已撤销 %d 个步骤。", res.Reverted))
}

// recoverPendingTx 恢复未完成的事务（启动时调用）。
func (a *App) recoverPendingTx() {
	// 优先用当前 manager 的隔离区；没有 manager 时，扫描常见位置
	var roots []string
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()
	if manager != nil {
		roots = append(roots, manager.DelCardsRoot)
	}

	for _, root := range roots {
		recovered, errs := tx.RecoverPending(root)
		if recovered > 0 {
			a.emitLog("warn", "history",
				fmt.Sprintf("检测到上次未完成的操作，已自动还原 %d 项。", recovered))
		}
		for _, e := range errs {
			a.emitLog("error", "history", "自动还原失败："+e)
		}
	}
}

// ---------------------------------------------------------------------------
// 8. 自由模式 CRUD（v2.1 §6）
// ---------------------------------------------------------------------------

// CmdMkdir 在当前目录下新建文件夹。
//
// 返回实际创建名（默认名冲突时自动加 (2)）。
func (a *App) CmdMkdir(side, mode, parentRel string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	res := manager.CmdMkdir(side, mode, parentRel)
	if !res.OK {
		return fail(res.Message)
	}
	a.emitLog("success", "crud", "已创建文件夹："+res.Name)
	return ok(res, "已创建「"+res.Name+"」")
}

// CmdRename 重命名一个条目。
func (a *App) CmdRename(pathAbs, newName string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	res := manager.CmdRename(pathAbs, newName)
	if !res.OK {
		return fail(res.Message)
	}
	a.emitLog("success", "crud", "已重命名为："+res.Name)
	return ok(res, "已重命名为「"+res.Name+"」")
}

// PlanDelete 生成删除计划（零副作用）。
func (a *App) PlanDelete(paths []string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	plan := manager.PlanDelete(paths)
	return ok(plan, fmt.Sprintf("将把 %d 个项目移入回收站（共 %s）", len(plan.Items), formatBytes(plan.TotalBytes)))
}

// CommitDelete 执行删除（移入回收站）。
func (a *App) CommitDelete(paths []string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	moved, errs, err := manager.CommitDelete(paths)
	if err != nil {
		for _, e := range errs {
			a.emitLog("error", "crud", e)
		}
		return fail(err.Error())
	}
	a.emitLog("success", "crud", fmt.Sprintf("已移入回收站 %d 个项目。", moved))
	return ok(map[string]interface{}{"moved": moved}, fmt.Sprintf("已移入回收站 %d 个项目。", moved))
}

// GetTrashTree 返回回收站列表。
func (a *App) GetTrashTree() string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return ok([]interface{}{}, "")
	}
	return ok(manager.GetTrashTree(), "")
}

// CmdTrashRestore 还原回收站条目。
func (a *App) CmdTrashRestore(trashRels []string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	res := manager.CmdTrashRestore(trashRels)
	if res.Restored > 0 {
		a.emitLog("success", "crud", fmt.Sprintf("已还原 %d 个项目。", res.Restored))
	}
	for _, c := range res.Conflicts {
		a.emitLog("warn", "crud", c.Reason)
	}
	return ok(res, fmt.Sprintf("已还原 %d 个项目。", res.Restored))
}

// CmdTrashPurge 彻底删除回收站条目（需输入名称确认）。
func (a *App) CmdTrashPurge(trashRels []string, confirmText string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	res := manager.CmdTrashPurge(trashRels, confirmText)
	for _, e := range res.Errors {
		a.emitLog("error", "crud", e)
	}
	if len(res.Errors) > 0 {
		return fail(strings.Join(res.Errors, "; "))
	}
	a.emitLog("success", "crud", fmt.Sprintf("已彻底删除 %d 个项目。", res.Purged))
	return ok(res, fmt.Sprintf("已彻底删除 %d 个项目。", res.Purged))
}

// formatBytes 把字节数格式化为易读文本。
func formatBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// ---------------------------------------------------------------------------
// 10. 辅助接口
// ---------------------------------------------------------------------------

// GetKKSInfo 返回当前 KKS 根目录与隔离区路径，以及各资源类型的目录映射。
func (a *App) GetKKSInfo() string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}

	kinds := make([]map[string]string, 0, len(ResourceKinds))
	for _, k := range ResourceKinds {
		kinds = append(kinds, map[string]string{
			"mode":      k.Mode,
			"name":      k.Name,
			"source":    manager.SourceDir(k),
			"del":       manager.DelDir(k),
			"sourceRel": k.SourceRel,
		})
	}
	return ok(map[string]interface{}{
		"kksRoot":      manager.KksRoot,
		"delCardsRoot": manager.DelCardsRoot,
		"kinds":        kinds,
	}, "")
}

// OpenDelCardsRoot 在文件管理器中打开隔离区目录。
func (a *App) OpenDelCardsRoot() string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	if err := openDirectory(manager.DelCardsRoot); err != nil {
		return fail(fmt.Sprintf("打开目录失败: %v", err))
	}
	return ok(nil, "已打开隔离区目录。")
}

// OpenFolderInExplorer 在文件管理器中打开指定目录。
//
// 相对 OpenDelCardsRoot 的区别：这里打开的是**用户当前浏览的目录**，
// 空白区菜单的「在资源管理器中打开」应当指向它，而不是固定的隔离区根。
// 路径必须位于受管范围内，避免被当作任意路径执行入口。
// OpenFolderInExplorer 在文件管理器中打开指定路径。
//
// 传目录 → 打开该目录；传文件 → 打开其父目录并选中该文件。
// 路径会先规范化成纯本地分隔符的绝对路径 —— 早期版本直接把前端拼出的
// 混合分隔符路径（如 E://KKV114//...//female/22.7.16）交给 explorer，
// Windows 资源管理器会把它当成无效参数，退回默认的「文档」目录。
func (a *App) OpenFolderInExplorer(absPath string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	raw := strings.TrimSpace(absPath)
	if raw == "" {
		return fail("未指定路径。")
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return fail("路径无效：" + err.Error())
	}
	abs = filepath.Clean(filepath.FromSlash(abs))
	if !manager.pathAllowed(abs) {
		return fail("该路径不在允许的范围内：" + abs)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fail("路径不存在：" + abs)
	}
	if info.IsDir() {
		if err := openDirectory(abs); err != nil {
			return fail(fmt.Sprintf("打开目录失败: %v", err))
		}
		return ok(nil, "已在资源管理器中打开。")
	}
	if err := revealFile(abs); err != nil {
		return fail(fmt.Sprintf("打开文件所在目录失败: %v", err))
	}
	return ok(nil, "已在资源管理器中选中该文件。")
}

// OpenListDirInExplorer 打开当前列表正在浏览的目录。
//
// 由后端按 side + mode + rel 解析，避免前端自己拼路径拼出混合分隔符。
func (a *App) OpenListDirInExplorer(side, mode, rel string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	dir := manager.resolveRelDir(mode, side, rel)
	if dir == "" {
		return fail("当前视图没有对应的磁盘目录。")
	}
	dir = filepath.Clean(filepath.FromSlash(dir))
	if !manager.pathAllowed(dir) {
		return fail("该目录不在允许的范围内：" + dir)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fail("目录不存在：" + dir)
	}
	if err := openDirectory(dir); err != nil {
		return fail(fmt.Sprintf("打开目录失败: %v", err))
	}
	return ok(nil, "已在资源管理器中打开当前目录。")
}

// revealFile 打开父目录并选中指定文件。
func revealFile(abs string) error {
	clean := filepath.Clean(filepath.FromSlash(abs))
	switch runtime.GOOS {
	case "windows":
		return exec.Command("explorer", "/select,"+clean).Start()
	case "darwin":
		return exec.Command("open", "-R", clean).Start()
	default:
		return exec.Command("xdg-open", filepath.Dir(clean)).Start()
	}
}

// openDirectory 跨平台打开目录。
func openDirectory(path string) error {
	clean := filepath.Clean(filepath.FromSlash(path))
	switch runtime.GOOS {
	case "windows":
		return exec.Command("explorer", clean).Start()
	case "darwin":
		return exec.Command("open", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}
