package main

// preview_test.go —— 缩略图预览与目录校验测试

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePNG 生成一张指定尺寸的 PNG 文件。
func writePNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("编码 PNG 失败: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
}

func TestBuildPreviewScalesDown(t *testing.T) {
	env := newTestEnv(t)
	// 构造一张 1600x2000 的大图
	card := env.femaleCard("big.png")
	writePNG(t, card, 1600, 2000)

	data, err := env.manager.BuildPreview(card)
	if err != nil {
		t.Fatalf("生成预览失败: %v", err)
	}
	if !strings.HasPrefix(data.DataURI, "data:image/jpeg;base64,") {
		t.Fatalf("应返回 JPEG data URI，实际前缀 %q", data.DataURI[:min(40, len(data.DataURI))])
	}
	// 必须被缩放到预览尺寸内
	if data.Width > previewMaxWidth || data.Height > previewMaxHeight {
		t.Errorf("缩略图超出限制: %dx%d", data.Width, data.Height)
	}
	if data.SourceWidth != 1600 || data.SourceHeight != 2000 {
		t.Errorf("原始尺寸记录错误: %dx%d", data.SourceWidth, data.SourceHeight)
	}
	// base64 内容应可解码
	payload := strings.TrimPrefix(data.DataURI, "data:image/jpeg;base64,")
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		t.Errorf("base64 载荷无效: %v", err)
	}
}

func TestBuildPreviewKeepsSmallImage(t *testing.T) {
	env := newTestEnv(t)
	card := env.femaleCard("small.png")
	writePNG(t, card, 64, 64)

	data, err := env.manager.BuildPreview(card)
	if err != nil {
		t.Fatalf("生成预览失败: %v", err)
	}
	if data.Width != 64 || data.Height != 64 {
		t.Errorf("小图不应被放大，实际 %dx%d", data.Width, data.Height)
	}
}

func TestBuildPreviewRejectsOutsidePath(t *testing.T) {
	env := newTestEnv(t)
	outside := filepath.Join(t.TempDir(), "outside.png")
	writePNG(t, outside, 100, 100)

	if _, err := env.manager.BuildPreview(outside); err == nil {
		t.Error("应拒绝读取受管范围之外的文件")
	}
}

func TestBuildPreviewRejectsNonImage(t *testing.T) {
	env := newTestEnv(t)
	p := env.femaleCard("notimage.png")
	writeFile(t, p, "definitely not a png")

	data, err := env.manager.BuildPreview(p)
	if err == nil {
		t.Error("非图片文件应返回错误")
	}
	if data.DataURI != "" {
		t.Error("失败时不应返回 data URI")
	}
}

func TestBuildPreviewHandlesDirectory(t *testing.T) {
	env := newTestEnv(t)
	if _, err := env.manager.BuildPreview(env.femaleCard("")); err == nil {
		t.Error("目录应返回错误")
	}
}

// ---------------------------------------------------------------------------
// 目录结构校验
// ---------------------------------------------------------------------------

func TestValidateRootDetectsMissingDirs(t *testing.T) {
	env := newTestEnv(t)

	// 空目录下，全部必需目录都缺失
	missing, err := env.manager.ValidateRoot()
	if err != nil {
		t.Fatalf("校验不应报错: %v", err)
	}
	if len(missing) != len(RequiredDirs) {
		t.Errorf("应报告 %d 个缺失目录，实际 %d: %v", len(RequiredDirs), len(missing), missing)
	}

	// 补齐全部必需目录后应通过
	for _, rel := range RequiredDirs {
		writeFile(t, filepath.Join(env.manager.KksRoot, rel, "x.png"), "x")
	}

	missing, err = env.manager.ValidateRoot()
	if err != nil {
		t.Fatalf("校验不应报错: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("目录齐全时不应报告缺失，实际 %v", missing)
	}
}

// TestValidateRootIgnoresOptionalDirs 验证可选目录缺失不影响校验。
//
// 很多 KKS 安装没有 UserData/coordinate/male（男装资源库），
// 其缺失只应导致该类型列表为空，不能阻断整个程序。
func TestValidateRootIgnoresOptionalDirs(t *testing.T) {
	env := newTestEnv(t)
	for _, rel := range RequiredDirs {
		writeFile(t, filepath.Join(env.manager.KksRoot, rel, "x.png"), "x")
	}

	// 确保 coordinate/male 不存在
	if err := os.RemoveAll(filepath.Join(env.manager.KksRoot, "UserData", "coordinate", "male")); err != nil {
		t.Fatalf("删除目录失败: %v", err)
	}

	missing, err := env.manager.ValidateRoot()
	if err != nil {
		t.Fatalf("校验不应报错: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("可选目录缺失不应报错，实际报告: %v", missing)
	}
}

func TestValidateRootDetectsSingleMissing(t *testing.T) {
	env := newTestEnv(t)
	for _, rel := range RequiredDirs {
		writeFile(t, filepath.Join(env.manager.KksRoot, rel, "x.png"), "x")
	}

	// 删掉 pose 目录
	if err := os.RemoveAll(filepath.Join(env.manager.KksRoot, "UserData", "Studio", "pose")); err != nil {
		t.Fatalf("删除目录失败: %v", err)
	}

	missing, err := env.manager.ValidateRoot()
	if err != nil {
		t.Fatalf("校验不应报错: %v", err)
	}
	if len(missing) != 1 {
		t.Fatalf("应报告 1 个缺失目录，实际 %d: %v", len(missing), missing)
	}
	if !strings.Contains(missing[0], "pose") {
		t.Errorf("缺失目录应为 pose，实际 %q", missing[0])
	}
}

// ---------------------------------------------------------------------------
// 工作室角色已移除
// ---------------------------------------------------------------------------

func TestStudioCharNotScanned(t *testing.T) {
	env := newTestEnv(t)

	// 在「工作室角色」路径下放一个文件，扫描时不应被统计
	writeFile(t, filepath.Join(env.root, "UserData", "Studio", "character", "c.png"), "x")
	// 同时放一个正常的女卡用于对照
	writeFile(t, env.femaleCard("f.png"), "x")

	result := env.manager.ScanAll(nil)
	if result.ResourceCount != 1 {
		t.Errorf("只应统计 1 个资源文件（工作室角色不应参与扫描），实际 %d", result.ResourceCount)
	}
}

func TestStudioCharDelDirNotCreated(t *testing.T) {
	env := newTestEnv(t)

	delStudioChar := filepath.Join(env.delRoot, "DEL-studio_char")
	if _, err := os.Stat(delStudioChar); !os.IsNotExist(err) {
		t.Error("不应创建 DEL-studio_char 目录")
	}

	// 逐个确认 ResourceKinds 中不含工作室角色
	for _, kind := range ResourceKinds {
		if strings.Contains(kind.SourceRel, "character") || kind.DelName == "DEL-studio_char" {
			t.Errorf("ResourceKinds 中不应包含工作室角色: %+v", kind)
		}
	}

	// 标准隔离子目录数量应为 6 + 1(zipmod)
	expected := []string{
		"DEL-female_card", "DEL-male_card", "DEL-scene",
		"DEL-pose", "DEL-coord_f", "DEL-coord_m", "DEL-zipmod",
	}
	for _, name := range expected {
		if _, err := os.Stat(filepath.Join(env.delRoot, name)); err != nil {
			t.Errorf("隔离目录 %s 未创建: %v", name, err)
		}
	}
}

// min 返回两个整数中的较小值。
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
