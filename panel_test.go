package main

// panel_test.go —— 右栏面板的数据层测试（规格 §11.1）
//
// 覆盖：tileCount 边界、区域 Visible 规则、排序、封面、占用数、零引用空态。

import (
	"os"
	"path/filepath"
	"testing"

	"kksresourcemanager/internal/index"
)

// TestTileCount 验证九宫格的数量规则。
//
//	n <= 9  : 全显示，无 "还有" 格
//	n >  9 : 显示 8 个 + 1 个 "还有 (n-8)" 格
func TestTileCount(t *testing.T) {
	cases := []struct {
		n         int
		wantTiles int
		wantMore  int
	}{
		{0, 0, 0},
		{1, 1, 0},
		{5, 5, 0},
		{8, 8, 0},
		{9, 9, 0},
		{10, 8, 2},
		{23, 8, 15},
		{115, 8, 107},
	}
	for _, c := range cases {
		tiles, more := tileCount(c.n)
		if tiles != c.wantTiles || more != c.wantMore {
			t.Errorf("tileCount(%d) = (%d, %d)，期望 (%d, %d)", c.n, tiles, more, c.wantTiles, c.wantMore)
		}
	}
}

// TestPanelFoldersOnly 焦点为纯文件夹目录时，区域 B 必须隐藏（§11.1）。
func TestPanelFoldersOnly(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	// 一个只有子文件夹、没有卡片的目录
	for _, name := range []string{"d1", "d2"} {
		writeFile(t, filepath.Join(env.femaleCard(name), "sub", "x.png"), "x")
	}

	panel, err := m.BuildPanel("left", "female_card", "", "")
	if err != nil {
		t.Fatalf("构建面板失败: %v", err)
	}
	if len(panel.Sections) != 2 {
		t.Fatalf("应有 2 个区域，实际 %d", len(panel.Sections))
	}
	secA, secB := panel.Sections[0], panel.Sections[1]

	if !secA.Visible {
		t.Error("有子文件夹时区域 A 应可见")
	}
	if secA.Total != 2 {
		t.Errorf("区域 A 应有 2 个子文件夹，实际 %d", secA.Total)
	}
	if secB.Visible {
		t.Error("无卡片时区域 B 必须整块隐藏（§11.1）")
	}
}

// TestPanelCardsOnly 焦点为无子文件夹但有卡片的目录时，区域 A 必须隐藏。
//
// 这是用户点名的场景：「无子文件夹的应该只有一个单独的右栏里的一行」。
func TestPanelCardsOnly(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	for i := 0; i < 5; i++ {
		writeFile(t, env.femaleCard("card"+string(rune('a'+i))+".png"), "x")
	}

	panel, err := m.BuildPanel("left", "female_card", "", "")
	if err != nil {
		t.Fatalf("构建面板失败: %v", err)
	}
	secA, secB := panel.Sections[0], panel.Sections[1]

	if secA.Visible {
		t.Error("无子文件夹时区域 A 必须整块隐藏，不留空白")
	}
	if !secB.Visible {
		t.Fatal("有卡片时区域 B 应可见")
	}
	if secB.Total != 5 {
		t.Errorf("区域 B 应有 5 张卡片，实际 %d", secB.Total)
	}
	// 5 个格子 = 2 行，不应有 "还有" 格
	if len(secB.Tiles) != 5 {
		t.Errorf("应渲染 5 个格子，实际 %d", len(secB.Tiles))
	}
	if secB.More != 0 {
		t.Errorf("5 张卡片不应有「还有」格，实际 %d", secB.More)
	}
}

// TestPanelEmptyDir 两者都无时，两区都隐藏。
func TestPanelEmptyDir(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	// 空的子目录
	writeFile(t, filepath.Join(env.femaleCard("empty"), "placeholder"), "")

	panel, err := m.BuildPanel("left", "female_card", "empty", "")
	if err != nil {
		t.Fatalf("构建面板失败: %v", err)
	}
	// empty 目录本身是空的（只有 placeholder 文件但不是 png）
	for _, sec := range panel.Sections {
		if sec.Visible {
			t.Errorf("空目录的区域 %s 应隐藏", sec.ID)
		}
	}
}

