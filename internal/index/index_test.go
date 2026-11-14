package index

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// env 构造一个测试用的扫描环境。
type env struct {
	root     string
	resource string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "KKV114")
	e := &env{
		root:     root,
		resource: filepath.Join(root, "UserData", "chara", "female"),
	}
	if err := os.MkdirAll(e.resource, 0755); err != nil {
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

func (e *env) newScanner() *Scanner {
	return &Scanner{
		Root: e.root,
		ResourceRoots: map[string]string{
			"female_card": e.resource,
		},
	}
}

// TestScanFirstTimeIndexesAll 首次扫描应索引全部卡片（只 stat，不解析内容）。
func TestScanFirstTimeIndexesAll(t *testing.T) {
	e := newEnv(t)
	e.writeCard(t, "a.png", "content-a")
	e.writeCard(t, "b.png", "content-b")
	e.writeCard(t, "sub/c.jpg", "content-c")

	s := e.newScanner()
	idx, stats, err := s.Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if stats.Parsed != 3 {
		t.Errorf("首次应更新 3 个条目，实际 %d", stats.Parsed)
	}
	if stats.Reused != 0 {
		t.Errorf("首次复用数应为 0，实际 %d", stats.Reused)
	}
	if len(idx.Cards) != 3 {
		t.Fatalf("应有 3 张卡片，实际 %d", len(idx.Cards))
	}
	entry := idx.Cards[filepath.Join(e.resource, "sub", "c.jpg")]
	if entry == nil {
		t.Fatal("子目录卡片未被索引")
	}
	if entry.Kind != "female_card" {
		t.Errorf("Kind 应为 female_card，实际 %q", entry.Kind)
	}
	if entry.Rel != "sub/c.jpg" {
		t.Errorf("Rel 应为 sub/c.jpg，实际 %q", entry.Rel)
	}
}

// TestScanSecondTimeReusesAll 二次扫描应全部复用（Parsed == 0）。
func TestScanSecondTimeReusesAll(t *testing.T) {
	e := newEnv(t)
	e.writeCard(t, "a.png", "a")
	e.writeCard(t, "b.png", "b")

	s := e.newScanner()
	idx, _, err := s.Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("首次扫描失败: %v", err)
	}

	_, stats, err := s.Scan(context.Background(), idx, nil)
	if err != nil {
		t.Fatalf("二次扫描失败: %v", err)
	}
	if stats.Reused != 2 {
		t.Errorf("应复用 2 个条目，实际 %d", stats.Reused)
	}
	if stats.Parsed != 0 {
		t.Errorf("热扫描不应更新任何条目（0 次解析），实际 %d", stats.Parsed)
	}
}

// TestScanOnlyIndexesChanged 只更新变化的文件。
func TestScanOnlyIndexesChanged(t *testing.T) {
	e := newEnv(t)
	e.writeCard(t, "a.png", "a")
	e.writeCard(t, "b.png", "b")

	s := e.newScanner()
	idx, _, err := s.Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("首次扫描失败: %v", err)
	}

	// 改变一个文件的大小，指纹必变
	e.writeCard(t, "a.png", "changed-content-longer")

	idx2, stats, err := s.Scan(context.Background(), idx, nil)
	if err != nil {
		t.Fatalf("增量扫描失败: %v", err)
	}
	if stats.Parsed != 1 {
		t.Errorf("应只更新 1 个条目，实际 %d", stats.Parsed)
	}
	if stats.Reused != 1 {
		t.Errorf("应复用 1 个条目，实际 %d", stats.Reused)
	}
	if len(idx2.Cards) != 2 {
		t.Errorf("卡片总数应保持 2，实际 %d", len(idx2.Cards))
	}
}

// TestScanDetectsRemoved 删除检测。
func TestScanDetectsRemoved(t *testing.T) {
	e := newEnv(t)
	p := e.writeCard(t, "a.png", "a")
	e.writeCard(t, "b.png", "b")

	s := e.newScanner()
	idx, _, err := s.Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("首次扫描失败: %v", err)
	}

	if err := os.Remove(p); err != nil {
		t.Fatalf("删除文件失败: %v", err)
	}
	idx2, stats, err := s.Scan(context.Background(), idx, nil)
	if err != nil {
		t.Fatalf("增量扫描失败: %v", err)
	}
	if stats.Removed != 1 {
		t.Errorf("应移除 1 个条目，实际 %d", stats.Removed)
	}
	if len(idx2.Cards) != 1 {
		t.Errorf("应剩 1 张卡片，实际 %d", len(idx2.Cards))
	}
}

