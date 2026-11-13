//go:build !windows

// diskfree_other.go —— 非 Windows 平台的磁盘剩余空间查询
//
// 当前实现不做空间预检（跨平台获取可用空间的 API 各不相同），
// 但保留函数签名，保证调用方无需分支处理。

package main

// checkFreeSpace 非 Windows 平台不做空间预检，直接返回 nil。
func checkFreeSpace(dir string, required int64) error {
	return nil
}
