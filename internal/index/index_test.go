package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setup 构造一个测试用的扫描环境。
type env struct {
	root     string
	resource string
	mods     string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "KKV114")
	e := &env{
		root:     root,
		resource: filepath.Join(root, "UserData", "chara", "female"),
		mods:     filepath.Join(root, "mods"),
	}
	if err := os.MkdirAll(e.resource, 0755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.MkdirAll(e.mods, 0755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	return e
}

func (e *env) writeCard(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(e.resource, name)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	return p
}

func (e *env) writeZipmod(t *testing.T, name, guid string) string {
	t.Helper()
	p := filepath.Join(e.mods, name)
	if err := os.WriteFile(p, []byte("zipmod:"+guid), 0644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	return p
}

// newScanner 构造一个使用简单桩函数的 Scanner。
func (e *env) newScanner() *Scanner {
	return &Scanner{
		Root:  e.root,
		ModsDir: e.mods,
		ResourceRoots: map[string]string{
			"female_card": e.resource,
		},
		ParseZipmod: func(path string) (string, []string, error) {
			data, err := os.ReadFile(path)
			if err != nil {
				return "", nil, err
			}
			s := string(data)
			guid := ""
			if len(s) > 7 {
				guid = s[7:]
			}
			return guid, nil, nil
		},
		ParseCard: func(path string) ([]string, error) {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			// 约定：文件内容形如 "g1,g2"
			guids := []string{}
			s := string(data)
			if s != "" {
				guids = splitComma(s)
			}
			return guids, nil
		},
		MatchPlugin: func(path string, guids []string) ([]string, error) {
			return nil, nil
		},
	}
}

func splitComma(s string) []string {
	out := []string{}
	cur := ""
	for _, c := range s {
		if c == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// TestScanFirstTimeParsesAll 首次扫描应解析全部文件。
func TestScanFirstTimeParsesAll(t *testing.T) {
	e := newEnv(t)
	e.writeCard(t, "a.png", "g1")
	e.writeCard(t, "b.png", "g2")
	e.writeZipmod(t, "m.zipmod", "guid.m")

	s := e.newScanner()
	idx, derived, stats, err := s.Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if stats.Parsed != 3 {
		t.Errorf("应解析 3 个文件，实际 %d", stats.Parsed)
	}
	if stats.Reused != 0 {
		t.Errorf("首次扫描复用数应为 0，实际 %d", stats.Reused)
	}
	if len(idx.Cards) != 2 {
		t.Errorf("应有 2 张卡片，实际 %d", len(idx.Cards))
	}
	if len(idx.Zipmods) != 1 {
		t.Errorf("应有 1 个 zipmod，实际 %d", len(idx.Zipmods))
	}
	// 派生反向图
	if refs := derived.GlobalDepMap["g1"]; len(refs) != 1 {
		t.Errorf("g1 应被引用 1 次，实际 %d", len(refs))
	}
}

// TestScanSecondTimeReusesAll 二次扫描（无变更）应全部复用，零文件读取。
func TestScanSecondTimeReusesAll(t *testing.T) {
	e := newEnv(t)
	e.writeCard(t, "a.png", "g1")
	e.writeCard(t, "b.png", "g2")
	e.writeZipmod(t, "m.zipmod", "guid.m")

	s := e.newScanner()
	idx, _, _, err := s.Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("首次扫描失败: %v", err)
	}

	// 二次扫描传入旧索引
	idx2, _, stats, err := s.Scan(context.Background(), idx, nil)
	if err != nil {
		t.Fatalf("二次扫描失败: %v", err)
	}
	if stats.Parsed != 0 {
		t.Errorf("无变更时不应重新解析，实际解析 %d 个", stats.Parsed)
	}
	if stats.Reused != 3 {
		t.Errorf("无变更时应复用 3 个，实际 %d", stats.Reused)
	}
	if len(idx2.Cards) != 2 {
		t.Errorf("卡片数应保持 2，实际 %d", len(idx2.Cards))
	}
}

// TestScanOnlyParsesChanged 单文件变更后只应解析该文件。
func TestScanOnlyParsesChanged(t *testing.T) {
	e := newEnv(t)
	e.writeCard(t, "a.png", "g1")
	e.writeCard(t, "b.png", "g2")

	s := e.newScanner()
	idx, _, _, err := s.Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("首次扫描失败: %v", err)
	}

	// 修改 b.png 的内容与 mtime
	time.Sleep(10 * time.Millisecond)
	e.writeCard(t, "b.png", "g9")

	_, _, stats, err := s.Scan(context.Background(), idx, nil)
	if err != nil {
		t.Fatalf("二次扫描失败: %v", err)
	}
	if stats.Parsed != 1 {
		t.Errorf("只应解析 1 个变更文件，实际 %d", stats.Parsed)
	}
	if stats.Reused != 1 {
		t.Errorf("应复用 1 个未变更文件，实际 %d", stats.Reused)
	}
}

// TestScanDetectsRemoved 删除的文件应从索引移除。
func TestScanDetectsRemoved(t *testing.T) {
	e := newEnv(t)
	e.writeCard(t, "a.png", "g1")
	e.writeCard(t, "b.png", "g2")

	s := e.newScanner()
	idx, _, _, err := s.Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("首次扫描失败: %v", err)
	}

	if err := os.Remove(filepath.Join(e.resource, "b.png")); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	idx2, _, stats, err := s.Scan(context.Background(), idx, nil)
	if err != nil {
		t.Fatalf("二次扫描失败: %v", err)
	}
	if stats.Removed != 1 {
		t.Errorf("应检测到 1 个移除，实际 %d", stats.Removed)
	}
	if len(idx2.Cards) != 1 {
		t.Errorf("索引应剩 1 张卡片，实际 %d", len(idx2.Cards))
	}
}

// TestScanDetectsAdded 新增的文件应被解析。
func TestScanDetectsAdded(t *testing.T) {
	e := newEnv(t)
	e.writeCard(t, "a.png", "g1")

	s := e.newScanner()
	idx, _, _, err := s.Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("首次扫描失败: %v", err)
	}

	e.writeCard(t, "c.png", "g3")

	idx2, _, stats, err := s.Scan(context.Background(), idx, nil)
	if err != nil {
		t.Fatalf("二次扫描失败: %v", err)
	}
	if stats.Parsed != 1 {
		t.Errorf("应解析 1 个新增文件，实际 %d", stats.Parsed)
	}
	if len(idx2.Cards) != 2 {
		t.Errorf("索引应有 2 张卡片，实际 %d", len(idx2.Cards))
	}
}

