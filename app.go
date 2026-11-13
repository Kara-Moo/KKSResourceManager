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

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

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
}

// NewApp 创建 App 实例。
func NewApp() *App {
	return &App{
		logs: newLogRing(5000),
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
		if err := manager.StartThumbQueue(a.notifyThumbsReady); err != nil {
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

	a.mu.Lock()
	a.manager = manager
	a.mu.Unlock()

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
	a.scanPhase = ""
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

		msg := fmt.Sprintf("扫描完成：zipmod %d 个，资源文件 %d 个，依赖 GUID %d 个，耗时 %d ms",
			result.ZipmodCount, result.ResourceCount, result.DepGUIDCount, result.ScanDurationMS)
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
		"zipmodCount":   result.ZipmodCount,
		"resourceCount": result.ResourceCount,
		"depGuidCount":  result.DepGUIDCount,
		"failedFiles":   result.FailedFiles,
		"missingMods":   result.MissingMods,
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
		if strings.EqualFold(filepath.Ext(path), ".png") {
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
	Type string `json:"type"` // card / folder / mod / hidden
	Path string `json:"path"`
	// Rel 相对其所属根的 POSIX 路径。索引回查必须用它，不能用 ID（绝对路径）。
	Rel string `json:"rel,omitempty"`
	// GUID zipmod 的 GUID（仅 mod 类型有值）
	GUID string `json:"guid,omitempty"`
	// RefCount 被多少个文件占用（卡片 + 插件）。
	// 仅 mod 类型有值；由索引内存表直接得出，不做任何磁盘访问（红线 N2）。
	RefCount int `json:"refCount"`
	// RefCards / RefPlugins 占用明细，用于 tooltip 与详情弹窗
	RefCards   int      `json:"refCards"`
	RefPlugins int      `json:"refPlugins"`
	Mods       []string `json:"mods"`
	// ModNames 依赖的 zipmod 文件名列表
	ModNames []string `json:"modNames"`
	Size     int64    `json:"size"`
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
	nodes := manager.buildTree(root, false)
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
	nodes := manager.buildTree(root, true)
	return ok(TreeData{
		Mode:     mode,
		Root:     root,
		RootName: kind.DelName,
		Nodes:    nodes,
	}, "")
}

// GetModsTreeData 返回隔离区中的 zipmod 列表（扁平结构）。
func (a *App) GetModsTreeData() string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}

	root := manager.DelZipmodDir()
	nodes := manager.buildModsTree(root)
	return ok(TreeData{
		Mode:     ModeZipmod,
		Root:     root,
		RootName: delZipmodDirName,
		Nodes:    nodes,
	}, "")
}

// GetInstalledModsData 返回游戏 mods 目录中已安装的 zipmod 列表（只读）。
func (a *App) GetInstalledModsData() string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}

	root := manager.ModsDir()
	nodes := manager.buildModsTree(root)
	return ok(TreeData{
		Mode:     ModeZipmod,
		Root:     root,
		RootName: "mods",
		Nodes:    nodes,
	}, "")
}

// buildTree 递归扫描目录，返回扁平的节点列表（前端按 path 字段做目录匹配）。
//
// 扫描过程在后台协程中进行，可能同时修改 Manager 的依赖索引，
// 因此这里必须持有锁，避免数据竞争。
// depsFor 在解析失败时会回写 DelFileDeps 缓存，故使用写锁。
func (m *Manager) buildTree(root string, isDel bool) []TreeNode {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buildTreeLocked(root, isDel)
}

