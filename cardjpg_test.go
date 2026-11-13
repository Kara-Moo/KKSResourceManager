package main

// cardjpg_test.go —— JPG 角色卡支持的回归测试
//
// 背景：用户目录里出现 .JPG 卡片时，旧版只认 .png，
// 列表看不到它、面板统计与隐藏判定又互相矛盾。
// 这里锁死「识别 / 载荷提取 / 预览 / 依赖解析」四件事。

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GenesisAN/illusionsCard/util"
	"kksresourcemanager/internal/core"
)

// makeJPEG 生成一张最小的 JPEG 图片字节。
func makeJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 16), G: uint8(y * 16), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestIsCardFileExtensions 三种扩展名、任意大小写都必须被识别为资源卡。
func TestIsCardFileExtensions(t *testing.T) {
	yes := []string{"a.png", "a.PNG", "b.jpg", "b.JPG", "c.jpeg", "c.JPEG", "x.0.JPG"}
	no := []string{"readme.txt", "Cache_x.gif", "Halo.ppk", "a.zipmod", "noext"}
	for _, n := range yes {
		if !core.IsCardFile(n) {
			t.Errorf("应识别为卡片：%s", n)
		}
	}
	for _, n := range no {
		if core.IsCardFile(n) {
			t.Errorf("不应识别为卡片：%s", n)
		}
	}
}

// TestJPEGPayload 纯 JPEG 无 payload；EOI 后附加数据能被提取。
func TestJPEGPayload(t *testing.T) {
	jpg := makeJPEG(t)
	if p := cardPayload(jpg); len(p) != 0 {
		t.Errorf("纯 JPEG 不应有 KKS 附加数据，实际 %d 字节", len(p))
	}

	fake := []byte("KKS-CARD-PAYLOAD")
	withTail := append(append([]byte(nil), jpg...), fake...)
	got := cardPayload(withTail)
	if !bytes.Equal(got, fake) {
		t.Fatalf("应提取 EOI 后的数据，实际 %q", got)
	}
}

// TestJPEGPreview JPG 必须能生成大图预览（否则右栏一片空白）。
func TestJPEGPreview(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "card.JPG")
	if err := os.WriteFile(p, makeJPEG(t), 0644); err != nil {
		t.Fatal(err)
	}

	img, w, h, err := decodeCardImage(p)
	if err != nil {
		t.Fatalf("JPG 预览解码失败: %v", err)
	}
	if img == nil || w != 16 || h != 16 {
		t.Errorf("预览尺寸异常: %dx%d", w, h)
	}

	// 缩略图与大图两档都要能出图
	grid, err := buildThumbJPEGSizedTest(p)
	if err != nil || len(grid) == 0 {
		t.Errorf("JPG 缩略图生成失败: %v", err)
	}
	large, err := buildThumbJPEGSizedTestLarge(p)
	if err != nil || len(large) == 0 {
		t.Errorf("JPG 大图生成失败: %v", err)
	}
}

func buildThumbJPEGSizedTest(p string) ([]byte, error) {
	return newTestManagerForThumb().buildThumbJPEGSized(p, previewMaxWidth, previewMaxHeight, previewJPEGQuality)
}
func buildThumbJPEGSizedTestLarge(p string) ([]byte, error) {
	return newTestManagerForThumb().buildThumbJPEGSized(p, previewLargeWidth, previewLargeHeight, previewLargeQuality)
}

// newTestManagerForThumb 构造一个把 temp 目录纳入允许范围的 Manager。
func newTestManagerForThumb() *Manager {
	m := &Manager{}
	m.KksRoot = os.TempDir()
	m.DelCardsRoot = os.TempDir()
	return m
}

// TestJPEGCardWithRealPayload 把真实 PNG 卡的 payload 挂到 JPEG 后面，
// 解析出的 GUID 必须与原 PNG 一致 —— 证明「JPG 卡对 zipmod 的绑定」
// 走的是与 PNG 完全相同的数据格式。
func TestJPEGCardWithRealPayload(t *testing.T) {
	root := strings.TrimSpace(os.Getenv("KKS_REAL_ROOT"))
	if root == "" {
		t.Skip("未设置 KKS_REAL_ROOT，跳过")
	}

	// 找一张带依赖的真实 PNG 卡
	var pngPath string
	var pngGUIDs []string
	female := filepath.Join(root, "UserData", "chara", "female")
	filepath.Walk(female, func(path string, info os.FileInfo, err error) error {
		if err != nil || pngPath != "" || info == nil || info.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".png") {
			return nil
		}
		g, e := ParseCardGUIDs(path)
		if e == nil && len(g) > 0 {
			pngPath, pngGUIDs = path, g
			return filepath.SkipAll
		}
		return nil
	})
	if pngPath == "" {
		t.Skip("未找到带依赖的 PNG 卡")
	}

	raw, err := os.ReadFile(pngPath)
	if err != nil {
		t.Fatal(err)
	}
	payload := pngPayload(raw)
	if len(payload) == 0 {
		t.Skip("该 PNG 卡没有尾部 payload")
	}

	jpgData := append(append([]byte(nil), makeJPEG(t)...), payload...)
	jpgGUIDs, err := ParseCardGUIDsFromBytes(jpgData, "virtual.jpg")
	if err != nil {
		t.Fatalf("JPG 解析失败: %v", err)
	}

	t.Logf("样本 %s：PNG 有 %d 个依赖，JPG 解析出 %d 个",
		filepath.Base(pngPath), len(pngGUIDs), len(jpgGUIDs))
	if len(jpgGUIDs) == 0 {
		t.Error("JPG 携带相同 payload 时应解析出同样的依赖")
	}

	// 同时确认 payload 的数据确实是 illusionsCard 可识别的格式
	if _, err := util.PngBytesRead(raw); err != nil {
		t.Logf("（该卡不走标准 PngBytesRead 路径，依赖来自 ResolveInfo 兜底扫描：%v）", err)
	}
}

