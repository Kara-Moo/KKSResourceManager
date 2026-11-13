package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMatchGUIDsInFileUTF8 验证 UTF-8 形态的 GUID 能被匹配。
func TestMatchGUIDsInFileUTF8(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "plug.dll")
	content := "xx some binary xx com.DeathWeasel.AccessoryHairBack yy"
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	guids := []string{"com.DeathWeasel.AccessoryHairBack", "com.other.NotPresent"}
	hits, err := MatchGUIDsInFile(p, guids)
	if err != nil {
		t.Fatalf("匹配失败: %v", err)
	}
	if len(hits) != 1 || hits[0] != "com.DeathWeasel.AccessoryHairBack" {
		t.Errorf("应匹配到 1 个 GUID，实际 %v", hits)
	}
}

// TestMatchGUIDsInFileUTF16 验证 UTF-16LE 形态的 GUID 能被匹配。
//
// 部分插件把资源以 UTF-16 存储，只匹配 UTF-8 会漏掉。
func TestMatchGUIDsInFileUTF16(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "plug.dll")

	guid := "com.DeathWeasel.AccessoryHairBack"
	content := append([]byte("head"), utf16le(guid)...)
	content = append(content, []byte("tail")...)
	if err := os.WriteFile(p, content, 0644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	hits, err := MatchGUIDsInFile(p, []string{guid})
	if err != nil {
		t.Fatalf("匹配失败: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("应匹配到 1 个 GUID，实际 %v", hits)
	}
}

// TestMatchGUIDsSkipsShortGUID 验证过短的 GUID 不参与匹配（避免大量误判）。
func TestMatchGUIDsSkipsShortGUID(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "plug.dll")
	if err := os.WriteFile(p, []byte("contains abc and abcd"), 0644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	hits, err := MatchGUIDsInFile(p, []string{"abc", "abcd"})
	if err != nil {
		t.Fatalf("匹配失败: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("短 GUID 不应参与匹配，实际 %v", hits)
	}
}

// TestGuardBlocksCardReference 验证第一道防线：卡片引用。
func TestGuardBlocksCardReference(t *testing.T) {
	g := &Guard{
		GlobalDepMap: map[string][]string{"g1": {"C:\\KK\\a.png"}},
	}
	ok, reason := g.IsZipmodSafeToMove("g1")
	if ok {
		t.Error("被卡片引用的 GUID 不应允许移出")
	}
	if !strings.Contains(reason, "资源文件引用") {
		t.Errorf("拒绝原因应说明卡片引用，实际 %q", reason)
	}
}

// TestGuardBlocksPluginReference 验证第二道防线：插件硬编码引用。
func TestGuardBlocksPluginReference(t *testing.T) {
	g := &Guard{
		GlobalDepMap: map[string][]string{},
		PluginDepMap: map[string][]string{"g2": {"BepInEx/plugins/Foo.dll"}},
	}
	ok, reason := g.IsZipmodSafeToMove("g2")
	if ok {
		t.Error("被插件引用的 GUID 不应允许移出")
	}
	if !strings.Contains(reason, "[插件保护]") {
		t.Errorf("拒绝原因应含 [插件保护] 前缀，实际 %q", reason)
	}
	if !strings.Contains(reason, "Foo.dll") {
		t.Errorf("拒绝原因应列出 DLL 名，实际 %q", reason)
	}
}

// TestGuardBlocksModDependents 验证第三道防线：mod 间依赖。
func TestGuardBlocksModDependents(t *testing.T) {
	g := &Guard{
		GlobalDepMap: map[string][]string{},
		ModDeps:      map[string][]string{"C:\\KK\\mods\\other.zipmod": {"g3"}},
	}
	ok, reason := g.IsZipmodSafeToMove("g3")
	if ok {
		t.Error("被其它 mod 依赖的 GUID 不应允许移出")
	}
	if !strings.Contains(reason, "其它 zipmod 依赖") {
		t.Errorf("拒绝原因应说明 mod 间依赖，实际 %q", reason)
	}
}

// TestGuardAllowsOrphan 三道防线都通过时允许移出。
func TestGuardAllowsOrphan(t *testing.T) {
	g := &Guard{
		GlobalDepMap: map[string][]string{},
		PluginDepMap: map[string][]string{},
		ModDeps:      map[string][]string{},
	}
	ok, reason := g.IsZipmodSafeToMove("g4")
	if !ok {
		t.Errorf("孤立 GUID 应允许移出，实际拒绝原因 %q", reason)
	}
}

// TestGuardEmptyGUIDRejected 空 GUID 必须拒绝。
func TestGuardEmptyGUIDRejected(t *testing.T) {
	g := &Guard{}
	ok, reason := g.IsZipmodSafeToMove("  ")
	if ok {
		t.Error("空 GUID 不应允许移出")
	}
	if !strings.Contains(reason, "无法识别 GUID") {
		t.Errorf("拒绝原因应说明无法识别 GUID，实际 %q", reason)
	}
}

// TestGuardExclusionSet 验证排除集：正在移动的文件不算「仍被引用」。
func TestGuardExclusionSet(t *testing.T) {
	g := &Guard{
		GlobalDepMap: map[string][]string{"g1": {"C:\\KK\\a.png", "C:\\KK\\b.png"}},
	}
	// 排除 a.png 后，b.png 仍引用 → 仍应拒绝
	ok, _ := g.IsZipmodSafeToMoveEx("g1", map[string]bool{"C:\\KK\\a.png": true})
	if ok {
		t.Error("排除一个引用者后仍有其它引用，应拒绝")
	}
	// 排除全部 → 允许
	ok, _ = g.IsZipmodSafeToMoveEx("g1", map[string]bool{
		"C:\\KK\\a.png": true, "C:\\KK\\b.png": true,
	})
	if !ok {
		t.Error("排除全部引用者后应允许移出")
	}
}

// TestMatchGUIDsNoFalsePositive 验证不存在的 GUID 不会被匹配。
func TestMatchGUIDsNoFalsePositive(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "plug.dll")
	if err := os.WriteFile(p, []byte("nothing to see here"), 0644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	hits, err := MatchGUIDsInFile(p, []string{"com.not.present.at.all"})
	if err != nil {
		t.Fatalf("匹配失败: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("不应有匹配，实际 %v", hits)
	}
}
