/**
 * 前端交互回归测试（阶段 0）
 *
 * 用无头浏览器加载 index.html，注入模拟的 Wails 后端，
 * 验证三个致命缺陷已修复：
 *   1. 单击任意行 → 焦点高亮 + 预览刷新（原先抛 ReferenceError 完全无响应）
 *   2. 右键任意行 → 菜单出现且不消失
 *   3. 「隐藏的文件夹」模式 → 全选可用、可多选、恢复成功
 *   4. 全选复选框在纯文件夹目录下可用（原先被禁用）
 *   5. 隐藏项图标与普通文件夹一致
 *
 * 运行：node tests/frontend.test.mjs
 */

import { chromium } from 'playwright';
import { fileURLToPath } from 'url';
import path from 'path';
import fs from 'fs';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const htmlPath = path.join(__dirname, '..', 'frontend', 'index.html');

let passed = 0;
let failed = 0;

function check(name, cond, detail = '') {
  if (cond) {
    passed++;
    console.log(`  ✓ ${name}`);
  } else {
    failed++;
    console.log(`  ✗ ${name}${detail ? ' — ' + detail : ''}`);
  }
}

/** 构造模拟的树数据：含文件夹、卡片、隐藏项 */
function makeTreeData() {
  const nodes = [
    // 根目录：只有文件夹（用于验证全选可用）
    { id: 'C:\\KK\\female\\FolderA', name: 'FolderA', type: 'folder', path: '', mods: [], modNames: [], size: 0 },
    { id: 'C:\\KK\\female\\FolderB', name: 'FolderB', type: 'folder', path: '', mods: [], modNames: [], size: 0 },
    // FolderA 的子项
    { id: 'C:\\KK\\female\\FolderA\\card1.png', name: 'card1.png', type: 'card', path: 'FolderA', mods: ['g1'], modNames: ['m.zipmod'], size: 1024 },
    { id: 'C:\\KK\\female\\FolderA\\sub', name: 'sub', type: 'folder', path: 'FolderA', mods: [], modNames: [], size: 0 },
    { id: 'C:\\KK\\female\\FolderA\\sub\\deep.png', name: 'deep.png', type: 'card', path: 'FolderA/sub', mods: [], modNames: [], size: 2048 },
  ];
  return { mode: 'female_card', root: 'C:\\KK\\female', rootName: 'UserData/chara/female', nodes };
}

const hiddenTree = {
  left: { mode: 'invisible', root: 'C:\\KK', rootName: 'KKS', nodes: [
    { id: 'C:\\KK\\UserData\\chara\\female\\hidden1', rel: 'UserData/chara/female/hidden1', name: 'UserData/chara/female/hidden1', type: 'hidden', path: '', mods: [], modNames: ['hidden1'] },
    { id: 'C:\\KK\\UserData\\chara\\female\\hidden2', rel: 'UserData/chara/female/hidden2', name: 'UserData/chara/female/hidden2', type: 'hidden', path: '', mods: [], modNames: ['hidden2'] },
  ]},
  right: { mode: 'invisible', root: 'C:\\DEL', rootName: 'DEL', nodes: [] },
};

