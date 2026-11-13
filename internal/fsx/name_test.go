package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

// TestValidateNameRejectsIllegalChars 非法字符应被拒绝。
func TestValidateNameRejectsIllegalChars(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{`a<b`, `a>b`, `a:b`, `a"b`, `a/b`, `a\b`, `a|b`, `a?b`, `a*b`} {
		err := ValidateName(dir, name, "")
		if err == nil {
			t.Errorf("「%s」应被判为非法", name)
			continue
		}
		if CodeOf(err) != ErrNameInvalid {
			t.Errorf("「%s」的错误码应为 NAME_INVALID，实际 %s", name, CodeOf(err))
		}
	}
}

// TestValidateNameRejectsTrailingSpaceOrDot 结尾空格/点应被拒绝。
func TestValidateNameRejectsTrailingSpaceOrDot(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"name ", "name.", "name  "} {
		if err := ValidateName(dir, name, ""); err == nil {
			t.Errorf("「%s」应被拒绝（Windows 会静默改名）", name)
		}
	}
}

// TestValidateNameRejectsTooLong 超长名称应被拒绝。
func TestValidateNameRejectsTooLong(t *testing.T) {
	dir := t.TempDir()
	long := ""
	for i := 0; i < MaxNameLength+1; i++ {
		long += "a"
	}
	if err := ValidateName(dir, long, ""); err == nil {
		t.Error("超长名称应被拒绝")
	}
}

// TestValidateNameRejectsReserved 保留设备名应被拒绝。
func TestValidateNameRejectsReserved(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"CON", "con", "PRN", "AUX", "NUL", "COM1", "LPT9"} {
		if err := ValidateName(dir, name, ""); err == nil {
			t.Errorf("「%s」是保留名，应被拒绝", name)
		}
	}
	// 带扩展名的形式也要拒绝
	if err := ValidateName(dir, "CON.txt", ""); err == nil {
		t.Error("「CON.txt」应被拒绝")
	}
}

// TestValidateNameRejectsEmpty 空名应被拒绝。
func TestValidateNameRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"", "   ", "\t"} {
		err := ValidateName(dir, name, "")
		if err == nil {
			t.Errorf("「%q」应被拒绝", name)
			continue
		}
		if CodeOf(err) != ErrEmptyName {
			t.Errorf("「%q」的错误码应为 NAME_EMPTY，实际 %s", name, CodeOf(err))
		}
	}
}

// TestValidateNameRejectsDuplicate 重名应被拒绝（不区分大小写）。
func TestValidateNameRejectsDuplicate(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "existing.png"), []byte("x"), 0644)

	err := ValidateName(dir, "existing.png", "")
	if err == nil {
		t.Fatal("重名应被拒绝")
	}
	if CodeOf(err) != ErrNameExists {
		t.Errorf("错误码应为 NAME_EXISTS，实际 %s", CodeOf(err))
	}

	// 大小写不同也算重名
	err = ValidateName(dir, "EXISTING.PNG", "")
	if err == nil {
		t.Error("大小写不同的重名也应被拒绝")
	}

	// 重命名成自己不算重名
	if err := ValidateName(dir, "existing.png", "existing.png"); err != nil {
		t.Errorf("重命名成同名不应报错: %v", err)
	}
}

// TestUniqueFolderName 冲突时自动加序号。
func TestUniqueFolderName(t *testing.T) {
	dir := t.TempDir()

	name, err := UniqueFolderName(dir, "新建文件夹")
	if err != nil {
		t.Fatalf("生成名称失败: %v", err)
	}
	if name != "新建文件夹" {
		t.Errorf("首次应无后缀，实际 %q", name)
	}
	os.MkdirAll(filepath.Join(dir, "新建文件夹"), 0755)

	name, err = UniqueFolderName(dir, "新建文件夹")
	if err != nil {
		t.Fatalf("生成名称失败: %v", err)
	}
	if name != "新建文件夹 (2)" {
		t.Errorf("冲突时应为 (2)，实际 %q", name)
	}
	os.MkdirAll(filepath.Join(dir, "新建文件夹 (2)"), 0755)

	name, _ = UniqueFolderName(dir, "新建文件夹")
	if name != "新建文件夹 (3)" {
		t.Errorf("应递增到 (3)，实际 %q", name)
	}
}

// TestExtensionChanged 扩展名变化检测。
func TestExtensionChanged(t *testing.T) {
	if ExtensionChanged("a.png", "b.png") {
		t.Error("扩展名未变不应报告变化")
	}
	if !ExtensionChanged("a.png", "b.txt") {
		t.Error("扩展名变化应被检测到")
	}
	if ExtensionChanged("a.png", "b.PNG") {
		t.Error("仅大小写变化不算扩展名变化")
	}
}

// TestPathWithin 路径范围检查。
func TestPathWithin(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "a", "b.txt")
	outside := filepath.Join(t.TempDir(), "c.txt")

	if !PathWithin(inside, base) {
		t.Error("内部路径应通过")
	}
	if PathWithin(outside, base) {
		t.Error("外部路径不应通过")
	}
}
