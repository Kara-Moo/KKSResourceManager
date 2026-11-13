package main

// guard.go —— zipmod 隔离判定的统一入口
//
// 重构规格 §8.6 的强制要求：
//
//	所有会移动 zipmod 的代码路径（Isolate、Paste、一键隔离多余 zipmod、未来的拖拽）
//	**必须**只调用这一个函数做判定。禁止在别处再写一份「检查逻辑」。
//
// 这样做的收益：
//   - 三道防线（卡片依赖 / 插件依赖 / mod 间依赖）只需维护一处；
//   - 日志文案统一，便于用户搜索与测试断言；
//   - 不会出现「隔离路径查了三道防线、粘贴路径只查了一道」的不一致。

import (
	"fmt"
	"strings"

	"kksresourcemanager/internal/plugin"
)

// zipmodGuard 构造当前的依赖保护判定器。
//
// 数据全部来自内存索引，**绝不扫盘**（红线 R5）。
func (m *Manager) zipmodGuard() *plugin.Guard {
	return &plugin.Guard{
		GlobalDepMap: m.GlobalDepMap,
		PluginDepMap: m.PluginDepMap,
		ModDeps:      m.ModDeps,
		ZipmodDB:     m.ZipmodDB,
	}
}

// IsZipmodSafeToMove 判定某个 zipmod 是否可以安全移出 mods。
//
// 返回 (是否可移动, 拒绝原因)。原因文本直接用于日志与弹窗。
func (m *Manager) IsZipmodSafeToMove(guid string) (bool, string) {
	return m.IsZipmodSafeToMoveEx(guid, nil)
}

// IsZipmodSafeToMoveEx 是带排除集的重载。
//
// moving 是本次批量操作中正在移动的文件路径集合 ——
// 这些文件算「即将离开」，不构成阻止隔离的理由。
func (m *Manager) IsZipmodSafeToMoveEx(guid string, moving map[string]bool) (bool, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.zipmodGuard().IsZipmodSafeToMoveEx(guid, moving)
}

// checkZipmodMovable 在持锁状态下判定 zipmod 是否可移出。
//
// 供 Manager 内部的批量操作调用（此时已持有写锁，不能再取读锁）。
func (m *Manager) checkZipmodMovable(guid string, moving map[string]bool) (bool, string) {
	return m.zipmodGuard().IsZipmodSafeToMoveEx(guid, moving)
}

// zipmodStillReferenced 与 modDependents 保留在 manager.go / clipboard.go，
// 本文件只提供三道防线的统一入口。

// describeZipmodRefusal 生成用户可读的拒绝原因。
//
// 供批量操作在无法逐条判定时给出整体说明。
func describeZipmodRefusal(guid string, refs []string) string {
	if len(refs) == 0 {
		return ""
	}
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r)
	}
	if len(names) > 3 {
		names = names[:3]
	}
	return fmt.Sprintf("GUID %q 仍被 %d 个资源文件引用，例如：%s",
		guid, len(refs), strings.Join(names, ", "))
}