// TestPanelCardBadgeEmpty 卡片格必须不显示"N 个文件"（§11.1）。
func TestPanelCardBadgeEmpty(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	writeFile(t, env.femaleCard("card1.png"), "x")

	panel, err := m.BuildPanel("left", "female_card", "", "")
	if err != nil {
		t.Fatalf("构建面板失败: %v", err)
	}
	secB := panel.Sections[1]
	for _, tile := range secB.Tiles {
		if tile.Kind == "card" && tile.Badge != "" {
			t.Errorf("卡片格的 badge 必须为空串，实际 %q", tile.Badge)
		}
	}
}

// TestPanelFolderBadgeHasCount 文件夹格必须显示文件数。
func TestPanelFolderBadgeHasCount(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	writeFile(t, filepath.Join(env.femaleCard("d1"), "a.png"), "x")
	writeFile(t, filepath.Join(env.femaleCard("d1"), "b.png"), "x")

	panel, err := m.BuildPanel("left", "female_card", "", "")
	if err != nil {
		t.Fatalf("构建面板失败: %v", err)
	}
	secA := panel.Sections[0]
	found := false
	for _, tile := range secA.Tiles {
		if tile.Kind == "folder" {
			found = true
			if tile.Badge == "" {
				t.Error("文件夹格的 badge 应显示文件数")
			}
		}
	}
	if !found {
		t.Error("应至少有一个文件夹格")
	}
}

// TestPanelMoreTile 超过 9 个时应出现"还有"格。
func TestPanelMoreTile(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	for i := 0; i < 12; i++ {
		writeFile(t, env.femaleCard("card"+string(rune('a'+i))+".png"), "x")
	}

	panel, err := m.BuildPanel("left", "female_card", "", "")
	if err != nil {
		t.Fatalf("构建面板失败: %v", err)
	}
	secB := panel.Sections[1]
	if secB.More != 4 {
		t.Errorf("12 张卡片应有 4 个「还有」，实际 %d", secB.More)
	}
	if len(secB.Tiles) != 9 {
		t.Errorf("应渲染 8+1=9 个格子，实际 %d", len(secB.Tiles))
	}
	last := secB.Tiles[len(secB.Tiles)-1]
	if last.Kind != "more" {
		t.Errorf("最后一格应为 more，实际 %s", last.Kind)
	}
	if secB.MoreLabel == "" {
		t.Error("应显示「还有 N 张角色卡」文案")
	}
}

// TestPanelFocusSelected 焦点是卡片时，该卡格应标记 Selected。
func TestPanelFocusSelected(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	card := env.femaleCard("target.png")
	writeFile(t, card, "x")
	writeFile(t, env.femaleCard("other.png"), "x")

	panel, err := m.BuildPanel("left", "female_card", "", card)
	if err != nil {
		t.Fatalf("构建面板失败: %v", err)
	}
	secB := panel.Sections[1]
	selected := 0
	for _, tile := range secB.Tiles {
		if tile.Selected {
			selected++
			if filepath.Base(tile.Abs) != "target.png" {
				t.Errorf("Selected 标记应落在焦点卡片上，实际 %s", tile.Abs)
			}
		}
	}
	if selected != 1 {
		t.Errorf("应有且仅有 1 个 Selected 格，实际 %d", selected)
	}
}

// TestPanelZipmodZeroRefs 零引用 zipmod 必须显示空态而非"无法预览"（红线 N3）。
func TestPanelZipmodZeroRefs(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	modPath := filepath.Join(m.ModsDir(), "lonely.zipmod")
	writeFile(t, modPath, "mod")
	m.ZipmodDB["lonely.guid"] = modPath
	m.guidOfPath[modPath] = "lonely.guid"

	panel, err := m.BuildPanel("left", "zipmod", "", modPath)
	if err != nil {
		t.Fatalf("构建面板失败: %v", err)
	}
	secA, secB := panel.Sections[0], panel.Sections[1]

	if !secA.Visible {
		t.Error("零引用时区域 A 仍应可见（0 引用是关键判断信息）")
	}
	if secA.Empty == "" {
		t.Error("零引用时应显示空态文案")
	}
	if !secB.Visible {
		t.Error("零引用时区域 B 仍应可见")
	}
	if secB.Empty == "" {
		t.Error("零插件引用时应显示空态文案")
	}
	// 全文不应出现"无法预览"
	for _, sec := range panel.Sections {
		for _, tile := range sec.Tiles {
			if tile.Name == "无法预览" {
				t.Error("不应出现「无法预览」文案")
			}
		}
	}
}