// TestStoreRoundTrip 索引的保存与加载应保持一致。
func TestStoreRoundTrip(t *testing.T) {
	base := t.TempDir()
	store, err := NewStore(base)
	if err != nil {
		t.Fatalf("创建 Store 失败: %v", err)
	}

	idx := NewIndexData("C:\\KK")
	idx.Cards["C:\\KK\\a.png"] = &CardEntry{
		Fingerprint: Fingerprint{Size: 100, ModTime: 12345},
		Kind:        "female_card",
		Rel:         "a.png",
		GUIDs:       []string{"g1", "g2"},
	}
	idx.Zipmods["C:\\KK\\mods\\m.zipmod"] = &ZipmodEntry{
		GUID: "guid.m",
		Deps: []string{"g2"},
	}

	if err := store.Save(idx); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if _, err := os.Stat(store.Path()); err != nil {
		t.Fatalf("索引文件未生成: %v", err)
	}

	loaded, err := store.Load("C:\\KK")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if loaded == nil {
		t.Fatal("加载结果为空")
	}
	if len(loaded.Cards) != 1 {
		t.Errorf("应有 1 张卡片，实际 %d", len(loaded.Cards))
	}
	card := loaded.Cards["C:\\KK\\a.png"]
	if card == nil {
		t.Fatal("卡片条目丢失")
	}
	if card.Size != 100 || card.ModTime != 12345 {
		t.Errorf("指纹不一致: %+v", card.Fingerprint)
	}
	if len(card.GUIDs) != 2 {
		t.Errorf("GUID 列表不一致: %v", card.GUIDs)
	}
}

