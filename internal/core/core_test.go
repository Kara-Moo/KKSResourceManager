package core

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNaturalLess 验证自然序排序。
//
// 关键场景：22.7.9 必须排在 22.7.10 之前（字典序会排反）。
func TestNaturalLess(t *testing.T) {
	cases := []struct {
		a, b string
	}{
		{"22.7.9", "22.7.10"},
		{"2", "10"},
		{"a2", "a10"},
		{"card1", "card2"},
		{"card2", "card10"},
		{"0Harmony", "0Harmony20"},
	}
	for _, c := range cases {
		if !NaturalLess(c.a, c.b) {
			t.Errorf("NaturalLess(%q, %q) 应为 true", c.a, c.b)
		}
		if NaturalLess(c.b, c.a) {
			t.Errorf("NaturalLess(%q, %q) 应为 false（反对称）", c.b, c.a)
		}
	}
}

// TestNaturalLessEqual 相同字符串不应互相小于。
func TestNaturalLessEqual(t *testing.T) {
	if NaturalLess("same", "same") {
		t.Error("相同字符串不应满足严格小于")
	}
}

// TestSortNatural 验证列表按自然序排序。
func TestSortNatural(t *testing.T) {
	list := []string{"22.7.10", "22.7.9", "22.7.1", "1.0"}
	SortNatural(list)
	want := []string{"1.0", "22.7.1", "22.7.9", "22.7.10"}
	for i := range want {
		if list[i] != want[i] {
			t.Fatalf("排序错误：%v，期望 %v", list, want)
		}
	}
}

// TestSnapshotCache 验证目录快照缓存的命中与失效。
func TestSnapshotCache(t *testing.T) {
	dir := t.TempDir()
	c := NewSnapshotCache(8)

	// 首次：未命中
	if _, ok := c.Get(dir); ok {
		t.Error("首次不应命中缓存")
	}

	// 列举后写入
	snap, err := c.ListDir(dir, nil)
	if err != nil {
		t.Fatalf("列举失败: %v", err)
	}
	if len(snap.Entries) != 0 {
		t.Errorf("空目录应有 0 项，实际 %d", len(snap.Entries))
	}
	if _, ok := c.Get(dir); !ok {
		t.Error("列举后应命中缓存")
	}

	// 新增文件后 mtime 变化 → 缓存失效
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	snap2, err := c.ListDir(dir, nil)
	if err != nil {
		t.Fatalf("列举失败: %v", err)
	}
	if len(snap2.Entries) != 1 {
		t.Errorf("应有 1 项，实际 %d", len(snap2.Entries))
	}
}

// TestSnapshotListDirFilter 验证过滤器生效。
func TestSnapshotListDirFilter(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.png"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0755)

	c := NewSnapshotCache(8)
	snap, err := c.ListDir(dir, func(name string, isDir bool) bool {
		return !isDir && filepath.Ext(name) == ".png"
	})
	if err != nil {
		t.Fatalf("列举失败: %v", err)
	}
	if len(snap.Entries) != 1 || snap.Entries[0].Name != "a.png" {
		t.Errorf("过滤后应只有 a.png，实际 %v", snap.Entries)
	}
}

// TestPickCoverDirectFile 封面选取：一级目录直接有 PNG 时应立即命中。
func TestPickCoverDirectFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "b.png"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(dir, "a.png"), []byte("x"), 0644)

	c := NewSnapshotCache(16)
	cover := PickCover(dir, func(d string) (*DirSnapshot, error) {
		return c.ListDir(d, func(name string, isDir bool) bool { return true })
	})
	// 自然序第一张 → a.png
	if filepath.Base(cover) != "a.png" {
		t.Errorf("封面应为自然序第一张 a.png，实际 %s", cover)
	}
}

// TestPickCoverBFS 封面选取：一级没有 PNG 时应按 BFS 深度查找。
func TestPickCoverBFS(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub", "deep"), 0755)
	os.WriteFile(filepath.Join(dir, "sub", "deep", "x.png"), []byte("x"), 0644)

	c := NewSnapshotCache(16)
	cover := PickCover(dir, func(d string) (*DirSnapshot, error) {
		return c.ListDir(d, func(name string, isDir bool) bool { return true })
	})
	if filepath.Base(cover) != "x.png" {
		t.Errorf("应找到深层封面 x.png，实际 %s", cover)
	}
}

// TestPickCoverDepthLimit 封面选取：超过深度 3 不应再往下找。
func TestPickCoverDepthLimit(t *testing.T) {
	dir := t.TempDir()
	// 深度 4：a/b/c/d/x.png
	os.MkdirAll(filepath.Join(dir, "a", "b", "c", "d"), 0755)
	os.WriteFile(filepath.Join(dir, "a", "b", "c", "d", "x.png"), []byte("x"), 0644)

	c := NewSnapshotCache(16)
	cover := PickCover(dir, func(d string) (*DirSnapshot, error) {
		return c.ListDir(d, func(name string, isDir bool) bool { return true })
	})
	if cover != "" {
		t.Errorf("超过深度限制应返回空，实际 %s", cover)
	}
}

// TestPickCoverEmpty 封面选取：空目录返回空字符串（不是错误）。
func TestPickCoverEmpty(t *testing.T) {
	dir := t.TempDir()
	c := NewSnapshotCache(16)
	cover := PickCover(dir, func(d string) (*DirSnapshot, error) {
		return c.ListDir(d, func(name string, isDir bool) bool { return true })
	})
	if cover != "" {
		t.Errorf("空目录应返回空封面，实际 %s", cover)
	}
}

// TestCoverCacheInvalidate 封面缓存失效。
func TestCoverCacheInvalidate(t *testing.T) {
	dir := t.TempDir()
	c := NewCoverCache(8)
	c.Put(dir, "/some/cover.png")
	if p, ok := c.Get(dir); !ok || p != "/some/cover.png" {
		t.Error("应命中封面缓存")
	}
	c.Invalidate(dir)
	if _, ok := c.Get(dir); ok {
		t.Error("失效后不应命中")
	}
}

// TestParseSize 验证大小格式化。
func TestParseSize(t *testing.T) {
	cases := map[int64]string{
		0:       "0 B",
		512:     "512 B",
		1024:    "1.0 KB",
		1048576: "1.0 MB",
	}
	for in, want := range cases {
		if got := ParseSize(in); got != want {
			t.Errorf("ParseSize(%d) = %q，期望 %q", in, got, want)
		}
	}
}
