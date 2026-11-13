//go:build windows

// diskfree_windows.go —— Windows 磁盘剩余空间查询
//
// 跨盘复制前必须检查目标盘剩余空间，否则大文件复制到一半失败，
// 会留下半成品文件并浪费大量 IO。

package main

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// checkFreeSpace 检查目标目录所在磁盘的剩余空间。
//
// required 为需要的字节数；调用方应传入源大小的 1.1 倍作为安全余量。
func checkFreeSpace(dir string, required int64) error {
	if required <= 0 {
		return nil
	}
	path, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}

	var freeToCaller, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(path, &freeToCaller, &totalBytes, &totalFree); err != nil {
		// 查询失败不阻断操作（部分虚拟盘不支持该 API）
		return nil
	}
	if int64(freeToCaller) < required {
		return fmt.Errorf("目标磁盘剩余空间不足：需要 %.1f MB，可用 %.1f MB",
			float64(required)/(1024*1024), float64(freeToCaller)/(1024*1024))
	}
	return nil
}
