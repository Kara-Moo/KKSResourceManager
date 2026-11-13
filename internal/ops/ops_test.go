package ops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMkdirUniqueName 新建文件夹时同名自动加 (2)。
func TestMkdirUniqueName(t *testing.T) {
	dir := t.TempDir()
	store, err := NewTrashStore(filepath.Join(dir, "DELCARDS"))
	if err != nil {
		t.Fatalf("创建 TrashStore 失败: %v", err)
	}

	r1 := store.Mkdir(nil, dir, "新建文件夹")
	if !r1.OK {
		t.Fatalf("第一次创建失败: %s", r1.Message)
	}
	if r1.Name != "新建文件夹" {
		t.Errorf("第一次应叫「新建文件夹」，实际 %q", r1.Name)
	}

	r2 := store.Mkdir(nil, dir, "新建文件夹")
	if !r2.OK {
		t.Fatalf("第二次创建失败: %s", r2.Message)
	}
	if r2.Name != "新建文件夹 (2)" {
		t.Errorf("第二次应叫「新建文件夹 (2)」，实际 %q", r2.Name)
	}

	r3 := store.Mkdir(nil, dir, "新建文件夹")
	if r3.Name != "新建文件夹 (3)" {
		t.Errorf("第三次应叫「新建文件夹 (3)」，实际 %q", r3.Name)
	}
}