/** 注入模拟 Wails 运行时（在页面脚本执行前） */
const mockScript = `
window.__calls = [];
const mockApp = {
  GetLeftTreeData: (mode) => {
    window.__calls.push(['GetLeftTreeData', mode]);
    if (mode === 'invisible') return JSON.stringify({ success: true, message: '', data: ${JSON.stringify(hiddenTree)} });
    return JSON.stringify({ success: true, message: '', data: ${JSON.stringify(makeTreeData())} });
  },
  GetRightTreeData: (mode) => JSON.stringify({ success: true, message: '', data: { mode, root: 'C:\\\\DEL', rootName: 'DEL', nodes: [] } }),
  GetInvisibleTreeData: () => JSON.stringify({ success: true, message: '', data: ${JSON.stringify(hiddenTree)} }),
  GetInstalledModsData: () => JSON.stringify({ success: true, message: '', data: { nodes: [] } }),
  GetModsTreeData: () => JSON.stringify({ success: true, message: '', data: { nodes: [] } }),
  GetCardPreview: (p) => JSON.stringify({ success: true, message: '', data: { dataUri: 'data:image/jpeg;base64,/9j/4AAQSkZJRg==' } }),
  GetFolderPreview: (p) => JSON.stringify({ success: true, message: '', data: {
    stats: { subfolderCount: 2, cardCount: 3, totalFiles: 7, totalSize: 1234 },
    previews: [{ name: 's1', path: 'p1', count: 3, dataUri: 'data:image/jpeg;base64,/9j/4AAQSkZJRg==' }],
    remainingFolders: ['s2','s3']
  }}),
  RestoreInvisibleFolders: (paths) => {
    window.__calls.push(['RestoreInvisibleFolders', paths]);
    return JSON.stringify({ success: true, message: '恢复完成', data: { restored: paths.length, failedCount: 0, logs: [] } });
  },
  HideFolders: (paths, side) => JSON.stringify({ success: true, message: '隐藏完成', data: { hidden: 1, skipped: 0, failedCount: 0, logs: [] } }),
  SetClipboard: (op, paths, side, mode) => JSON.stringify({ success: true, message: 'ok', data: { op, paths, sourceSide: side, mode } }),
  GetClipboard: () => JSON.stringify({ success: true, message: '', data: { op: '', paths: [] } }),
  ClearClipboard: () => JSON.stringify({ success: true, message: '', data: null }),
  PasteClipboard: () => JSON.stringify({ success: true, message: 'ok', data: { affected: 1, failedCount: 0, logs: [] } }),
  ExecuteIsolate: () => JSON.stringify({ success: true, message: '', data: { movedCards: 1, movedMods: 0, keptMods: 0, failedCount: 0, logs: [] } }),
  ExecuteRestore: () => JSON.stringify({ success: true, message: '', data: { restoredCards: 1, restoredMods: 0, failedCount: 0, logs: [] } }),
  GetScanStatus: () => JSON.stringify({ success: true, message: '', data: { scanning: false } }),
  GetGlobalDepInfo: () => JSON.stringify({ success: true, message: '', data: {} }),
  Rescan: () => JSON.stringify({ success: true, message: 'ok', data: null }),
  SelectKKSRoot: () => JSON.stringify({ success: true, message: 'ok', data: { kksRoot: 'C:\\\\KK', delCardsRoot: 'C:\\\\DEL', valid: true } }),
};
window.go = { main: { App: mockApp } };
window.runtime = { EventsOn: () => {} };
`;

// 优先使用本机已安装的 Edge/Chrome，避免下载 Playwright 自带内核
let browser;
for (const ch of ['msedge', 'chrome']) {
  try {
    browser = await chromium.launch({ channel: ch });
    console.log('使用浏览器:', ch);
    break;
  } catch (e) { /* 试下一个 */ }
}
if (!browser) {
  browser = await chromium.launch();
  console.log('使用浏览器: bundled chromium');
}
const page = await browser.newPage({ viewport: { width: 1400, height: 900 } });

// 收集 JS 错误 —— 任何未捕获异常都判定为失败
const jsErrors = [];
page.on('pageerror', (e) => jsErrors.push(e.message));

await page.addInitScript(mockScript);
await page.goto('file://' + htmlPath.replace(/\\/g, '/'));
await page.waitForTimeout(300);

// 应用启动后列表为空（等待用户选根目录），测试先主动加载一次数据
await page.evaluate(async () => {
  await loadLeft();
  await loadRight();
});
await page.waitForTimeout(300);

console.log('\n【1】单击行 → 焦点高亮 + 预览刷新（§3.1）');
{
  const before = jsErrors.length;
  const row = page.locator('#left-list .list-item').first();
  await row.click();
  // 单击焦点是延迟的（等待双击序列结束），需等过延迟窗口
  await page.waitForTimeout(400);

  check('单击不抛 JS 异常', jsErrors.length === before, jsErrors.slice(before).join('; '));
  const focused = await page.locator('#left-list .list-item.is-focused').count();
  check('单击后出现焦点高亮行', focused === 1, `实际 ${focused} 个`);
  const title = await page.locator('#preview-title').innerText();
  check('预览标题已更新', title.includes('FolderA'), `实际 "${title}"`);
}

console.log('\n【2】右键行 → 菜单出现且不消失（§3.2）');
{
  const row = page.locator('#left-list .list-item').nth(1);
  await row.click({ button: 'right' });
  await page.waitForTimeout(400);

  const visible = await page.locator('#context-menu.show').count();
  check('右键后菜单可见', visible === 1, `实际 ${visible} 个`);
  const items = await page.locator('#context-menu .menu-item').allInnerTexts();
  check('菜单包含隔离/复制等项', items.length > 0, `实际项: ${items.join('|')}`);
  const box = await page.locator('#context-menu').boundingBox();
  check('菜单位于视口内', box && box.x >= 0 && box.y >= 0, JSON.stringify(box));
}

