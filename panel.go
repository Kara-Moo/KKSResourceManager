package main

// panel.go —— 右栏面板组装（规格 §3.3）
//
// 关键约束：
//
//	N6 右栏面板数据必须来自**一次请求** —— 不允许前端发 N 个请求拼一个面板。
//	N7 禁止为预览另建一套扫描 —— 文件夹统计复用 folderstats.go，
//	   卡片依赖复用 index，封面走缓存。
//	N1 UI 请求路径禁止同步解码 PNG —— 缩略图只查缓存，未命中返回 pending。
//
// 数据来源全部是内存索引或带缓存的目录列举，全程不做 PNG 解码。

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"kksresourcemanager/internal/core"
	"kksresourcemanager/internal/preview"
)

// Tile 一个预览格子。
type Tile struct {
	// Kind folder | card | plugin | more | empty
	Kind string `json:"kind"`
	// Name 显示名
	Name string `json:"name"`
	// Rel 相对类型根的路径（POSIX）
	Rel string `json:"rel"`
	// Abs 绝对路径（前端用于定位/打开）
	Abs string `json:"abs"`
	// Badge 副标签："12 个文件" / ""（卡片格必须为空串）
	Badge string `json:"badge"`
	// ThumbKey 缓存键 = sha1(abs + "|" + size + "|" + mtime)
	ThumbKey string `json:"thumbKey"`
	// Thumb dataURI，未命中为空
	Thumb string `json:"thumb"`
	// ThumbPreview 占位用小图 dataURI（仅 single 布局使用）。
	//
	// 大图 720×960 生成较慢（约 60-120ms），先用 320 档顶上，
	// 避免用户看到一片空白。
	ThumbPreview string `json:"thumbPreview"`
	// Pending 缩略图未就绪
	Pending bool `json:"pending"`
	// Selected 焦点所在格
	Selected bool `json:"selected"`
	// RefCount 被多少个文件占用（仅 zipmod 卡片格使用）
	RefCount int `json:"refCount"`
}

// Section 右栏的一个区域。
type Section struct {
	// ID folders | cards | plugins
	ID string `json:"id"`
	// Title 区域标题
	Title string `json:"title"`
	// Layout grid | list
	Layout string `json:"layout"`
	// Total 该区域的条目总数
	Total int `json:"total"`
	// Tiles 渲染的格子
	Tiles []Tile `json:"tiles"`
	// More "还有"格的数量（0 = 无"还有"格）
	More int `json:"more"`
	// MoreLabel "还有 14 个文件夹"
	MoreLabel string `json:"moreLabel"`
	// Visible false 时前端整块不渲染
	Visible bool `json:"visible"`
	// Empty 非空且 Visible=true 时显示的空态文案
	Empty string `json:"empty"`
	// Lazy true 表示前端按可视区懒加载缩略图（zipmod 全量列表用）。
	//
	// 为什么需要：500 张引用若都预请求，等于 500 次 os.Stat + 500 个队列任务，
	// 切换焦点会瞬间打满队列，把真正需要的图挤到后面。
	Lazy bool `json:"lazy"`
}

// Panel 右栏完整数据。
type Panel struct {
	Focus struct {
		// Kind folder|card|mod|hidden|none
		Kind string `json:"kind"`
		Name string `json:"name"`
		Abs  string `json:"abs"`
		// Info 信息栏主文案
		Info string `json:"info"`
	} `json:"focus"`
	// Sections 固定长度 2，顺序 A、B
	Sections []Section `json:"sections"`
	// Gen 面板代数，前端用于丢弃过期响应
	Gen int64 `json:"gen"`
}

// tileCount 返回实际渲染的格子数与 "还有" 数量。
//
// n <= 9  : 全显示，无 "还有" 格
// n >  9 : 显示 8 个 + 1 个 "还有 (n-8)" 格
//
// 为什么是 8 而不是 9：9 格全占满就没位置放"还有 N"。
// 用户明确要"8 个预览 + 还有 XXX"。
func tileCount(n int) (tiles int, more int) {
	if n <= 9 {
		return n, 0
	}
	return 8, n - 8
}

