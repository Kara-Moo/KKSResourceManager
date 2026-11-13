// Package fsx 提供文件系统的安全操作与名称校验。
//
// 重构规格 §6.1 / §6.2 的名称校验规则集中在这里，
// 前后端各做一次（前端即时反馈，后端最终把关）。
package fsx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MaxNameLength 名称最大长度（字符数）。
const MaxNameLength = 100

// DefaultFolderName 新建文件夹的默认名。
const DefaultFolderName = "新建文件夹"

// 错误码（结构化，前端据此高亮对应输入框）。
const (
	ErrNameInvalid = "NAME_INVALID"   // 含非法字符 / 结尾是空格或点 / 超长 / 保留名
	ErrNameExists  = "NAME_EXISTS"    // 同目录已存在同名
	ErrIOError     = "IO_ERROR"       // 磁盘操作失败
	ErrEmptyName   = "NAME_EMPTY"     // 空或全空白
)

// Error 带错误码的校验错误。
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// newErr 构造带错误码的错误。
func newErr(code, format string, args ...interface{}) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// CodeOf 从错误中提取错误码（非校验错误返回 IO_ERROR）。
func CodeOf(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ErrIOError
}

// reservedNames Windows 保留设备名（不区分大小写）。
var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// illegalChars Windows 文件名非法字符。
const illegalChars = `<>:"/\|?*`

// ValidateName 校验一个条目名称。
//
// dir      所在目录（用于检查重名）
// name     待校验的名称
// oldName  重命名时的原名称（与 name 相同则不算重名）
func ValidateName(dir, name, oldName string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return newErr(ErrEmptyName, "名称不能为空")
	}
	// 结尾空格或点：Windows 会静默改名，必须拒绝
	if strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".") {
		return newErr(ErrNameInvalid, "名称不能以空格或「.」结尾")
	}
	if len([]rune(name)) > MaxNameLength {
		return newErr(ErrNameInvalid, "名称过长（最多 %d 字符）", MaxNameLength)
	}
	for _, c := range name {
		if strings.ContainsRune(illegalChars, c) {
			return newErr(ErrNameInvalid, "名称不能包含以下字符：%s", illegalChars)
		}
		if c < 0x20 {
			return newErr(ErrNameInvalid, "名称不能包含控制字符")
		}
	}
	// 保留设备名（含扩展名的形式，如 CON.txt）
	base := name
	if i := strings.IndexByte(name, '.'); i > 0 {
		base = name[:i]
	}
	if reservedNames[strings.ToUpper(base)] {
		return newErr(ErrNameInvalid, "「%s」是系统保留名称", base)
	}
	// 重名检查（不区分大小写）
	if oldName == "" || !strings.EqualFold(name, oldName) {
		target := filepath.Join(dir, name)
		if _, err := os.Stat(target); err == nil {
			return newErr(ErrNameExists, "已存在同名项")
		}
	}
	return nil
}

// UniqueFolderName 生成不冲突的文件夹名。
//
// 默认名冲突时依次尝试「新建文件夹 (2)」「新建文件夹 (3)」…，
// 探测上限 99，超限报错。
func UniqueFolderName(dir, base string) (string, error) {
	if base == "" {
		base = DefaultFolderName
	}
	if _, err := os.Stat(filepath.Join(dir, base)); os.IsNotExist(err) {
		return base, nil
	}
	for i := 2; i <= 99; i++ {
		candidate := fmt.Sprintf("%s (%d)", base, i)
		if _, err := os.Stat(filepath.Join(dir, candidate)); os.IsNotExist(err) {
			return candidate, nil
		}
	}
	return "", newErr(ErrNameExists, "同名项过多，无法自动重命名")
}

// HasSameName 判断目录中是否已存在同名项（不区分大小写）。
func HasSameName(dir, name string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.EqualFold(e.Name(), name) {
			return true
		}
	}
	return false
}

// ExtensionChanged 判断改名后扩展名是否变化。
func ExtensionChanged(oldName, newName string) bool {
	return !strings.EqualFold(filepath.Ext(oldName), filepath.Ext(newName))
}

// PathWithin 判断 path 是否位于 root 之内。
func PathWithin(path, root string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