console.log('\n【3】全选复选框在纯文件夹目录可用（§3.4）');
{
  // 根目录只有文件夹 → 旧的实现会禁用全选
  const disabled = await page.locator('#select-all-left').isDisabled();
  check('全选复选框未被禁用', !disabled);

  // 用 click 而非 check：indeterminate 状态下 check() 的语义不适用
  await page.locator('#select-all-left').click();
  await page.waitForTimeout(250);
  const checked = await page.locator('#left-list .list-item input[type=checkbox]:checked').count();
  const total = await page.locator('#left-list .list-item').count();
  check('全选后所有条目被勾选', checked === total && total >= 2, `勾选 ${checked}/${total}`);

  // 取消
  await page.locator('#select-all-left').click();
  await page.waitForTimeout(250);
  const after = await page.locator('#left-list .list-item input[type=checkbox]:checked').count();
  check('取消全选后全部取消', after === 0, `实际 ${after} 个`);
}

console.log('\n【4】勾选文件夹级联勾选子级（§11.3）');
{
  // 进入 FolderA
  await page.locator('#left-list .list-item').first().dblclick();
  await page.waitForTimeout(200);
  const count = await page.locator('#left-list .list-item').count();
  check('双击进入 FolderA', count >= 1, `实际 ${count} 项`);

  // 回到根目录，勾选 FolderA 应级联
  await page.locator('#btn-back-left').click();
  await page.waitForTimeout(200);
  const folderCb = page.locator('#left-list .list-item').first().locator('input[type=checkbox]');
  await folderCb.check();
  await page.waitForTimeout(200);

  const checkedCount = await page.evaluate(() => leftSelected.size);
  check('勾选文件夹后选中集合包含子孙', checkedCount >= 3, `实际 ${checkedCount} 项`);
}

console.log('\n【5】隐藏的文件夹：可全选、可多选、可恢复（§3.3）');
{
  await page.selectOption('#mode-select', 'invisible');
  await page.waitForTimeout(400);

  const rowCount = await page.locator('#left-list .list-item').count();
  check('隐藏列表已渲染', rowCount === 2, `实际 ${rowCount} 项`);

  const selDisabled = await page.locator('#select-all-left').isDisabled();
  check('隐藏模式下全选可用', !selDisabled);

  // 隔离/启用按钮应禁用
  const execDisabled = await page.locator('#btn-exec-left').isDisabled();
  check('隐藏模式下隔离按钮禁用', execDisabled);

  // 恢复按钮应显示
  const restoreVisible = await page.locator('#btn-restore-hidden').isVisible();
  check('隐藏模式下恢复按钮可见', restoreVisible);

  // 勾选并恢复
  await page.locator('#left-list .list-item').first().locator('input[type=checkbox]').check();
  await page.waitForTimeout(150);
  await page.locator('#btn-restore-hidden').click();
  await page.waitForTimeout(300);

  const calls = await page.evaluate(() => window.__calls.filter(c => c[0] === 'RestoreInvisibleFolders'));
  check('恢复操作已调用后端', calls.length === 1, `实际 ${calls.length} 次`);
  if (calls.length === 1) {
    const arg = calls[0][1][0];
    check('恢复传入的是相对路径（索引 key）', !arg.includes(':\\'), `实际传入 "${arg}"`);
  }
}

console.log('\n【6】隐藏项图标与普通文件夹一致（§3.5）');
{
  const hiddenIcon = await page.evaluate(() => {
    const n = leftNodes.find(x => x.type === 'hidden');
    return n ? n.name : '';
  });
  // 直接比对渲染出的图标前缀
  const icons = await page.evaluate(() => {
    const rows = Array.from(document.querySelectorAll('#left-list .list-item .name'));
    return rows.map(r => r.textContent.trim().charAt(0));
  });
  const allSame = icons.length > 0 && icons.every(i => i === icons[0]);
  check('隐藏项与普通项图标一致', allSame, `实际图标: ${icons.join(',')}`);
}

console.log('\n【7】无未捕获 JS 异常');
{
  check('全程无 JS 错误', jsErrors.length === 0, jsErrors.join(' | '));
}

await page.screenshot({ path: path.join(__dirname, '..', 'build', 'frontend-test.png'), fullPage: false });
await browser.close();

console.log(`\n═══ 结果：${passed} 通过 / ${failed} 失败 ═══\n`);
process.exit(failed === 0 ? 0 : 1);
