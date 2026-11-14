package main

// realworld_test.go —— 针对真实 KKS 安装目录的验证
//
// 通过环境变量 KKS_REAL_ROOT 指定真实游戏根目录时才会执行，
// 例如：KKS_REAL_ROOT=E:\KKV114 go test -run TestReal -v
//
// 这些测试只读不写，绝不会移动或删除用户的任何文件。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realRoot 返回真实 KKS 根目录，未设置则跳过测试。
func realRoot(t *testing.T) string {
	t.Helper()
	root := strings.TrimSpace(os.Getenv("KKS_REAL_ROOT"))
	if root == "" {
		t.Skip("未设置 KKS_REAL_ROOT，跳过真实目录验证")
	}
	if _, err := os.Stat(root); err != nil {
		t.Skipf("真实目录不可访问，跳过: %v", err)
	}
	return root
}

// TestRealValidateRoot 在真实目录上验证结构校验结果。
func TestRealValidateRoot(t *testing.T) {
	root := realRoot(t)
	m, err := NewManager(root)
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}
	missing, err := m.ValidateRoot()
	if err != nil {
		t.Fatalf("校验出错: %v", err)
	}
	if len(missing) > 0 {
		t.Errorf("真实目录应通过校验，但报告缺失: %v", missing)
	}
}

// TestRealPreviewSamples 在真实目录上抽样生成缩略图。
func TestRealPreviewSamples(t *testing.T) {
	root := realRoot(t)
	m, err := NewManager(root)
	if err != nil {
		t.Fatalf("创建 Manager 失败: %v", err)
	}

	sampled := 0
	failed := 0
	for _, kind := range ResourceKinds {
		dir := m.SourceDir(kind)
		files := m.collectFiles(dir, func(path string) bool {
			return strings.EqualFold(filepath.Ext(path), ".png")
		})
		// 每种类型最多抽 5 个，避免全量处理
		for i, path := range files {
			if i >= 5 {
				break
			}
			sampled++
			data, err := m.BuildPreview(path)
			if err != nil {
				failed++
				t.Logf("预览失败 %s: %v", filepath.Base(path), err)
				continue
			}
			if data.DataURI == "" {
				failed++
				t.Logf("预览为空 %s", filepath.Base(path))
				continue
			}
			if len(data.DataURI) > 500*1024 {
				t.Logf("缩略图偏大 %s: %d 字节", filepath.Base(path), len(data.DataURI))
			}
			t.Logf("预览成功 %s -> %dx%d (原 %dx%d, %d 字节)",
				filepath.Base(path), data.Width, data.Height,
				data.SourceWidth, data.SourceHeight, len(data.DataURI))
		}
	}
	if sampled == 0 {
		t.Skip("真实目录中没有找到可抽样的 PNG 文件")
	}
	t.Logf("共抽样 %d 个文件，失败 %d 个", sampled, failed)
}

