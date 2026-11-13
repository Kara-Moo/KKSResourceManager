package main

// cardimage_test.go —— 验证真实 KKS 卡片结构（PNG + IEND 后追加数据）能正确解析出图片
//
// 这是预览功能能否工作的关键：KKS 卡片文件在 PNG 的 IEND 之后还追加了卡片数据，
// 如果解码器不能正确处理尾随数据，预览就会失败。

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// writeKKSLikeCard 构造一个与 KKS 卡片结构一致的文件：
// [完整 PNG 图片][IEND 之后的追加数据]
func writeKKSLikeCard(t *testing.T, path string, w, h int, trailer []byte) {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("编码 PNG 失败: %v", err)
	}
	// 追加卡片数据（模拟 IEND 之后的二进制负载）
	buf.Write(trailer)

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
}

func TestPreviewHandlesCardWithTrailingData(t *testing.T) {
	env := newTestEnv(t)
	card := env.femaleCard("card_with_trailer.png")

	// 模拟 KKS 卡片：PNG 之后跟着一段二进制卡片数据
	trailer := make([]byte, 4096)
	for i := range trailer {
		trailer[i] = byte(i % 251)
	}
	writeKKSLikeCard(t, card, 512, 512, trailer)

	data, err := env.manager.BuildPreview(card)
	if err != nil {
		t.Fatalf("带追加数据的卡片应能生成预览: %v", err)
	}
	if data.DataURI == "" {
		t.Fatal("应返回 data URI")
	}
	if data.SourceWidth != 512 || data.SourceHeight != 512 {
		t.Errorf("原始尺寸错误: %dx%d", data.SourceWidth, data.SourceHeight)
	}
}

func TestPreviewHandlesLargeCard(t *testing.T) {
	env := newTestEnv(t)
	card := env.femaleCard("large.png")
	// 2000x2500，模拟大尺寸卡片
	writeKKSLikeCard(t, card, 2000, 2500, make([]byte, 1024))

	data, err := env.manager.BuildPreview(card)
	if err != nil {
		t.Fatalf("大尺寸卡片应能生成预览: %v", err)
	}
	if data.Width > previewMaxWidth || data.Height > previewMaxHeight {
		t.Errorf("大图应被缩放，实际 %dx%d", data.Width, data.Height)
	}
	// 缩略图不应过大（保证传输效率）
	if len(data.DataURI) > 300*1024 {
		t.Errorf("缩略图 data URI 过大: %d 字节", len(data.DataURI))
	}
}

func TestPreviewHandlesTransparentCard(t *testing.T) {
	env := newTestEnv(t)
	card := env.femaleCard("transparent.png")

	// 全透明图片
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("编码 PNG 失败: %v", err)
	}
	writeFile(t, card, "") // 占位，下面用原始字节覆盖
	if err := os.WriteFile(card, buf.Bytes(), 0644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	// 透明图合成到白底后仍应能编码为 JPEG
	data, err := env.manager.BuildPreview(card)
	if err != nil {
		t.Fatalf("透明卡片应能生成预览: %v", err)
	}
	if !bytes.HasPrefix([]byte(data.DataURI), []byte("data:image/jpeg;base64,")) {
		t.Error("应返回 JPEG data URI")
	}
}