// TestDeleteRestoreRoundTrip 删除后还原应回到原位（§11.4）。
func TestDeleteRestoreRoundTrip(t *testing.T) {
	base := t.TempDir()
	delRoot := filepath.Join(base, "DELCARDS")
	store, err := NewTrashStore(delRoot)
	if err != nil {
		t.Fatalf("创建 TrashStore 失败: %v", err)
	}

	// 源文件
	srcDir := filepath.Join(base, "KKV114", "UserData", "chara", "female")
	src := filepath.Join(srcDir, "card.png")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(src, []byte("content"), 0644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	specOf := func(abs string) (string, string, bool) { return "female_card", base, true }

	// 计划
	plan := store.PlanDelete([]string{src}, specOf)
	if len(plan.Items) != 1 {
		t.Fatalf("应有 1 个待删项，实际 %d", len(plan.Items))
	}
	if len(plan.Conflicts) != 0 {
		t.Fatalf("不应有冲突: %v", plan.Conflicts)
	}

	// 执行
	moved, errs := store.CommitDelete(nil, plan)
	if moved != 1 {
		t.Fatalf("应移动 1 项，实际 %d，错误 %v", moved, errs)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("删除后原位置不应有文件")
	}
	// 文件应在 DELED 里
	trashAbs := filepath.Join(delRoot, DeletedDirName, filepath.FromSlash(plan.Items[0].TrashRel))
	if _, err := os.Stat(trashAbs); err != nil {
		t.Errorf("文件应在回收站中: %v", err)
	}
	// 元数据记录了原路径
	meta, err := store.Load()
	if err != nil {
		t.Fatalf("读取元数据失败: %v", err)
	}
	entry, ok := meta[plan.Items[0].TrashRel]
	if !ok {
		t.Fatal("元数据缺少记录")
	}
	if entry.OriginalAbs != src {
		t.Errorf("元数据应记录原路径 %s，实际 %s", src, entry.OriginalAbs)
	}

	// 还原
	res := store.Restore(nil, []string{plan.Items[0].TrashRel})
	if res.Restored != 1 {
		t.Fatalf("应还原 1 项，实际 %d，冲突 %v", res.Restored, res.Conflicts)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("还原后原路径应有文件: %v", err)
	}
	if string(data) != "content" {
		t.Errorf("内容应一致，实际 %q", string(data))
	}
	// 元数据应被清掉
	meta, _ = store.Load()
	if len(meta) != 0 {
		t.Errorf("还原后元数据应清空，实际 %d 条", len(meta))
	}
}

// TestRestoreConflict 目标已存在时应记为冲突而非覆盖。
func TestRestoreConflict(t *testing.T) {
	base := t.TempDir()
	store, _ := NewTrashStore(filepath.Join(base, "DELCARDS"))

	src := filepath.Join(base, "a.png")
	os.WriteFile(src, []byte("original"), 0644)

	specOf := func(abs string) (string, string, bool) { return "female_card", base, true }
	plan := store.PlanDelete([]string{src}, specOf)
	store.CommitDelete(nil, plan)

	// 目标位置被别的文件占了
	os.WriteFile(src, []byte("occupant"), 0644)

	res := store.Restore(nil, []string{plan.Items[0].TrashRel})
	if res.Restored != 0 {
		t.Errorf("目标已存在时不应还原，实际 %d", res.Restored)
	}
	if len(res.Conflicts) == 0 {
		t.Error("应记录冲突")
	}
	// 占用文件不应被覆盖
	data, _ := os.ReadFile(src)
	if string(data) != "occupant" {
		t.Errorf("占用文件不应被覆盖，实际 %q", string(data))
	}
}

// TestPurgeRemovesPermanently 彻底删除应真删文件并清元数据。
//
// 注：名称二次确认在 Manager 层（crudops.go）做，
// 这里的 Purge 是底层实现。
func TestPurgeRemovesPermanently(t *testing.T) {
	base := t.TempDir()
	store, _ := NewTrashStore(filepath.Join(base, "DELCARDS"))

	src := filepath.Join(base, "a.png")
	os.WriteFile(src, []byte("x"), 0644)
	specOf := func(abs string) (string, string, bool) { return "female_card", base, true }
	plan := store.PlanDelete([]string{src}, specOf)
	store.CommitDelete(nil, plan)

	trashAbs := filepath.Join(store.Dir(), filepath.FromSlash(plan.Items[0].TrashRel))
	if _, err := os.Stat(trashAbs); err != nil {
		t.Fatalf("回收站应有文件: %v", err)
	}

	res := store.Purge([]string{plan.Items[0].TrashRel})
	if res.Purged != 1 {
		t.Errorf("应彻底删除 1 项，实际 %d，错误 %v", res.Purged, res.Errors)
	}
	if _, err := os.Stat(trashAbs); !os.IsNotExist(err) {
		t.Error("彻底删除后文件应不存在")
	}
	meta, _ := store.Load()
	if len(meta) != 0 {
		t.Errorf("彻底删除后元数据应清空，实际 %d 条", len(meta))
	}
}

// TestDeleteFolderMovesWholeTree 删除文件夹应整棵子树一次移动。
func TestDeleteFolderMovesWholeTree(t *testing.T) {
	base := t.TempDir()
	store, _ := NewTrashStore(filepath.Join(base, "DELCARDS"))

	dir := filepath.Join(base, "group")
	os.MkdirAll(filepath.Join(dir, "sub", "deep"), 0755)
	os.WriteFile(filepath.Join(dir, "sub", "deep", "x.png"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(dir, "a.png"), []byte("a"), 0644)

	specOf := func(abs string) (string, string, bool) { return "female_card", base, true }
	plan := store.PlanDelete([]string{dir}, specOf)
	if len(plan.Items) != 1 {
		t.Fatalf("应只有 1 个计划项（整棵树一次移动），实际 %d", len(plan.Items))
	}

	moved, errs := store.CommitDelete(nil, plan)
	if moved != 1 {
		t.Fatalf("应移动 1 项，实际 %d，错误 %v", moved, errs)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("删除后原目录不应存在")
	}
	// 回收站里应保留完整子树
	trashAbs := filepath.Join(store.Dir(), filepath.FromSlash(plan.Items[0].TrashRel))
	if _, err := os.Stat(filepath.Join(trashAbs, "sub", "deep", "x.png")); err != nil {
		t.Errorf("回收站应保留完整子树: %v", err)
	}
}

// TestDeleteOutsideScope 目录外的条目应记为冲突。
func TestDeleteOutsideScope(t *testing.T) {
	base := t.TempDir()
	store, _ := NewTrashStore(filepath.Join(base, "DELCARDS"))

	outside := filepath.Join(t.TempDir(), "outside.png")
	os.WriteFile(outside, []byte("x"), 0644)

	specOf := func(abs string) (string, string, bool) { return "", "", false }
	plan := store.PlanDelete([]string{outside}, specOf)
	if len(plan.Items) != 0 {
		t.Error("范围外的条目不应进入计划")
	}
	if len(plan.Conflicts) != 1 {
		t.Errorf("应有 1 个冲突，实际 %d", len(plan.Conflicts))
	}
}

// TestPlanDeletePreservesSubdirs 回收站必须保留原目录结构（v2.2 FIX-07）。
//
// 这是回归测试：早前 PlanDelete 对单文件用 UniqueTrashPath(spec, name)，
// 只取文件名，删两个不同子目录里的同名 2.png 会变成 2.png / 2__2.png。
func TestPlanDeletePreservesSubdirs(t *testing.T) {
	base := t.TempDir()
	store, _ := NewTrashStore(filepath.Join(base, "DELCARDS"))

	root := filepath.Join(base, "KKV114", "UserData", "chara", "female")
	dirA := filepath.Join(root, "22.7.27")
	dirB := filepath.Join(root, "22.7.28")
	if err := os.MkdirAll(dirA, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dirB, 0755); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(dirA, "2.png")
	b := filepath.Join(dirB, "2.png")
	if err := os.WriteFile(a, []byte("AAA"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("BBB"), 0644); err != nil {
		t.Fatal(err)
	}

	specOf := func(abs string) (string, string, bool) {
		return "female_card", root, true
	}
	plan := store.PlanDelete([]string{a, b}, specOf)
	if len(plan.Items) != 2 {
		t.Fatalf("应有 2 个待删项，实际 %d；冲突 %v", len(plan.Items), plan.Conflicts)
	}

	want := map[string]bool{
		"female_card/22.7.27/2.png": false,
		"female_card/22.7.28/2.png": false,
	}
	for _, item := range plan.Items {
		if _, ok := want[item.TrashRel]; !ok {
			t.Errorf("trashRel 丢失了子目录或意外加了后缀：%q", item.TrashRel)
			continue
		}
		want[item.TrashRel] = true
		if strings.Contains(item.TrashRel, "__2") {
			t.Errorf("不该出现 __N 后缀（两个文件本就不冲突）：%q", item.TrashRel)
		}
	}
	for rel, seen := range want {
		if !seen {
			t.Errorf("缺少期望的路径 %q", rel)
		}
	}

	// 执行删除 → 两条各自在自己的子目录
	moved, errs := store.CommitDelete(nil, plan)
	if moved != 2 {
		t.Fatalf("应移动 2 项，实际 %d，错误 %v", moved, errs)
	}
	for rel := range want {
		abs := filepath.Join(store.Dir(), filepath.FromSlash(rel))
		if _, err := os.Stat(abs); err != nil {
			t.Errorf("回收站缺少 %s: %v", rel, err)
		}
	}

	// 还原 → 两条都回到原位
	rels := []string{"female_card/22.7.27/2.png", "female_card/22.7.28/2.png"}
	res := store.Restore(nil, rels)
	if res.Restored != 2 {
		t.Errorf("应还原 2 项，实际 %d，冲突 %v", res.Restored, res.Conflicts)
	}
	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("未回到原位 %s: %v", p, err)
		}
	}
	meta, _ := store.Load()
	if len(meta) != 0 {
		t.Errorf("还原后元数据应清空，实际 %d 条", len(meta))
	}
}