// BuildPanel 组装右栏两个区域。
//
// side     "left" | "right"
// mode     当前管理种类（female_card / zipmod / invisible ...）
// rel      当前列表目录（相对类型根，POSIX）
// focusAbs 焦点条目的绝对路径（可空）
//
// 一次调用完成：目录列举（一级）、封面选取、占用统计、缩略图查缓存。
// 全程不递归遍历整棵树，不做 PNG 解码。
func (m *Manager) BuildPanel(side, mode, rel, focusAbs string) (Panel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var panel Panel

	// --- 1. 定位焦点目录 ---
	focusKind, focusName, focusAbsNorm := "none", "", ""
	dir := ""

	if focusAbs != "" {
		abs := focusAbs
		if info, err := os.Stat(abs); err == nil {
			if info.IsDir() {
				focusKind = "folder"
				dir = abs
			} else {
				focusKind = "file"
				dir = filepath.Dir(abs)
			}
			focusAbsNorm = abs
			focusName = filepath.Base(abs)
		}
	}
	if dir == "" {
		// 无焦点 → 当前列表目录
		dir = m.resolveRelDir(mode, side, rel)
	}

	panel.Focus.Kind = focusKind
	panel.Focus.Name = focusName
	panel.Focus.Abs = focusAbsNorm

	// --- zipmod 分支 ---
	if mode == ModeZipmod {
		return m.buildZipmodPanel(&panel, focusAbsNorm, focusKind), nil
	}

	// --- 卡片分支：单张大图（v2.2 FIX-02）---
	//
	// 必须在这里分流：九宫格是「文件夹视角」的语义，
	// 对单张卡是错误的 —— 用户点一张卡，想看的就是这一张。
	if focusKind == "file" && mode != ModeZipmod && mode != "invisible" && focusAbsNorm != "" {
		return m.buildCardPanel(&panel, focusAbsNorm), nil
	}

	// --- invisible 模式：无目录概念 ---
	if mode == "invisible" {
		panel.Sections = []Section{
			{ID: "folders", Title: "子文件夹", Layout: "grid", Visible: false},
			{ID: "cards", Title: "此文件夹的卡片", Layout: "grid", Visible: false},
		}
		panel.Focus.Info = m.invisibleFocusInfo(focusAbsNorm)
		panel.Gen = m.panelGen()
		return panel, nil
	}

	// --- 2. 列一级目录（命中缓存则零 IO）---
	snap, err := m.snapshots.ListDir(dir, func(name string, isDir bool) bool {
		if isDir {
			return true
		}
		return strings.EqualFold(filepath.Ext(name), ".png")
	})
	if err != nil {
		panel.Sections = []Section{
			{ID: "folders", Title: "子文件夹", Layout: "grid", Visible: false},
			{ID: "cards", Title: "此文件夹的卡片", Layout: "grid", Visible: false, Empty: "此文件夹为空"},
		}
		panel.Focus.Info = "无法读取目录"
		panel.Gen = m.panelGen()
		return panel, nil
	}

	var subDirs, subFiles []core.DirEntry
	for _, e := range snap.Entries {
		if e.IsDir {
			subDirs = append(subDirs, e)
		} else {
			subFiles = append(subFiles, e)
		}
	}

	// --- 3. 区域 A（子文件夹）---
	secA := Section{ID: "folders", Title: "子文件夹", Layout: "grid"}
	secA.Total = len(subDirs)
	if len(subDirs) == 0 {
		// 无子文件夹 → 整块隐藏（不占位、不留空白）
		secA.Visible = false
	} else {
		secA.Visible = true
		shown, more := tileCount(len(subDirs))
		for i := 0; i < shown && i < len(subDirs); i++ {
			d := subDirs[i]
			cover := m.pickCover(d.Path)
			t := Tile{
				Kind: "folder",
				Name: d.Name,
				Rel:  m.relOf(d.Path),
				Abs:  d.Path,
			}
			// badge = 该子文件夹的文件数
			t.Badge = strconv.Itoa(m.countFilesCached(d.Path)) + " 个文件"
			if cover != "" {
				t.ThumbKey, t.Thumb, t.Pending = m.thumbRequest(cover, true)
			} else {
				// 无封面：仍给一个 thumbKey，前端渲染占位
				t.ThumbKey = preview.ThumbKey(d.Path, preview.Fingerprint{})
				t.Pending = true
			}
			secA.Tiles = append(secA.Tiles, t)
		}
		if more > 0 {
			secA.More = more
			secA.MoreLabel = "还有 " + strconv.Itoa(more) + " 个文件夹"
			secA.Tiles = append(secA.Tiles, Tile{
				Kind: "more",
				Name: secA.MoreLabel,
				Badge: "",
			})
		}
	}

	// --- 4. 区域 B（卡片）---
	secB := Section{ID: "cards", Title: "此文件夹的卡片", Layout: "grid"}
	secB.Total = len(subFiles)
	if len(subFiles) == 0 {
		secB.Visible = false
	} else {
		secB.Visible = true
		shown, more := tileCount(len(subFiles))
		for i := 0; i < shown && i < len(subFiles); i++ {
			f := subFiles[i]
			t := Tile{
				Kind: "card",
				Name: f.Name,
				Rel:  m.relOf(f.Path),
				Abs:  f.Path,
				// 卡片格的 badge 必须为空串（用户明确要求不显示"N 个文件"）
				Badge: "",
			}
			t.ThumbKey, t.Thumb, t.Pending = m.thumbRequest(f.Path, true)
			if focusAbsNorm != "" && samePath(f.Path, focusAbsNorm) {
				t.Selected = true
			}
			secB.Tiles = append(secB.Tiles, t)
		}
		if more > 0 {
			secB.More = more
			secB.MoreLabel = "还有 " + strconv.Itoa(more) + " 张角色卡"
			secB.Tiles = append(secB.Tiles, Tile{
				Kind: "more",
				Name: secB.MoreLabel,
				Badge: "",
			})
		}
	}

	panel.Sections = []Section{secA, secB}
	panel.Focus.Info = m.panelFocusInfo(mode, dir, focusAbsNorm, focusKind)
	panel.Gen = m.panelGen()
	return panel, nil
}

