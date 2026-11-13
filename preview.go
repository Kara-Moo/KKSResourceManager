package main

// preview.go —— 卡片缩略图预览
//
// Wails v2 的 WebView 出于安全策略不允许通过 file:// 直接加载本地绝对路径，
// 因此由后端读取图片、缩放后以 base64 data URI 的形式返回，前端用 <img src> 渲染。
//
// KKS 卡片文件很大（部分超过 40MB），必须先缩放再编码，
// 否则 base64 传输会非常慢甚至拖垮 WebView。

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // 注册 GIF 解码器
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/image/draw"
)

const (
	// previewMaxWidth 缩略图最大宽度
	previewMaxWidth = 320
	// previewMaxHeight 缩略图最大高度
	previewMaxHeight = 400
	// previewMinPixels 小于该像素数的图片视为无效（占位图）
	previewMinPixels = 32
	// previewMaxPixels 超过该像素数则拒绝解码，防止内存爆炸
	previewMaxPixels = 80_000_000
	// previewMaxFileSize 超过该大小的文件直接拒绝读取
	previewMaxFileSize = 256 << 20 // 256MB
	// previewJPEGQuality JPEG 编码质量
	previewJPEGQuality = 82
)

// PreviewData 缩略图数据。
type PreviewData struct {
	// DataURI base64 编码的 data URI，可直接用于 <img src>
	DataURI string `json:"dataUri"`
	// Width 缩略图宽度
	Width int `json:"width"`
	// Height 缩略图高度
	Height int `json:"height"`
	// SourceWidth 原始图片宽度
	SourceWidth int `json:"sourceWidth"`
	// SourceHeight 原始图片高度
	SourceHeight int `json:"sourceHeight"`
	// Message 失败原因（成功时为空）
	Message string `json:"message"`
}

// BuildPreview 生成指定图片文件的缩略图。
//
// 只允许读取 KKS_ROOT 或隔离区 DELCARDS_ROOT 内的文件，
// 避免前端传入任意路径读取系统文件。
func (m *Manager) BuildPreview(path string) (PreviewData, error) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return PreviewData{}, fmt.Errorf("路径无效: %w", err)
	}
	if !m.pathAllowed(abs) {
		return PreviewData{}, fmt.Errorf("该路径不在允许的范围内")
	}

	info, err := os.Stat(abs)
	if err != nil {
		return PreviewData{}, fmt.Errorf("读取文件失败: %w", err)
	}
	if info.IsDir() {
		return PreviewData{}, fmt.Errorf("这是文件夹，无法预览")
	}
	if info.Size() == 0 {
		return PreviewData{}, fmt.Errorf("文件为空")
	}
	if info.Size() > previewMaxFileSize {
		return PreviewData{}, fmt.Errorf("文件过大（%.1f MB），已跳过预览", float64(info.Size())/(1024*1024))
	}

	img, srcW, srcH, err := decodeCardImage(abs)
	if err != nil {
		return PreviewData{}, err
	}
	if img == nil {
		return PreviewData{}, fmt.Errorf("无法解析图片内容")
	}

	// 卡片 PNG 常常带透明背景，先合成到白底再编码 JPEG
	flattened := flattenOnWhite(img)

	// 等比缩放到预览尺寸内
	scaled := scaleToFit(flattened, previewMaxWidth, previewMaxHeight)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: previewJPEGQuality}); err != nil {
		return PreviewData{}, fmt.Errorf("编码缩略图失败: %w", err)
	}

	bounds := scaled.Bounds()
	return PreviewData{
		DataURI:      "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()),
		Width:        bounds.Dx(),
		Height:       bounds.Dy(),
		SourceWidth:  srcW,
		SourceHeight: srcH,
	}, nil
}

// buildThumbJPEG 生成缩略图的 JPEG 原始字节。
//
// 与 BuildPreview 共用解码/缩放逻辑，只是输出形式不同：
//   - BuildPreview 返回 data URI（前端直接用）
//   - buildThumbJPEG 返回原始字节（磁盘缓存 / 队列用）
func (m *Manager) buildThumbJPEG(path string) ([]byte, error) {
	return m.buildThumbJPEGSized(path, previewMaxWidth, previewMaxHeight, previewJPEGQuality)
}

// 大图档尺寸（v2.2 FIX-02）。
//
// 九宫格 320×400 在 376px 宽的面板上拉伸会发虚，
// 单卡预览需要 2x 余量，所以单独一档 720×960。
const (
	// previewLargeWidth 单卡大图最大宽度
	previewLargeWidth = 720
	// previewLargeHeight 单卡大图最大高度
	previewLargeHeight = 960
	// previewLargeQuality 大图 JPEG 质量（略高于九宫格）
	previewLargeQuality = 85
)