// TestPanelZipmodWithRefs zipmod 应列出引用它的卡片。
func TestPanelZipmodWithRefs(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	modPath := filepath.Join(m.ModsDir(), "used.zipmod")
	writeFile(t, modPath, "mod")
	m.ZipmodDB["used.guid"] = modPath

	card := env.femaleCard("card1.png")
	writeFile(t, card, "x")
	m.GlobalDepMap["used.guid"] = []string{card}

	panel, err := m.BuildPanel("left", "zipmod", "", modPath)
	if err != nil {
		t.Fatalf("构建面板失败: %v", err)
	}
	secA := panel.Sections[0]
	if secA.Total != 1 {
		t.Errorf("区域 A 应有 1 张引用卡片，实际 %d", secA.Total)
	}
	if len(secA.Tiles) != 1 {
		t.Fatalf("应渲染 1 个格子，实际 %d", len(secA.Tiles))
	}
	if filepath.Base(secA.Tiles[0].Abs) != "card1.png" {
		t.Errorf("格子应指向引用卡片，实际 %s", secA.Tiles[0].Abs)
	}
}

// TestPanelZipmodPluginSection zipmod 的插件区域应为 list 布局。
func TestPanelZipmodPluginSection(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	modPath := filepath.Join(m.ModsDir(), "plug.zipmod")
	writeFile(t, modPath, "mod")
	m.ZipmodDB["plug.guid"] = modPath
	m.PluginDepMap["plug.guid"] = []string{"BepInEx/plugins/Foo.dll"}

	panel, err := m.BuildPanel("left", "zipmod", "", modPath)
	if err != nil {
		t.Fatalf("构建面板失败: %v", err)
	}
	secB := panel.Sections[1]
	if secB.ID != "plugins" {
		t.Errorf("区域 B 应为 plugins，实际 %s", secB.ID)
	}
	if secB.Layout != "list" {
		t.Errorf("插件区域应为 list 布局，实际 %s", secB.Layout)
	}
	if len(secB.Tiles) != 1 {
		t.Errorf("应渲染 1 个插件格，实际 %d", len(secB.Tiles))
	}
	if secB.Tiles[0].Name != "Foo.dll" {
		t.Errorf("插件名应为 Foo.dll，实际 %s", secB.Tiles[0].Name)
	}
}

// TestPanelZipmodInfoBar 信息栏应显示占用数明细。
func TestPanelZipmodInfoBar(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	modPath := filepath.Join(m.ModsDir(), "info.zipmod")
	writeFile(t, modPath, "mod")
	m.ZipmodDB["info.guid"] = modPath
	m.GlobalDepMap["info.guid"] = []string{"a.png", "b.png"}
	m.PluginDepMap["info.guid"] = []string{"BepInEx/plugins/X.dll"}

	panel, err := m.BuildPanel("left", "zipmod", "", modPath)
	if err != nil {
		t.Fatalf("构建面板失败: %v", err)
	}
	info := panel.Focus.Info
	if info == "" {
		t.Fatal("信息栏不应为空")
	}
	// 应包含 GUID 与占用数（卡片 2 / 插件 1）
	if !contains(info, "info.guid") {
		t.Errorf("信息栏应含 GUID，实际 %q", info)
	}
	if !contains(info, "3") {
		t.Errorf("信息栏应含总占用数 3，实际 %q", info)
	}
}

// TestPanelNaturalSort 面板内的文件应按自然序排列。
func TestPanelNaturalSort(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	// 故意乱序创建
	for _, name := range []string{"22.7.10.png", "22.7.9.png", "22.7.1.png"} {
		writeFile(t, env.femaleCard(name), "x")
	}

	panel, err := m.BuildPanel("left", "female_card", "", "")
	if err != nil {
		t.Fatalf("构建面板失败: %v", err)
	}
	tiles := panel.Sections[1].Tiles
	if len(tiles) < 3 {
		t.Fatalf("应有至少 3 个格子，实际 %d", len(tiles))
	}
	want := []string{"22.7.1.png", "22.7.9.png", "22.7.10.png"}
	for i := range want {
		if tiles[i].Name != want[i] {
			t.Errorf("第 %d 格应为 %s，实际 %s", i, want[i], tiles[i].Name)
		}
	}
}