// buildCardPanel 单卡片焦点：一张大图 + 信息（v2.2 FIX-02）。
//
// 不返回九宫格 —— 九宫格是「文件夹视角」，
// 用户点一张卡想看的就是这一张，铺一屏同类卡片没有意义。
func (m *Manager) buildCardPanel(panel *Panel, cardAbs string) Panel {
	sec := Section{
		ID:      "preview",
		Title:   "",
		Layout:  "single",
		Visible: true,
		Total:   1,
	}

	t := Tile{
		Kind:     "card",
		Name:     filepath.Base(cardAbs),
		Abs:      cardAbs,
		Rel:      m.relOf(cardAbs),
		Selected: true,
	}
	t.ThumbKey, t.Thumb, t.Pending = m.requestLargeThumb(cardAbs)
	// 大图未就绪时先用九宫格档顶上（同一队列，不同 key，不会互相覆盖）
	if t.Pending {
		_, small, smallPending := m.thumbRequest(cardAbs, true)
		if !smallPending {
			t.ThumbPreview = small
		}
	}

	sec.Tiles = []Tile{t}
	panel.Sections = []Section{sec}
	panel.Focus.Kind = "card"
	panel.Focus.Name = filepath.Base(cardAbs)
	panel.Focus.Abs = cardAbs
	panel.Focus.Info = m.cardInfoLine(cardAbs)
	panel.Gen = m.panelGen()
	return *panel
}

// cardInfoLine 卡片信息栏文案：大小 + 依赖数。
//
// 不再回填文件名 —— 标题已经是文件名，重复两行是 FIX-08。
func (m *Manager) cardInfoLine(abs string) string {
	var size int64
	if info, err := os.Stat(abs); err == nil {
		size = info.Size()
	}

	// 依赖数直接查内存索引，不做磁盘访问
	deps := 0
	if m.index != nil {
		if c, ok := m.index.Cards[abs]; ok {
			deps = len(c.GUIDs)
		}
	}
	if deps == 0 {
		if g, ok := m.FileDeps[abs]; ok {
			deps = len(g)
		}
	}

	sizeText := core.ParseSize(size)
	if size == 0 {
		sizeText = "0 B"
	}
	if deps == 0 {
		return sizeText + " · 无依赖"
	}
	return sizeText + " · 依赖 " + strconv.Itoa(deps) + " 个 zipmod"
}

