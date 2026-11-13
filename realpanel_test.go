package main

// realpanel_test.go —— 真实目录上的右栏面板验证
//
// 需要环境变量 KKS_REAL_ROOT 指定真实 KKS 根目录。
// 只读游戏目录。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kksresourcemanager/internal/index"
)

// TestRealModsTreeRefCount 在真实目录上验证 zipmod 占用数。
//
// 这是 §11.3 的关键验收：
//   - zipmod 模式左列第三列是红色数字
//   - 数字 = 引用卡片数 + 插件数
//   - 4531 行列表滚动流畅（无逐行 os.Stat）
func TestRealModsTreeRefCount(t *testing.T) {
	root := strings.TrimSpace(os.Getenv("KKS_REAL_ROOT"))
	if root == "" {
		t.Skip("未设置 KKS_REAL_ROOT，跳过")
	}
	m, err := NewManager(root)
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}

	// 加载索引
	store, err := index.NewStore(m.DelCardsRoot)
	if err != nil {
		t.Fatalf("创建 Store 失败: %v", err)
	}
	old, _ := store.Load(m.KksRoot)
	if old == nil {
		t.Skip("无索引，请先运行 TestRealIncrementalScan")
	}

	scanner := &index.Scanner{
		Root:          m.KksRoot,
		ModsDir:       m.ModsDir(),
		ResourceRoots: m.resourceRoots(),
		PluginRoots:   m.pluginRoots(),
		ParseZipmod: func(p string) (string, []string, error) {
			mf, e := ReadModManifest(p)
			return mf.GUID, nil, e
		},
		ParseCard:   func(p string) ([]string, error) { return ParseCardGUIDs(p) },
		MatchPlugin: func(p string, gs []string) ([]string, error) { return nil, nil },
	}
	idx, derived, _, err := scanner.Scan(nil, old, nil)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	m.applyIndex(idx, derived)

	// --- 构建 zipmod 列表，测耗时 ---
	start := time.Now()
	nodes := m.buildModsTree(m.ModsDir())
	elapsed := time.Since(start)

	t.Logf("构建 %d 个 zipmod 列表耗时 %v", len(nodes), elapsed)
	if elapsed > 500*time.Millisecond {
		t.Errorf("构建 zipmod 列表耗时 %v，过慢（4531 行应 < 500ms）", elapsed)
	}

	// --- 抽查占用数是否与索引一致 ---
	withRef := 0
	for _, n := range nodes {
		if n.RefCount > 0 {
			withRef++
		}
	}
	t.Logf("%d 个 zipmod 中，%d 个有引用", len(nodes), withRef)

	// 抽查 3 个：验证 RefCount == RefCards + RefPlugins 且与索引一致
	checked := 0
	for _, n := range nodes {
		if checked >= 3 {
			break
		}
		if n.GUID == "" {
			continue
		}
		wantCards := len(m.GlobalDepMap[n.GUID])
		wantPlugins := len(m.PluginDepMap[n.GUID])
		if n.RefCards != wantCards || n.RefPlugins != wantPlugins {
			t.Errorf("%s 占用数不符：列表 %d+%d，索引 %d+%d",
				n.Name, n.RefCards, n.RefPlugins, wantCards, wantPlugins)
		}
		if n.RefCount != n.RefCards+n.RefPlugins {
			t.Errorf("%s 总占用数应等于卡片+插件", n.Name)
		}
		checked++
	}
	if checked == 0 {
		t.Log("没有可校验的条目（GUID 解析失败）")
	} else {
		t.Logf("抽查 %d 个 zipmod，占用数与索引一致", checked)
	}
}

