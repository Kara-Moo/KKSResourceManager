package main

// derive_test.go —— 派生索引测试（对应 §3.6 依赖图重复累积缺陷）
//
// 历史缺陷：afterResourceMoved 用增量 append 维护反向图，
// 同一文件被移动 N 次就会积累 N 条重复引用，
// 导致 zipmodStillReferenced 统计虚高、本该可隔离的 zipmod 隔离不掉。
//
// 修法：反向图永远由正向数据 derive() 现算。

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNormalizePathAcceptsBothForms 验证隐藏路径归一化（对应 §3.3）。
//
// 绝对路径与相对路径都必须能匹配到同一条隐藏记录。
func TestNormalizePathAcceptsBothForms(t *testing.T) {
	env := newTestEnv(t)
	inv := env.manager.invisible

	target := filepath.Join(env.femaleCard("hiddenFolder"))
	writeFile(t, filepath.Join(target, "sub"), "")
	os.RemoveAll(filepath.Join(target, "sub"))

	if res := inv.HideFolders([]string{target}, SideKKS); res.Hidden != 1 {
		t.Fatalf("隐藏失败: %v", res.Logs)
	}

	abs := target
	rel := "UserData/chara/female/hiddenFolder"

	// 绝对路径
	r1, side1, ok1 := inv.normalizePath(abs)
	if !ok1 || r1 != rel || side1 != SideKKS {
		t.Errorf("绝对路径归一化错误: rel=%q side=%q ok=%v", r1, side1, ok1)
	}
	// 相对路径
	r2, side2, ok2 := inv.normalizePath(rel)
	if !ok2 || r2 != rel || side2 != SideKKS {
		t.Errorf("相对路径归一化错误: rel=%q side=%q ok=%v", r2, side2, ok2)
	}
}

// TestRestoreFoldersAcceptsAbsolutePath 验证用绝对路径也能恢复（§3.3 的核心回归测试）。
func TestRestoreFoldersAcceptsAbsolutePath(t *testing.T) {
	env := newTestEnv(t)
	inv := env.manager.invisible

	target := filepath.Join(env.femaleCard("restoreMe"))
	writeFile(t, filepath.Join(target, "sub"), "")
	os.RemoveAll(filepath.Join(target, "sub"))

	if res := inv.HideFolders([]string{target}, SideKKS); res.Hidden != 1 {
		t.Fatalf("隐藏失败: %v", res.Logs)
	}

	// 传绝对路径（这是前端原先的做法，会导致「未找到隐藏记录」）
	res := inv.RestoreFolders([]string{target})
	if res.Restored != 1 {
		t.Fatalf("用绝对路径应能恢复，实际恢复 %d 个，日志 %v", res.Restored, res.Logs)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("目录未被重建: %v", err)
	}
}