// requestLargeThumb 请求单卡大图（720×960 档）。
//
// 与九宫格走同一个队列，但键带 "-l" 后缀、磁盘文件独立，
// 两档互不覆盖。
func (m *Manager) requestLargeThumb(abs string) (key string, dataURI string, pending bool) {
	fp := m.fingerprintOf(abs)
	key = preview.ThumbKeySized(abs, fp, preview.SizeLarge)
	if m.thumbs == nil {
		return key, "", true
	}
	uri, pend := m.thumbs.RequestSized(abs, fp, preview.SizeLarge, true)
	return key, uri, pend
}

// buildZipmodPanel 构建 zipmod 焦点的面板（规格 §4）。
//
// 区域 A = 使用该 zipmod 的卡片（九宫格）
// 区域 B = 引用该 GUID 的插件（列表）
func (m *Manager) buildZipmodPanel(panel *Panel, focusAbs, focusKind string) Panel {
	guid := m.guidOfPath[focusAbs]
	if guid == "" {
		// 兜底：从 ZipmodDB 反查。
		// guidOfPath 由 applyIndex 建立，但若索引尚未 apply
		// （例如刚扫描完还没落定），不能因此丢失整个面板。
		for g, p := range m.ZipmodDB {
			if samePath(p, focusAbs) {
				guid = g
				break
			}
		}
	}

	// --- 区域 A：引用它的卡片 ---
	secA := Section{ID: "cards", Title: "使用此 zipmod 的卡片", Layout: "grid"}
	var refs []string
	if guid != "" {
		refs = append(refs, m.GlobalDepMap[guid]...)
	}
	// 排序必须稳定且符合直觉：先按文件名自然序（22.7.9 在 22.7.10 之前），
	// 同名再按全路径。否则同一次刷新顺序会跳，用户会以为内容变了。
	sort.Slice(refs, func(i, j int) bool {
		bi, bj := filepath.Base(refs[i]), filepath.Base(refs[j])
		if bi != bj {
			return core.NaturalLess(bi, bj)
		}
		return refs[i] < refs[j]
	})

	secA.Total = len(refs)
	secA.Visible = true
	if len(refs) == 0 {
		// 零引用也是关键判断信息，必须显示
		secA.Empty = "未被任何卡片引用（可作为「多余 zipmod」隔离）"
	} else {
		// **全量返回，永不截断**（v2.2 FIX-03）。
		//
		// 规则差异的由来：文件夹九宫格是「概览」，194 张全铺开会卡死，所以要 8+more；
		// zipmod 是「审计」—— 用户要确认这个 mod 到底被谁用了，省略就失去意义。
		//
		// 这里**不调用 thumbRequest**：500 张引用 = 500 次 os.Stat + 500 个队列任务，
		// 切换焦点会瞬间打满队列，把真正需要的图挤到后面。
		// 只给 thumbKey，前端按可视区懒加载（Section.Lazy=true）。
		secA.Tiles = make([]Tile, 0, len(refs))
		for _, p := range refs {
			secA.Tiles = append(secA.Tiles, Tile{
				Kind:     "card",
				Name:     filepath.Base(p),
				Rel:      m.relOf(p),
				Abs:      p,
				Badge:    "",
				ThumbKey: preview.ThumbKey(p, m.fingerprintOf(p)),
				Pending:  true,
			})
		}
		secA.More = 0
		secA.Lazy = true
	}

	// --- 区域 B：引用它的插件 ---
	secB := Section{ID: "plugins", Title: "引用此 GUID 的插件", Layout: "list"}
	var plugins []string
	if guid != "" {
		plugins = append(plugins, m.PluginDepMap[guid]...)
	}
	sort.Strings(plugins)

	secB.Total = len(plugins)
	secB.Visible = true
	if len(plugins) == 0 {
		secB.Empty = "未被任何插件硬编码引用"
	} else {
		// 插件列表同样全量：zipmod 视角一律不省略（§3.3 决策表）
		shown, more := len(plugins), 0
		for i := 0; i < shown && i < len(plugins); i++ {
			rel := plugins[i]
			secB.Tiles = append(secB.Tiles, Tile{
				Kind: "plugin",
				Name: filepath.Base(rel),
				Rel:  rel,
				Abs:  filepath.Join(m.KksRoot, filepath.FromSlash(rel)),
				Badge: filepath.Dir(rel),
			})
		}
		if more > 0 {
			secB.More = more
			secB.MoreLabel = "还有 " + strconv.Itoa(more) + " 个插件"
			secB.Tiles = append(secB.Tiles, Tile{Kind: "more", Name: secB.MoreLabel})
		}
	}

	panel.Sections = []Section{secA, secB}

	// --- 信息栏 ---
	if guid == "" {
		panel.Focus.Info = "无法识别 GUID，永久禁止自动隔离"
	} else {
		cards := len(m.GlobalDepMap[guid])
		plugins := len(m.PluginDepMap[guid])
		panel.Focus.Info = "GUID: " + guid + " · 被 " + strconv.Itoa(cards+plugins) +
			" 个文件占用（卡片 " + strconv.Itoa(cards) + " / 插件 " + strconv.Itoa(plugins) + "）"
	}
	return *panel
}