// TestRealBuildPanel 在真实目录上验证面板构建耗时。
func TestRealBuildPanel(t *testing.T) {
	root := strings.TrimSpace(os.Getenv("KKS_REAL_ROOT"))
	if root == "" {
		t.Skip("未设置 KKS_REAL_ROOT，跳过")
	}
	m, err := NewManager(root)
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}

	store, _ := index.NewStore(m.DelCardsRoot)
	old, _ := store.Load(m.KksRoot)
	if old == nil {
		t.Skip("无索引")
	}
	scanner := &index.Scanner{
		Root: m.KksRoot, ModsDir: m.ModsDir(),
		ResourceRoots: m.resourceRoots(), PluginRoots: m.pluginRoots(),
		ParseZipmod: func(p string) (string, []string, error) {
			mf, e := ReadModManifest(p)
			return mf.GUID, nil, e
		},
		ParseCard:   func(p string) ([]string, error) { return ParseCardGUIDs(p) },
		MatchPlugin: func(p string, gs []string) ([]string, error) { return nil, nil },
	}
	idx, derived, _, _ := scanner.Scan(nil, old, nil)
	m.applyIndex(idx, derived)

	// 根目录（16150 文件的 female/）是最坏情况
	start := time.Now()
	panel, err := m.BuildPanel("left", "female_card", "", "")
	if err != nil {
		t.Fatalf("构建面板失败: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("根目录面板构建耗时 %v", elapsed)

	if elapsed > 80*time.Millisecond {
		t.Errorf("面板构建耗时 %v，超出 80ms 目标（§11.7）", elapsed)
	}
	if len(panel.Sections) != 2 {
		t.Errorf("应有 2 个区域，实际 %d", len(panel.Sections))
	}
	for _, sec := range panel.Sections {
		t.Logf("区域 %s: total=%d visible=%v tiles=%d", sec.ID, sec.Total, sec.Visible, len(sec.Tiles))
	}
}

// TestRealZipmodFullList 真实目录下引用最多的 zipmod 必须全量返回（FIX-03）。
func TestRealZipmodFullList(t *testing.T) {
	root := strings.TrimSpace(os.Getenv("KKS_REAL_ROOT"))
	if root == "" {
		t.Skip("未设置 KKS_REAL_ROOT，跳过")
	}
	m, err := NewManager(root)
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}
	store, _ := index.NewStore(m.DelCardsRoot)
	old, _ := store.Load(m.KksRoot)
	if old == nil {
		t.Skip("无索引")
	}
	m.applyIndex(old, index.Derive(old))

	// 找出引用最多的 zipmod
	var topGuid string
	topN := 0
	for guid, refs := range m.GlobalDepMap {
		if len(refs) > topN {
			topN = len(refs)
			topGuid = guid
		}
	}
	if topGuid == "" {
		t.Skip("没有带引用的 zipmod")
	}
	modPath := m.ZipmodDB[topGuid]
	if modPath == "" {
		t.Skip("未找到 zipmod 路径")
	}
	t.Logf("引用最多的 zipmod：%s（GUID %s，%d 个引用）",
		filepath.Base(modPath), topGuid, topN)

	start := time.Now()
	panel, err := m.BuildPanel("left", "zipmod", "", modPath)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("BuildPanel 失败: %v", err)
	}

	secA := panel.Sections[0]
	t.Logf("构建耗时 %v；区域 A total=%d tiles=%d more=%d lazy=%v",
		elapsed, secA.Total, len(secA.Tiles), secA.More, secA.Lazy)

	if secA.Total != topN {
		t.Errorf("total 应等于引用数 %d，实际 %d", topN, secA.Total)
	}
	if len(secA.Tiles) != topN {
		t.Errorf("应全量返回 %d 个格子，实际 %d", topN, len(secA.Tiles))
	}
	if secA.More != 0 {
		t.Errorf("不应出现「还有」格，实际 more=%d", secA.More)
	}
	if !secA.Lazy {
		t.Error("应标记 Lazy=true")
	}
	// 未预请求：所有格子都应是 pending 且无 thumb
	for _, tile := range secA.Tiles {
		if tile.Thumb != "" {
			t.Error("不应预生成缩略图（应由前端懒加载）")
			break
		}
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("全量返回不应慢：耗时 %v（目标 <200ms）", elapsed)
	}
}