// TestPanelCoverSelection 子文件夹格应带封面缩略图。
func TestPanelCoverSelection(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	// 子文件夹内直接有 png → 一级即可命中封面
	writeFile(t, filepath.Join(env.femaleCard("d1"), "cover.png"), "x")

	panel, err := m.BuildPanel("left", "female_card", "", "")
	if err != nil {
		t.Fatalf("构建面板失败: %v", err)
	}
	tiles := panel.Sections[0].Tiles
	if len(tiles) == 0 {
		t.Fatal("应有文件夹格")
	}
	// 有封面时 thumbKey 非空；无封面时也应有 key（前端渲染占位）
	if tiles[0].ThumbKey == "" {
		t.Error("文件夹格应有 thumbKey")
	}
}

// contains 是 strings.Contains 的简写（测试用）。
func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

// indexOf 返回子串位置。
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

var _ = os.Stat

// TestModsTreeRefCount 验证 zipmod 列表的占用数（O(1) 内存查询）。
func TestModsTreeRefCount(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	// 两个 zipmod
	modA := filepath.Join(m.ModsDir(), "a.zipmod")
	modB := filepath.Join(m.ModsDir(), "b.zipmod")
	writeFile(t, modA, "mod")
	writeFile(t, modB, "mod")

	m.ZipmodDB["guid.a"] = modA
	m.ZipmodDB["guid.b"] = modB
	// 索引里有 GUID（真实扫描后的状态）
	m.index = index.NewIndexData(m.KksRoot)
	m.index.Zipmods[modA] = &index.ZipmodEntry{GUID: "guid.a"}
	m.index.Zipmods[modB] = &index.ZipmodEntry{GUID: "guid.b"}
	m.GlobalDepMap["guid.a"] = []string{"c1.png", "c2.png", "c3.png"}
	m.PluginDepMap["guid.a"] = []string{"BepInEx/plugins/X.dll"}
	m.GlobalDepMap["guid.b"] = nil

	nodes := m.buildModsTree(m.ModsDir())
	if len(nodes) != 2 {
		t.Fatalf("应有 2 个 zipmod，实际 %d", len(nodes))
	}
	byName := make(map[string]TreeNode, 2)
	for _, n := range nodes {
		byName[n.Name] = n
	}

	a := byName["a.zipmod"]
	if a.RefCards != 3 {
		t.Errorf("a.zipmod 应有 3 个卡片引用，实际 %d", a.RefCards)
	}
	if a.RefPlugins != 1 {
		t.Errorf("a.zipmod 应有 1 个插件引用，实际 %d", a.RefPlugins)
	}
	if a.RefCount != 4 {
		t.Errorf("a.zipmod 总占用应为 4，实际 %d", a.RefCount)
	}
	if a.GUID != "guid.a" {
		t.Errorf("a.zipmod 的 GUID 应为 guid.a，实际 %q", a.GUID)
	}

	b := byName["b.zipmod"]
	if b.RefCount != 0 {
		t.Errorf("b.zipmod 占用应为 0，实际 %d", b.RefCount)
	}
}

// TestZipmodDetailOf 验证占用详情包含卡片与插件明细。
func TestZipmodDetailOf(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	modPath := filepath.Join(m.ModsDir(), "detail.zipmod")
	writeFile(t, modPath, "mod")
	m.ZipmodDB["detail.guid"] = modPath
	m.GlobalDepMap["detail.guid"] = []string{"a.png", "b.png"}
	m.PluginDepMap["detail.guid"] = []string{"BepInEx/plugins/Foo.dll"}

	d := m.ZipmodDetailOf("detail.guid")
	if d.GUID != "detail.guid" {
		t.Errorf("GUID 应为 detail.guid，实际 %q", d.GUID)
	}
	if d.RefCards != 2 {
		t.Errorf("应有 2 个卡片引用，实际 %d", d.RefCards)
	}
	if d.RefPlugins != 1 {
		t.Errorf("应有 1 个插件引用，实际 %d", d.RefPlugins)
	}
	if len(d.Cards) != 2 {
		t.Errorf("cards 列表应有 2 项，实际 %d", len(d.Cards))
	}
	if len(d.Plugins) != 1 || d.Plugins[0].Name != "Foo.dll" {
		t.Errorf("plugins 列表应含 Foo.dll，实际 %v", d.Plugins)
	}
}
