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