func (m *Manager) buildTreeLocked(root string, isDel bool) []TreeNode {
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

		guids := m.depsFor(path, isDel)
		names := make([]string, 0, len(guids))
		for _, guid := range guids {
			if modPath, ok := m.ZipmodDB[guid]; ok {
				names = append(names, filepath.Base(modPath))
				continue
			}
			names = append(names, fmt.Sprintf("未知(%s)", guid))
		}
		nodes = append(nodes, TreeNode{
			ID:       path,
			Rel:      filepath.ToSlash(rel),
			Name:     info.Name(),
			Type:     "card",
			Path:     filepath.ToSlash(parent),
			Mods:     guids,
			ModNames: names,
			Size:     info.Size(),
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

// buildModsTree 构建 zipmod 列表（扁平，不含子目录）。
func (m *Manager) buildModsTree(root string) []TreeNode {
	m.mu.Lock()
	defer m.mu.Unlock()
	nodes := make([]TreeNode, 0, 32)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nodes
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		full := filepath.Join(root, e.Name())
		// GUID 优先取索引（O(1)，索引里已有解析结果）；
		// 索引没有（如刚隔离进来的新文件）才实时解析一次。
		guid := ""
		if m.index != nil {
			if entry, ok := m.index.Zipmods[full]; ok {
				guid = entry.GUID
			}
		}
		if guid == "" {
			if manifest, err := ReadModManifest(full); err == nil {
				guid = manifest.GUID
			}
		}
		node := TreeNode{
			ID:   full,
			Name: e.Name(),
			Type: "mod",
			Path: "",
			Size: info.Size(),
		}
		if guid != "" {
			node.GUID = guid
			node.Mods = []string{guid}
			node.ModNames = []string{e.Name()}
			// 占用数：O(1) 内存查询（红线 N2）
			// 绝不为了显示这个数字去 os.Stat 每个引用文件。
			node.RefCards = len(m.GlobalDepMap[guid])
			node.RefPlugins = len(m.PluginDepMap[guid])
			node.RefCount = node.RefCards + node.RefPlugins
		}
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes
}

// depsFor 返回文件依赖的 GUID 列表。
func (m *Manager) depsFor(path string, isDel bool) []string {
	if isDel {
		if deps, ok := m.DelFileDeps[path]; ok {
			return deps
		}
		// 隔离区可能是上次会话遗留，实时解析一次
		if guids, err := ParseCardGUIDs(path); err == nil && len(guids) > 0 {
			m.DelFileDeps[path] = guids
			return guids
		}
		return nil
	}
	if deps, ok := m.FileDeps[path]; ok {
		return deps
	}
	return nil
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
	return ok(result, fmt.Sprintf("隔离完成：%d 个资源，%d 个 MOD", result.MovedCards, result.MovedMods))
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
	return ok(result, fmt.Sprintf("恢复完成：%d 个资源，%d 个 MOD", result.RestoredCard, result.RestoredMod))
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
			name := filepath.Base(abs)
			// 展示相对路径，便于用户辨认位置
			display := filepath.ToSlash(rel)
			nodes = append(nodes, TreeNode{
				ID: abs,
				// Rel 是索引 key 用的相对路径（POSIX）。
				// ID 绝对路径用于展示与定位，Rel 用于精确回查索引 ——
				// 二者混用会导致「找不到隐藏记录」。
				Rel:      display,
				Name:     display,
				Type:     "hidden",
				Path:     "",
				Mods:     []string{},
				ModNames: []string{name},
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
	wruntime.EventsEmit(a.ctx, "thumb:ready", results)
}

// ZipmodDetail zipmod 的占用详情。
type ZipmodDetail struct {
	GUID       string         `json:"guid"`
	Path       string         `json:"path"`
	Base       string         `json:"base"`
	Size       int64          `json:"size"`
	Cards      []string       `json:"cards"`
	Plugins    []PluginDetail `json:"plugins"`
	RefCards   int            `json:"refCards"`
	RefPlugins int            `json:"refPlugins"`
	ParseErr   string         `json:"parseErr"`
}

// PluginDetail 一个 DLL 的命中详情。
type PluginDetail struct {
	Name    string   `json:"name"`
	Rel     string   `json:"rel"`
	Abs     string   `json:"abs"`
	HitMode string   `json:"hitMode"`
	GUIDs   []string `json:"guids"`
}

// GetZipmodDetail 返回 zipmod 的完整占用详情。
//
// 这是排查「为什么这个 mod 隔离不了」的关键信息。
func (a *App) GetZipmodDetail(guid string) string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}
	detail := manager.ZipmodDetailOf(guid)
	return ok(detail, "")
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
// 8. 辅助接口
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
			"mode":     k.Mode,
			"name":     k.Name,
			"source":   manager.SourceDir(k),
			"del":      manager.DelDir(k),
			"sourceRel": k.SourceRel,
		})
	}
	return ok(map[string]interface{}{
		"kksRoot":      manager.KksRoot,
		"delCardsRoot": manager.DelCardsRoot,
		"modsDir":      manager.ModsDir(),
		"zipmodDel":    manager.DelZipmodDir(),
		"kinds":        kinds,
	}, "")
}

// GetGlobalDepInfo 返回全局依赖图的统计信息，用于验证隔离安全性。
func (a *App) GetGlobalDepInfo() string {
	a.mu.RLock()
	manager := a.manager
	a.mu.RUnlock()

	if manager == nil {
		return fail("请先选择 KKS 根目录。")
	}

	manager.mu.RLock()
	defer manager.mu.RUnlock()

	guidCount := len(manager.GlobalDepMap)
	refCount := 0
	for _, refs := range manager.GlobalDepMap {
		refCount += len(refs)
	}
	return ok(map[string]interface{}{
		"guidCount":    guidCount,
		"refCount":     refCount,
		"zipmodCount":  len(manager.ZipmodDB),
		"fileCount":    len(manager.FileDeps),
		"scanned":      manager.Scanned,
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

// openDirectory 跨平台打开目录。
func openDirectory(path string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("explorer", path).Start()
	case "darwin":
		return exec.Command("open", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}