// buildThumbJPEGSized 按指定尺寸档生成缩略图的 JPEG 字节。
func (m *Manager) buildThumbJPEGSized(path string, maxW, maxH, quality int) ([]byte, error) {
	abs, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return nil, err
	}
	if !m.pathAllowed(abs) {
		return nil, fmt.Errorf("该路径不在允许的范围内")
	}

	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if info.IsDir() || info.Size() == 0 {
		return nil, fmt.Errorf("不是可预览的文件")
	}
	if info.Size() > previewMaxFileSize {
		return nil, fmt.Errorf("文件过大（%.1f MB）", float64(info.Size())/(1024*1024))
	}

	img, _, _, err := decodeCardImage(abs)
	if err != nil {
		return nil, err
	}
	if img == nil {
		return nil, fmt.Errorf("无法解析图片内容")
	}

	flattened := flattenOnWhite(img)
	scaled := scaleToFit(flattened, maxW, maxH)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// pathAllowed 判断路径是否位于受管范围内。
func (m *Manager) pathAllowed(abs string) bool {
	return isUnder(abs, m.KksRoot) || isUnder(abs, m.DelCardsRoot)
}

// decodeCardImage 解析卡片文件中的图片。
//
// KKS 卡片的结构是「完整 PNG 图片 + IEND 之后的追加数据」，
// Go 的 PNG 解码器读到 IEND 即停止，尾随数据会被自动忽略。
//
// 少数卡片的首个 PNG 只是占位图，此时会继续在文件中寻找后续 PNG 帧，
// 取面积最大的一个作为预览图。
func decodeCardImage(path string) (image.Image, int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()

	// 优先按标准图片格式整体解析（PNG 或 JPEG）。
	// KKS 的 PNG 卡是「完整 PNG + IEND 之后追加数据」，Go 解码器读到 IEND 即停；
	// JPG 卡同理，解码器读到 EOI 即停，尾随数据被自动忽略。
	cfg, format, cfgErr := image.DecodeConfig(f)
	if cfgErr == nil {
		if cfg.Width*cfg.Height > previewMaxPixels {
			return nil, 0, 0, fmt.Errorf("图片分辨率过高（%dx%d），已跳过预览", cfg.Width, cfg.Height)
		}
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			var img image.Image
			var decErr error
			switch format {
			case "png":
				img, decErr = png.Decode(f)
			case "jpeg":
				img, decErr = jpeg.Decode(f)
			}
			if decErr == nil && img != nil {
				b := img.Bounds()
				if b.Dx()*b.Dy() >= previewMinPixels {
					return img, b.Dx(), b.Dy(), nil
				}
			}
		}
	}

	// 回退：在文件中逐个寻找 PNG 帧，取面积最大的
	best, bestW, bestH, err := findLargestPNGFrame(path)
	if err != nil {
		return nil, 0, 0, err
	}
	return best, bestW, bestH, nil
}

// findLargestPNGFrame 遍历文件中的所有 PNG 帧，返回面积最大的那张。
func findLargestPNGFrame(path string) (image.Image, int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("打开文件失败: %w", err)
	}
	defer f.Close()

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("读取文件失败: %w", err)
	}

	signature := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	var best image.Image
	bestArea := 0
	bestW, bestH := 0, 0

	offset := 0
	for offset < len(data) {
		rel := bytes.Index(data[offset:], signature)
		if rel < 0 {
			break
		}
		start := offset + rel
		offset = start + len(signature)

		img, err := png.Decode(bytes.NewReader(data[start:]))
		if err != nil || img == nil {
			continue
		}
		b := img.Bounds()
		area := b.Dx() * b.Dy()
		if area > bestArea {
			best, bestArea, bestW, bestH = img, area, b.Dx(), b.Dy()
		}
	}

	if best == nil {
		return nil, 0, 0, fmt.Errorf("该文件不包含可预览的图片")
	}
	return best, bestW, bestH, nil
}

// flattenOnWhite 把带透明通道的图片合成到白色背景上。
// JPEG 不支持透明，必须先处理。
func flattenOnWhite(src image.Image) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst
}

// scaleToFit 等比缩放图片，使其落在 maxW x maxH 的范围内。
// 若图片本身已小于该尺寸，则原样返回。
func scaleToFit(src image.Image, maxW, maxH int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxW && h <= maxH {
		return src
	}

	scale := float64(maxW) / float64(w)
	if scaleH := float64(maxH) / float64(h); scaleH < scale {
		scale = scaleH
	}
	newW := int(float64(w) * scale)
	newH := int(float64(h) * scale)
	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	return dst
}
