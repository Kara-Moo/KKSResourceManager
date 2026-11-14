package main

// panel_test.go —— 右栏面板的数据层测试（规格 §11.1）
//
// 覆盖：tileCount 边界、区域 Visible 规则、排序、封面、占用数、零引用空态。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kksresourcemanager/internal/core"
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
// TestPanelFocusSelected 焦点卡片在单图布局里被标记为 Selected。
//
// 注意：v2.2 FIX-02 改变了契约 —— 卡片焦点不再返回九宫格，
// 而是单图布局（Sections 长度 1、Layout=single）。
// 本测试原先断言「九宫格里有一个 Selected 格」，那是旧行为。
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

	if len(panel.Sections) != 1 {
		t.Fatalf("卡片焦点应只有 1 个区域，实际 %d", len(panel.Sections))
	}
	sec := panel.Sections[0]
	if sec.Layout != "single" {
		t.Errorf("布局应为 single，实际 %q", sec.Layout)
	}
	if len(sec.Tiles) != 1 {
		t.Fatalf("应只有 1 个格子，实际 %d", len(sec.Tiles))
	}
	tile := sec.Tiles[0]
	if !tile.Selected {
		t.Error("焦点卡片应标记为 Selected")
	}
	if filepath.Base(tile.Abs) != "target.png" {
		t.Errorf("Selected 格应指向焦点卡片，实际 %s", tile.Abs)
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

// TestPanelCardFocusSingle 卡片焦点必须出单张大图，而不是九宫格（FIX-02）。
func TestPanelCardFocusSingle(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	card := env.femaleCard("c.png")
	if err := os.MkdirAll(filepath.Dir(card), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(card, tinyPNG, 0644); err != nil {
		t.Fatal(err)
	}

	panel, err := m.BuildPanel("left", "female_card", "", card)
	if err != nil {
		t.Fatalf("BuildPanel 失败: %v", err)
	}

	if panel.Focus.Kind != "card" {
		t.Errorf("焦点类型应为 card，实际 %q", panel.Focus.Kind)
	}
	if len(panel.Sections) != 1 {
		t.Fatalf("卡片焦点应只有 1 个区域，实际 %d", len(panel.Sections))
	}
	if panel.Sections[0].Layout != "single" {
		t.Errorf("布局应为 single，实际 %q", panel.Sections[0].Layout)
	}
	if len(panel.Sections[0].Tiles) != 1 {
		t.Errorf("应只有 1 个格子，实际 %d", len(panel.Sections[0].Tiles))
	}
	if panel.Sections[0].More != 0 {
		t.Errorf("单卡不应出现「还有」格，实际 more=%d", panel.Sections[0].More)
	}
}

// TestPanelCardInfoNotDuplicate 信息栏不得重复文件名（FIX-08）。
func TestPanelCardInfoNotDuplicate(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	card := env.femaleCard("87642255_p18.png")
	if err := os.MkdirAll(filepath.Dir(card), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(card, tinyPNG, 0644); err != nil {
		t.Fatal(err)
	}

	panel, err := m.BuildPanel("left", "female_card", "", card)
	if err != nil {
		t.Fatalf("BuildPanel 失败: %v", err)
	}

	if panel.Focus.Info == panel.Focus.Name {
		t.Errorf("信息栏与标题重复：%q", panel.Focus.Info)
	}
	if strings.Contains(panel.Focus.Info, "87642255_p18.png") {
		t.Errorf("信息栏不应包含文件名，实际 %q", panel.Focus.Info)
	}
	// v2.4：不再展示 zipmod 依赖数，信息栏只保留大小
	if !strings.Contains(panel.Focus.Info, "B") {
		t.Errorf("信息栏应含大小，实际 %q", panel.Focus.Info)
	}
	if strings.Contains(panel.Focus.Info, "依赖") {
		t.Errorf("信息栏不应再含依赖数，实际 %q", panel.Focus.Info)
	}
}

// TestPanelFolderFocusStillGrid 文件夹焦点仍是九宫格（不得被 FIX-02 误伤）。
func TestPanelFolderFocusStillGrid(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	for i := 0; i < 3; i++ {
		writeFile(t, env.femaleCard(fmt.Sprintf("f%d.png", i)), "x")
	}

	panel, err := m.BuildPanel("left", "female_card", "", m.SourceDir(ResourceKinds[0]))
	if err != nil {
		t.Fatalf("BuildPanel 失败: %v", err)
	}
	if panel.Focus.Kind != "folder" {
		t.Errorf("焦点类型应为 folder，实际 %q", panel.Focus.Kind)
	}
	for _, sec := range panel.Sections {
		if sec.Layout == "single" {
			t.Error("文件夹焦点不应使用 single 布局")
		}
	}
	if len(panel.Sections) != 2 {
		t.Errorf("文件夹焦点应有 A/B 两区，实际 %d", len(panel.Sections))
	}
}

// TestBuildTreeFiltersExtensions 列表不得混入非资源文件（FIX-06）。
func TestBuildTreeFiltersExtensions(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager
	kind := ResourceKinds[0]

	// 根目录：1 PNG + 2 JPG（合法卡片）+ 1 个 PPK（无关文件）
	// 子目录：1 PNG + 1 JPG
	writeFile(t, env.femaleCard("real.png"), "png")
	writeFile(t, env.femaleCard("Cache_4379b653c5a27864.jpg"), "jpg")
	writeFile(t, env.femaleCard("Cache_4175e3ce77527a70.jpeg"), "jpeg")
	writeFile(t, env.femaleCard("Halo.ppk"), "ppk")
	writeFile(t, env.femaleCard("sub/inside.png"), "png")
	writeFile(t, env.femaleCard("sub/inside.jpg"), "jpg")

	nodes := m.buildTree(m.SourceDir(kind), false, kind)

	files := 0
	for _, n := range nodes {
		if n.Type == "folder" {
			continue
		}
		files++
		if !core.IsCardFile(n.Name) {
			t.Errorf("列表混入了非卡片文件：%s（%s）", n.Name, n.Type)
		}
		if strings.EqualFold(filepath.Ext(n.Name), ".ppk") {
			t.Errorf("PPK 不应出现在列表：%s", n.Name)
		}
	}
	if files != 5 {
		t.Errorf("应有 5 张卡片（3 根目录 + 2 子目录），实际 %d 个文件节点", files)
	}

	// 面板的卡片数应与列表根目录行数一致（含 JPG）
	panel, err := m.BuildPanel("left", kind.Mode, "", m.SourceDir(kind))
	if err != nil {
		t.Fatalf("BuildPanel 失败: %v", err)
	}
	for _, sec := range panel.Sections {
		if sec.ID == "cards" && sec.Total != 3 {
			t.Errorf("根目录面板卡片数应为 3（含 2 张 JPG），实际 %d", sec.Total)
		}
	}
}