// invisibleFocusInfo 生成隐藏项的信息栏文案。
func (m *Manager) invisibleFocusInfo(abs string) string {
	if abs == "" {
		return "未选择条目"
	}
	return filepath.ToSlash(abs)
}

// panelFocusInfo 生成焦点信息栏文案。
func (m *Manager) panelFocusInfo(mode, dir, focusAbs, focusKind string) string {
	if focusKind == "folder" || focusAbs == "" {
		// 焦点是文件夹 → 展示该目录的统计
		folders := m.countSubDirsCached(dir)
		cards := m.countCardsCached(dir)
		size := m.dirSizeCached(dir)
		return strconv.Itoa(cards) + " 张卡片 · " + strconv.Itoa(folders) + " 个子文件夹 · " + core.ParseSize(size)
	}
	return filepath.Base(focusAbs)
}

// resolveRelDir 把「相对类型根的路径」解析为绝对目录。
func (m *Manager) resolveRelDir(mode, side, rel string) string {
	var root string
	if mode == ModeZipmod {
		if side == "left" {
			root = m.ModsDir()
		} else {
			root = m.DelZipmodDir()
		}
	} else {
		kind, ok := KindByMode(mode)
		if !ok {
			return ""
		}
		if side == "left" {
			root = m.SourceDir(kind)
		} else {
			root = m.DelDir(kind)
		}
	}
	if rel == "" {
		return root
	}
	return filepath.Join(root, filepath.FromSlash(rel))
}

// relOf 返回路径相对类型根的 POSIX 路径。
func (m *Manager) relOf(abs string) string {
	for _, kind := range ResourceKinds {
		if r, err := filepath.Rel(m.SourceDir(kind), abs); err == nil && !strings.HasPrefix(r, "..") {
			return filepath.ToSlash(r)
		}
		if r, err := filepath.Rel(m.DelDir(kind), abs); err == nil && !strings.HasPrefix(r, "..") {
			return filepath.ToSlash(r)
		}
	}
	return filepath.Base(abs)
}

// thumbRequest 请求缩略图（只查缓存，未命中返回 pending）。
//
// 这是红线 N1 的落地点：UI 请求路径绝不触发 PNG 解码。
func (m *Manager) thumbRequest(abs string, priority bool) (key string, dataURI string, pending bool) {
	fp := m.fingerprintOf(abs)
	key = preview.ThumbKey(abs, fp)
	if m.thumbs == nil {
		return key, "", true
	}
	uri, pend := m.thumbs.Request(abs, fp, priority)
	return key, uri, pend
}

// fingerprintOf 取文件指纹。
func (m *Manager) fingerprintOf(abs string) preview.Fingerprint {
	info, err := os.Stat(abs)
	if err != nil {
		return preview.Fingerprint{}
	}
	return preview.Fingerprint{
		Size:    info.Size(),
		ModTime: info.ModTime().UnixNano(),
	}
}

// samePath 比较两个路径是否指向同一文件（不区分大小写，Windows 语义）。
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