// TestStoreLoadRejectsDifferentRoot root 不一致时应返回 nil（触发全量重建）。
func TestStoreLoadRejectsDifferentRoot(t *testing.T) {
	base := t.TempDir()
	store, err := NewStore(base)
	if err != nil {
		t.Fatalf("创建 Store 失败: %v", err)
	}
	idx := NewIndexData("C:\\KK")
	if err := store.Save(idx); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	loaded, err := store.Load("D:\\Other")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if loaded != nil {
		t.Error("root 不一致时应返回 nil")
	}
}

// TestStoreLoadMissingFile 文件不存在时应返回 nil 且不报错。
func TestStoreLoadMissingFile(t *testing.T) {
	base := t.TempDir()
	store, err := NewStore(base)
	if err != nil {
		t.Fatalf("创建 Store 失败: %v", err)
	}
	loaded, err := store.Load("C:\\KK")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if loaded != nil {
		t.Error("文件不存在时应返回 nil")
	}
}

// TestStoreLoadCorruptedFile 文件损坏时应返回 nil 而非报错。
func TestStoreLoadCorruptedFile(t *testing.T) {
	base := t.TempDir()
	store, err := NewStore(base)
	if err != nil {
		t.Fatalf("创建 Store 失败: %v", err)
	}
	if err := os.WriteFile(store.Path(), []byte("{ this is not json"), 0644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	loaded, err := store.Load("C:\\KK")
	if err != nil {
		t.Fatalf("损坏文件不应报错: %v", err)
	}
	if loaded != nil {
		t.Error("损坏文件应返回 nil（触发全量重建）")
	}
}

// TestDeriveNoDuplicateRefs 派生图不应出现重复引用。
func TestDeriveNoDuplicateRefs(t *testing.T) {
	idx := NewIndexData("C:\\KK")
	// 同一张卡片被"移动"多次：正向索引只有一个 key，反向图也只能有一条
	idx.Cards["C:\\KK\\a.png"] = &CardEntry{GUIDs: []string{"g1"}}
	d := Derive(idx)
	if refs := d.GlobalDepMap["g1"]; len(refs) != 1 {
		t.Errorf("应只有 1 条引用，实际 %d: %v", len(refs), refs)
	}
}

// TestDeriveMissingAndExtra 派生应正确识别缺失与多余的 mod。
func TestDeriveMissingAndExtra(t *testing.T) {
	idx := NewIndexData("C:\\KK")
	idx.Cards["C:\\KK\\a.png"] = &CardEntry{GUIDs: []string{"g1"}}
	idx.Zipmods["C:\\KK\\mods\\used.zipmod"] = &ZipmodEntry{GUID: "g2"}

	d := Derive(idx)
	if len(d.MissingGUIDs) != 1 || d.MissingGUIDs[0] != "g1" {
		t.Errorf("缺失 GUID 应为 [g1]，实际 %v", d.MissingGUIDs)
	}
	if len(d.ExtraZipmods) != 1 || d.ExtraZipmods[0] != "g2" {
		t.Errorf("多余 GUID 应为 [g2]，实际 %v", d.ExtraZipmods)
	}
}

// TestHashGUIDSetStable 哈希应与顺序无关。
func TestHashGUIDSetStable(t *testing.T) {
	h1 := HashGUIDSet([]string{"a", "b", "c"})
	h2 := HashGUIDSet([]string{"c", "a", "b"})
	if h1 != h2 {
		t.Error("哈希应与顺序无关")
	}
	h3 := HashGUIDSet([]string{"a", "b"})
	if h1 == h3 {
		t.Error("不同集合的哈希应不同")
	}
}
