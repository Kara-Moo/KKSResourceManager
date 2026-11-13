package main

// parser_test.go —— 解析层单元测试

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

// writeZip 构造一个包含指定文件的 zip（模拟 zipmod）。
func writeZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("创建 zip 失败: %v", err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("创建 zip 条目失败: %v", err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("写入 zip 条目失败: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip 失败: %v", err)
	}
}

func TestReadModManifest(t *testing.T) {
	dir := t.TempDir()
	modPath := filepath.Join(dir, "sample.zipmod")
	writeZip(t, modPath, map[string]string{
		"manifest.xml": `<?xml version="1.0" encoding="utf-8"?>
<manifest>
  <name>Sample Mod</name>
  <guid>com.test.sample</guid>
  <version>1.0.0</version>
  <author>Tester</author>
</manifest>`,
	})

	mod, err := ReadModManifest(modPath)
	if err != nil {
		t.Fatalf("解析 manifest 失败: %v", err)
	}
	if mod.GUID != "com.test.sample" {
		t.Errorf("GUID 不正确，期望 com.test.sample，实际 %q", mod.GUID)
	}
	if mod.Name != "Sample Mod" {
		t.Errorf("Name 不正确，实际 %q", mod.Name)
	}
	if mod.FileName != "sample.zipmod" {
		t.Errorf("FileName 不正确，实际 %q", mod.FileName)
	}
}

func TestReadModManifestUTF16(t *testing.T) {
	dir := t.TempDir()
	modPath := filepath.Join(dir, "utf16.zipmod")

	// UTF-16LE 带 BOM 的 manifest
	content := `<?xml version="1.0"?>
<manifest><guid>com.test.utf16</guid><name>UTF16</name></manifest>`
	utf16Bytes := []byte{0xFF, 0xFE}
	for _, r := range content {
		utf16Bytes = append(utf16Bytes, byte(r), 0x00)
	}
	writeZip(t, modPath, map[string]string{"manifest.xml": string(utf16Bytes)})

	mod, err := ReadModManifest(modPath)
	if err != nil {
		t.Fatalf("解析 UTF-16 manifest 失败: %v", err)
	}
	if mod.GUID != "com.test.utf16" {
		t.Errorf("UTF-16 GUID 不正确，实际 %q", mod.GUID)
	}
}

func TestReadModManifestMalformedXML(t *testing.T) {
	dir := t.TempDir()
	modPath := filepath.Join(dir, "broken.zipmod")
	// 故意带一个多余的闭合标签，验证修补逻辑生效
	writeZip(t, modPath, map[string]string{
		"manifest.xml": `<manifest><guid>com.test.broken</guid></name></manifest>`,
	})

	mod, err := ReadModManifest(modPath)
	if err != nil {
		t.Fatalf("解析畸形 manifest 失败: %v", err)
	}
	if mod.GUID != "com.test.broken" {
		t.Errorf("畸形 manifest 的 GUID 不正确，实际 %q", mod.GUID)
	}
}

func TestReadModManifestNoManifest(t *testing.T) {
	dir := t.TempDir()
	modPath := filepath.Join(dir, "nomanifest.zipmod")
	writeZip(t, modPath, map[string]string{"readme.txt": "hello"})

	mod, err := ReadModManifest(modPath)
	if err != nil {
		t.Fatalf("无 manifest 时不应报错: %v", err)
	}
	if mod.GUID != "" {
		t.Errorf("无 manifest 时 GUID 应为空，实际 %q", mod.GUID)
	}
}

func TestRemoveUnmatchedClosingTags(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"多余闭合标签", "<a><b>text</c></a>", "<a><b>text</b></a>"},
		{"正常 XML 不变", "<a><b>text</b></a>", "<a><b>text</b></a>"},
		{"自闭合标签保留", "<a><br/></a>", "<a><br/></a>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := removeUnmatchedClosingTags(tc.in); got != tc.want {
				t.Errorf("期望 %q，实际 %q", tc.want, got)
			}
		})
	}
}

func TestIsLikelyUTF16(t *testing.T) {
	if !isLikelyUTF16([]byte{0xFF, 0xFE, 0x41}) {
		t.Error("应识别 UTF-16LE BOM")
	}
	if !isLikelyUTF16([]byte{0xFE, 0xFF, 0x41}) {
		t.Error("应识别 UTF-16BE BOM")
	}
	if isLikelyUTF16([]byte{0x3C, 0x3F}) {
		t.Error("不应把 UTF-8 误判为 UTF-16")
	}
}

func TestDedupeStrings(t *testing.T) {
	got := dedupeStrings([]string{"a", " b ", "a", "", "b", "c"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("期望 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 项期望 %q，实际 %q", i, want[i], got[i])
		}
	}
}

func TestParseCardGUIDsNonPNG(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notacard.png")
	if err := os.WriteFile(p, []byte("this is not a png"), 0644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	guids, err := ParseCardGUIDs(p)
	if err != nil {
		t.Fatalf("非卡片文件不应报错: %v", err)
	}
	if len(guids) != 0 {
		t.Errorf("非卡片文件应返回空 GUID，实际 %v", guids)
	}
}