// TestScanDetectsAdded 新增检测。
func TestScanDetectsAdded(t *testing.T) {
	e := newEnv(t)
	e.writeCard(t, "a.png", "a")

	s := e.newScanner()
	idx, _, err := s.Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("首次扫描失败: %v", err)
	}

	e.writeCard(t, "b.png", "b")
	idx2, stats, err := s.Scan(context.Background(), idx, nil)
	if err != nil {
		t.Fatalf("增量扫描失败: %v", err)
	}
	if stats.Parsed != 1 {
		t.Errorf("应新增 1 个条目，实际 %d", stats.Parsed)
	}
	if len(idx2.Cards) != 2 {
		t.Errorf("应有 2 张卡片，实际 %d", len(idx2.Cards))
	}
}

// TestScanCollectsJPGCards JPG/JPEG 也应被索引（v2.3 支持）。
func TestScanCollectsJPGCards(t *testing.T) {
	e := newEnv(t)
	e.writeCard(t, "a.jpg", "jpg")
	e.writeCard(t, "b.jpeg", "jpeg")
	e.writeCard(t, "c.png", "png")

	s := e.newScanner()
	idx, _, err := s.Scan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(idx.Cards) != 3 {
		t.Errorf("应索引 3 张卡（png/jpg/jpeg），实际 %d", len(idx.Cards))
	}
}

// TestScanProgressOnlyResource 进度回调只发 resource。
func TestScanProgressOnlyResource(t *testing.T) {
	e := newEnv(t)
	e.writeCard(t, "a.png", "a")
	e.writeCard(t, "b.png", "b")

	s := e.newScanner()
	var phases []string
	var lastDone, lastTotal int
	progress := func(phase string, done, total int) {
		phases = append(phases, phase)
		lastDone, lastTotal = done, total
	}
	if _, _, err := s.Scan(context.Background(), nil, progress); err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(phases) == 0 {
		t.Fatal("进度回调未被调用")
	}
	for _, p := range phases {
		if p != "resource" {
			t.Errorf("进度阶段应恒为 resource，实际 %q", p)
		}
	}
	if lastDone != 2 || lastTotal != 2 {
		t.Errorf("最终进度应为 2/2，实际 %d/%d", lastDone, lastTotal)
	}
}

// -------------------- Store --------------------

