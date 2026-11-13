package plugin

import (
	"fmt"
	"sort"
	"strings"
)

// Guard 依赖保护判定器。
//
// 数据来源全部来自内存索引，**绝不扫盘**（红线 R5）。
type Guard struct {
	// GlobalDepMap GUID -> 引用它的资源文件路径
	GlobalDepMap map[string][]string
	// PluginDepMap GUID -> 硬编码引用它的 DLL 相对路径
	PluginDepMap map[string][]string
	// ModDeps zipmod 路径 -> 它声明依赖的 GUID
	ModDeps map[string][]string
	// ZipmodDB GUID -> zipmod 路径
	ZipmodDB map[string]string
}

// IsZipmodSafeToMove 判定某个 zipmod 是否可以安全移出 mods。
//
// 返回 (是否可移动, 拒绝原因)。原因文本直接用于日志与弹窗。
func (g *Guard) IsZipmodSafeToMove(guid string) (bool, string) {
	return g.IsZipmodSafeToMoveEx(guid, nil)
}

// IsZipmodSafeToMoveEx 是带排除集的重载。
//
// moving 是本次批量操作中正在移动的文件路径集合 ——
// 这些文件算「即将离开」，不构成阻止隔离的理由。
func (g *Guard) IsZipmodSafeToMoveEx(guid string, moving map[string]bool) (bool, string) {
	if strings.TrimSpace(guid) == "" {
		return false, "无法识别 GUID，无法确认依赖关系，已保留原文件。"
	}

	// --- 第一道：卡片/场景依赖 ---
	var refs []string
	for _, p := range g.GlobalDepMap[guid] {
		if moving[p] {
			continue
		}
		refs = append(refs, p)
	}
	if len(refs) > 0 {
		sort.Strings(refs)
		return false, fmt.Sprintf("GUID %q 仍被 %d 个资源文件引用，例如：%s",
			guid, len(refs), firstN(refs, 3))
	}

	// --- 第二道：插件硬编码依赖 ---
	if dlls := g.PluginDepMap[guid]; len(dlls) > 0 {
		sort.Strings(dlls)
		return false, fmt.Sprintf("[插件保护] GUID %q 被插件 %q 硬编码引用，跳过隔离以防崩溃。",
			guid, strings.Join(dlls, ", "))
	}

	// --- 第三道：其它 zipmod 依赖它 ---
	if deps := g.modDependents(guid); len(deps) > 0 {
		return false, fmt.Sprintf("GUID %q 被其它 zipmod 依赖：%s", guid, firstN(deps, 3))
	}

	return true, ""
}

// modDependents 反查：哪些 zipmod 声明依赖了 guid。
func (g *Guard) modDependents(guid string) []string {
	var dependents []string
	for modPath, deps := range g.ModDeps {
		for _, dep := range deps {
			if dep == guid {
				dependents = append(dependents, modPath)
				break
			}
		}
	}
	sort.Strings(dependents)
	return dependents
}

// firstN 返回列表前 n 项并拼接为可读字符串。
func firstN(list []string, n int) string {
	if len(list) == 0 {
		return ""
	}
	if len(list) < n {
		n = len(list)
	}
	return strings.Join(list[:n], ", ")
}