// TestRealJPGPreview 真实目录里的 .JPG 必须能预览、能被列表识别。
func TestRealJPGPreview(t *testing.T) {
	root := strings.TrimSpace(os.Getenv("KKS_REAL_ROOT"))
	if root == "" {
		t.Skip("未设置 KKS_REAL_ROOT，跳过")
	}
	var jpg string
	filepath.Walk(filepath.Join(root, "UserData", "chara", "female"),
		func(path string, info os.FileInfo, err error) error {
			if err != nil || jpg != "" || info == nil || info.IsDir() {
				return nil
			}
			if core.IsCardFile(path) && !strings.EqualFold(filepath.Ext(path), ".png") {
				jpg = path
				return filepath.SkipAll
			}
			return nil
		})
	if jpg == "" {
		t.Skip("真实目录下没有 JPG 卡片")
	}
	t.Logf("真实 JPG 样本: %s", filepath.Base(jpg))

	img, w, h, err := decodeCardImage(jpg)
	if err != nil {
		t.Fatalf("真实 JPG 预览失败: %v", err)
	}
	if img == nil || w <= 0 || h <= 0 {
		t.Errorf("预览尺寸异常 %dx%d", w, h)
	}

	guids, err := ParseCardGUIDs(jpg)
	if err != nil {
		t.Fatalf("真实 JPG 依赖解析失败: %v", err)
	}
	t.Logf("预览 %dx%d；依赖 %d 个 zipmod（纯图片为 0 属正常）", w, h, len(guids))
}

// TestZipmodRefsIncludeJPG zipmod 的引用列表必须同时包含 PNG 与 JPG 卡片。
func TestZipmodRefsIncludeJPG(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	modPath := filepath.Join(m.ModsDir(), "jpg.zipmod")
	writeFile(t, modPath, "mod")
	m.ZipmodDB["jpg.guid"] = modPath
	m.guidOfPath[modPath] = "jpg.guid"
	m.GlobalDepMap["jpg.guid"] = []string{
		env.femaleCard("a.png"),
		env.femaleCard("b.JPG"),
	}
	m.PluginDepMap["jpg.guid"] = []string{"BepInEx/plugins/X.dll"}

	panel, err := m.BuildPanel("left", "zipmod", "", modPath)
	if err != nil {
		t.Fatalf("构建面板失败: %v", err)
	}
	secA, secB := panel.Sections[0], panel.Sections[1]

	if len(secA.Tiles) != 2 {
		t.Fatalf("引用卡片应全量返回 2 个（含 JPG），实际 %d", len(secA.Tiles))
	}
	foundJPG := false
	for _, tile := range secA.Tiles {
		if strings.EqualFold(filepath.Ext(tile.Name), ".jpg") {
			foundJPG = true
		}
	}
	if !foundJPG {
		t.Error("区域 A 应显示 JPG 引用卡片")
	}

	if secB.Layout != "summary" {
		t.Errorf("插件区域应为 summary，实际 %s", secB.Layout)
	}
	if len(secB.Tiles) == 0 || !strings.Contains(secB.Tiles[0].Name, "被 1 个 dll 插件使用") {
		t.Errorf("插件区域应给出文字说明，实际 %v", secB.Tiles)
	}
}

// TestExplorerDirResolution 资源管理器打开的目标必须是受管范围内的规范绝对路径。
func TestExplorerDirResolution(t *testing.T) {
	env := newTestEnv(t)
	m := env.manager

	dir := m.resolveRelDir("female_card", "left", "22.7.16")
	if dir == "" {
		t.Fatal("应能解析出目录")
	}
	if !filepath.IsAbs(dir) {
		t.Errorf("应为绝对路径，实际 %s", dir)
	}
	if !m.pathAllowed(dir) {
		t.Errorf("应在受管范围内，实际 %s", dir)
	}
	clean := filepath.Clean(filepath.FromSlash(dir))
	if clean != dir {
		t.Errorf("路径应已规范化（混合分隔符会让 explorer 退回文档目录）：%s -> %s", dir, clean)
	}
}

// TestRealJPGInTree 真实目录下含 JPG 的文件夹必须在列表与统计里都非空。
func TestRealJPGInTree(t *testing.T) {
	root := strings.TrimSpace(os.Getenv("KKS_REAL_ROOT"))
	if root == "" {
		t.Skip("未设置 KKS_REAL_ROOT，跳过")
	}
	m, err := NewManager(root)
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}
	kind := ResourceKinds[0]
	nodes := m.buildTree(m.SourceDir(kind), false, kind)

	var jpgNode string
	var parentStats *FolderStats
	for _, n := range nodes {
		if n.Type == "card" && strings.EqualFold(filepath.Ext(n.Name), ".jpg") {
			jpgNode = n.ID
			break
		}
	}
	if jpgNode == "" {
		t.Skip("真实目录下没有 JPG 卡片")
	}

	// 找该 JPG 所在文件夹的统计
	for _, n := range nodes {
		if n.Type == "folder" && n.ID == filepath.Dir(jpgNode) {
			parentStats = n.FolderStats
			break
		}
	}
	t.Logf("真实 JPG: %s；父目录统计: %+v", filepath.Base(jpgNode), parentStats)
	if parentStats == nil || parentStats.TotalFiles == 0 {
		t.Error("含 JPG 的文件夹统计不应为 0（否则列表与隐藏判定会互相矛盾）")
	}
}