func TestStoreRoundTrip(t *testing.T) {
	delRoot := t.TempDir()
	store, err := NewStore(delRoot)
	if err != nil {
		t.Fatalf("NewStore 失败: %v", err)
	}
	idx := NewIndexData("C:/KK")
	idx.Cards["C:/KK/a.png"] = &CardEntry{
		Fingerprint: FingerprintOf(10, time.Unix(0, 100)),
		Kind:        "female_card",
		Rel:         "a.png",
	}
	if err := store.Save(idx); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	lr, err := store.Load("C:/KK")
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if lr.Index == nil {
		t.Fatal("应加载到索引")
	}
	if lr.Index.SchemaVersion != SchemaVersion {
		t.Errorf("schema 应为 %d，实际 %d", SchemaVersion, lr.Index.SchemaVersion)
	}
	if len(lr.Index.Cards) != 1 {
		t.Errorf("应有 1 张卡，实际 %d", len(lr.Index.Cards))
	}
	card := lr.Index.Cards["C:/KK/a.png"]
	if card == nil {
		t.Fatal("未找到 a.png")
	}
	if card.Size != 10 || card.ModTime != 100 {
		t.Errorf("指纹未往返：%+v", card.Fingerprint)
	}
	if card.Kind != "female_card" || card.Rel != "a.png" {
		t.Errorf("Kind/Rel 未往返：kind=%q rel=%q", card.Kind, card.Rel)
	}

	// 磁盘上必须是紧凑编码（fp 字符串 + 单字母字段），否则 2 万卡会超 2MB。
	data, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"mtime"`)) {
		t.Error("索引仍使用旧的 size/mtime 对象编码")
	}
	if !bytes.Contains(data, []byte(`"fp":"10:100"`)) {
		t.Errorf("索引未使用紧凑指纹编码：%s", data)
	}
}

func TestStoreLoadRejectsDifferentRoot(t *testing.T) {
	delRoot := t.TempDir()
	store, _ := NewStore(delRoot)
	if err := store.Save(NewIndexData("C:/A")); err != nil {
		t.Fatal(err)
	}
	lr, err := store.Load("C:/B")
	if err != nil {
		t.Fatal(err)
	}
	if lr.Index != nil {
		t.Error("root 不同不应复用索引")
	}
}

func TestStoreLoadMissingFile(t *testing.T) {
	store, _ := NewStore(t.TempDir())
	lr, err := store.Load("C:/KK")
	if err != nil {
		t.Fatalf("缺文件不应报错: %v", err)
	}
	if lr.Index != nil || lr.Upgraded {
		t.Error("缺文件应返回空结果")
	}
}

func TestStoreLoadCorruptedFile(t *testing.T) {
	delRoot := t.TempDir()
	store, _ := NewStore(delRoot)
	if err := os.WriteFile(store.Path(), []byte("{not-json"), 0644); err != nil {
		t.Fatal(err)
	}
	lr, err := store.Load("C:/KK")
	if err != nil {
		t.Fatalf("损坏文件不应报错: %v", err)
	}
	if lr.Index != nil {
		t.Error("损坏文件应触发全量重建")
	}
}

// TestStoreLoadV1Migrates v1 旧索引不得 panic、不复用、不被删除。
func TestStoreLoadV1Migrates(t *testing.T) {
	delRoot := t.TempDir()
	store, _ := NewStore(delRoot)

	// 一份典型的 v1 索引：含 zipmods / plugins / pluginDepMap / guids
	v1 := `{
	  "schemaVersion": 1,
	  "root": "C:/KK",
	  "scannedAt": 1,
	  "guidSetHash": "abc",
	  "zipmods": {"C:/KK/mods/a.zipmod": {"size":1,"mtime":2,"guid":"g1","deps":["g2"],"rel":"a.zipmod"}},
	  "cards": {"C:/KK/a.png": {"size":1,"mtime":2,"kind":"female_card","rel":"a.png","guids":["g1"],"parseErr":""}},
	  "plugins": {"C:/KK/BepInEx/plugins/a.dll": {"size":1,"mtime":2,"rel":"a.dll","guids":["g1"]}},
	  "pluginDepMap": {"g1":["a.dll"]},
	  "isolatedZipmods": {},
	  "originRel": {}
	}`
	if err := os.WriteFile(store.Path(), []byte(v1), 0644); err != nil {
		t.Fatal(err)
	}

	lr, err := store.Load("C:/KK")
	if err != nil {
		t.Fatalf("v1 索引不应报错: %v", err)
	}
	if lr.Index != nil {
		t.Error("v1 索引不应被复用")
	}
	if !lr.Upgraded {
		t.Error("应标记 Upgraded=true")
	}
	if lr.OldVersion != 1 {
		t.Errorf("OldVersion 应为 1，实际 %d", lr.OldVersion)
	}
	// 旧文件不得被删除
	if _, err := os.Stat(store.Path()); err != nil {
		t.Errorf("旧索引文件不得被删除: %v", err)
	}

	// 全量重建后覆盖写为 v2
	idx := NewIndexData("C:/KK")
	idx.Cards["C:/KK/a.png"] = &CardEntry{
		Fingerprint: FingerprintOf(1, time.Unix(0, 2)),
		Kind:        "female_card",
		Rel:         "a.png",
	}
	if err := store.Save(idx); err != nil {
		t.Fatalf("Save v2 失败: %v", err)
	}
	lr2, err := store.Load("C:/KK")
	if err != nil {
		t.Fatal(err)
	}
	if lr2.Index == nil || lr2.Index.SchemaVersion != 2 {
		t.Errorf("覆盖写后应为可用的 v2 索引，实际 %+v", lr2)
	}
}

// TestStoreLoadLegacyV2Entry 早期 v2 索引（size/mtime 对象形式）仍应可复用。
//
// 编码压缩只改磁盘格式，不应让已存在的 v2 索引整份作废。
func TestStoreLoadLegacyV2Entry(t *testing.T) {
	delRoot := t.TempDir()
	store, _ := NewStore(delRoot)

	legacy := `{"schemaVersion":2,"root":"C:/KK","scannedAt":1,` +
		`"cards":{"C:/KK/a.png":{"size":10,"mtime":100,"kind":"female_card","rel":"a.png"}}}`
	if err := os.WriteFile(store.Path(), []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}

	lr, err := store.Load("C:/KK")
	if err != nil {
		t.Fatalf("早期 v2 索引不应报错: %v", err)
	}
	if lr.Index == nil {
		t.Fatal("早期 v2 索引应可复用，不应全量重建")
	}
	card := lr.Index.Cards["C:/KK/a.png"]
	if card == nil {
		t.Fatal("未找到 a.png")
	}
	if card.Size != 10 || card.ModTime != 100 || card.Kind != "female_card" || card.Rel != "a.png" {
		t.Errorf("早期 v2 条目解析错误: %+v", card)
	}
}
