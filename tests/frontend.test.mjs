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
window.__thumbBatchCalls = [];
window.__rightNodes = [];
window.__treeData = null;
const mockApp = {
  GetLeftTreeData: (mode) => {
    window.__calls.push(['GetLeftTreeData', mode]);
    if (mode === 'invisible') return JSON.stringify({ success: true, message: '', data: ${JSON.stringify(hiddenTree)} });
    return JSON.stringify({ success: true, message: '', data: window.__treeData || ${JSON.stringify(makeTreeData())} });
  },
  GetRightTreeData: (mode) => JSON.stringify({ success: true, message: '', data: { mode, root: 'C:\\\\DEL', rootName: 'DEL', nodes: window.__rightNodes || [] } }),
  GetInvisibleTreeData: () => JSON.stringify({ success: true, message: '', data: ${JSON.stringify(hiddenTree)} }),
  GetInstalledModsData: () => JSON.stringify({ success: true, message: '', data: { mode: 'zipmod', root: 'C:/KK/mods', rootName: 'mods', nodes: [
    { id: 'C:/KK/mods/used.zipmod', name: 'used.zipmod', type: 'mod', path: '', mods: ['g1'], modNames: ['used.zipmod'], size: 1024, guid: 'g1', refCount: 15, refCards: 14, refPlugins: 1 },
    { id: 'C:/KK/mods/free.zipmod', name: 'free.zipmod', type: 'mod', path: '', mods: ['g2'], modNames: ['free.zipmod'], size: 2048, guid: 'g2', refCount: 0, refCards: 0, refPlugins: 0 },
  ]}}),
  GetModsTreeData: () => JSON.stringify({ success: true, message: '', data: { nodes: [] } }),
  GetCardPreview: (p) => JSON.stringify({ success: true, message: '', data: { dataUri: 'data:image/jpeg;base64,/9j/4AAQSkZJRg==' } }),
  // v2.1：右栏面板一次请求出全部
  GetRightPanel: (side, mode, rel, focusAbs) => {
    window.__calls.push(['GetRightPanel', side, mode, rel, focusAbs]);
    const d = window.__panelData || {
      focus: { kind: 'folder', name: '根目录', abs: 'C:/KK/female', info: '2 个子文件夹' },
      sections: [
        { id: 'folders', title: '子文件夹', layout: 'grid', total: 2, tiles: [
          { kind: 'card', name: 'c1.png', rel: '', abs: 'C:/x/c1.png', badge: '', thumbKey: 'k1', thumb: '', pending: true },
          { kind: 'card', name: 'c2.png', rel: '', abs: 'C:/x/c2.png', badge: '', thumbKey: 'k2', thumb: '', pending: true },
        ], more: 0, moreLabel: '', visible: true, empty: '' },
        { id: 'cards', title: '此文件夹的卡片', layout: 'grid', total: 0, tiles: [], more: 0, moreLabel: '', visible: false, empty: '' },
      ],
      gen: 0,
    };
    return JSON.stringify({ success: true, message: '', data: d });
  },
  GetLogs: (sinceSeq, level) => JSON.stringify({ success: true, message: '', data: { seq: 10, entries: [
    { seq: 9, ts: Date.now(), level: 'info', scope: 'scan', msg: '测试日志信息' },
    { seq: 10, ts: Date.now(), level: 'error', scope: 'isolate', msg: '测试日志错误' },
  ], total: 2, errors: 1 } }),
  GetHistory: (offset, limit) => JSON.stringify({ success: true, message: '', data: { entries: [
    { txId: 'tx1', kind: 'isolate', title: '隔离 3 个条目', at: Date.now() - 60000, affected: 3, bytes: 0, status: 'committed', reverted: false, steps: [{kind:'move',src:'a',dst:'b',state:'done'}] },
    { txId: 'tx2', kind: 'restore', title: '恢复 1 个条目', at: Date.now() - 30000, affected: 1, bytes: 0, status: 'committed', reverted: false, steps: [{kind:'move',src:'c',dst:'d',state:'done'}] },
    { txId: 'tx3', kind: 'undo', title: '撤销：隔离 3 个条目', at: Date.now(), affected: 3, bytes: 0, status: 'committed', reverted: false, steps: [] },
  ], total: 3, max: 200 } }),
  CmdUndo: (txId) => {
    window.__calls.push(['CmdUndo', txId]);
    return JSON.stringify({ success: true, message: '已撤销', data: { ok: true, reverted: 3 } });
  },
  CmdMkdir: (side, mode, parentRel) => {
    window.__calls.push(['CmdMkdir', side, mode, parentRel]);
    return JSON.stringify({ success: true, message: 'ok', data: { ok: true, name: '新建文件夹 (2)', rel: '新建文件夹 (2)', path: 'C:/KK/female/新建文件夹 (2)' } });
  },
  CmdRename: (pathAbs, newName) => {
    window.__calls.push(['CmdRename', pathAbs, newName]);
    if (newName.includes('/')) {
      return JSON.stringify({ success: false, message: '名称不能包含以下字符：< > : " / \\ | ? *', data: { code: 'NAME_INVALID' } });
    }
    return JSON.stringify({ success: true, message: 'ok', data: { ok: true, name: newName, path: pathAbs } });
  },
  PlanDelete: (paths) => {
    window.__calls.push(['PlanDelete', paths]);
    return JSON.stringify({ success: true, message: 'ok', data: { planId: 'del-1', items: paths.map(p => ({ src: p, trashRel: p, spec: 'female_card', size: 100, isDir: false, name: 'x' })), totalBytes: 5976883, conflicts: [] } });
  },
  CommitDelete: (paths) => {
    window.__calls.push(['CommitDelete', paths]);
    return JSON.stringify({ success: true, message: 'ok', data: { moved: paths.length } });
  },
  GetTrashTree: () => JSON.stringify({ success: true, message: '', data: [
    { rel: 'female_card/a.png', abs: 'C:/DELCARDS/DELED/female_card/a.png', originalAbs: 'C:/KK/female/a.png', spec: 'female_card', deletedAt: Date.now(), size: 100, isDir: false, name: 'a.png' },
  ] }),
  CmdTrashRestore: (rels) => JSON.stringify({ success: true, message: 'ok', data: { restored: rels.length, skipped: 0, conflicts: [] } }),
  CmdTrashPurge: (rels, text) => JSON.stringify({ success: true, message: 'ok', data: { purged: rels.length, errors: [] } }),
  GetThumbBatch: (paths) => {
    window.__thumbBatchCalls.push(paths.slice());
    return JSON.stringify({ success: true, message: '', data: paths.map(p => ({
      thumbKey: 'key-' + p.split(/[\\/]/).pop(),
      dataUri: 'data:image/jpeg;base64,TB',
      pending: false,
    })) });
  },
  PlanFixMissing: () => {
    window.__calls.push(['PlanFixMissing']);
    return JSON.stringify({ success: true, message: '', data: {
      planId: 'fix-1', totalBytes: 1048576,
      recoverable: [
        { guid: 'missing.a', name: 'a.zipmod', from: 'C:/DEL/zipmod/a.zipmod', to: 'C:/KK/mods/a.zipmod', refCards: 3, refPlugins: 0, source: 'del-zipmod' },
        { guid: 'missing.b', name: 'b.zipmod', from: 'C:/DELED/b.zipmod', to: 'C:/KK/mods/b.zipmod', refCards: 1, refPlugins: 1, source: 'deled' },
      ],
      unrecoverable: [ { guid: 'missing.c', refCards: 2, refPlugins: 0, sample: ['c1.png', 'c2.png'] } ],
    }});
  },
  CommitFixMissing: (guids) => {
    window.__calls.push(['CommitFixMissing', guids]);
    return JSON.stringify({ success: true, message: 'ok', data: { restored: guids.length, skipped: 0, failed: [] } });
  },
  PlanIsolateExtra: () => {
    window.__calls.push(['PlanIsolateExtra']);
    return JSON.stringify({ success: true, message: '', data: {
      planId: 'extra-1', totalBytes: 2097152,
      candidates: [
        { guid: 'extra.a', base: 'unused-a.zipmod', path: 'C:/KK/mods/unused-a.zipmod', size: 1048576, note: '' },
        { guid: 'extra.b', base: 'unused-b.zipmod', path: 'C:/KK/mods/unused-b.zipmod', size: 1048576, note: '重复副本' },
      ],
      skipped: [ { path: 'C:/KK/mods/timeline.zipmod', reason: '被插件 Timeline.dll 硬编码引用' } ],
    }});
  },
  CommitIsolateExtra: (guids) => {
    window.__calls.push(['CommitIsolateExtra', guids]);
    return JSON.stringify({ success: true, message: 'ok', data: { isolated: guids.length, bytes: 2097152, skipped: 0, failed: [] } });
  },
  OpenFolderInExplorer: (abs) => {
    window.__calls.push(['OpenFolderInExplorer', abs]);
    return JSON.stringify({ success: true, message: 'ok', data: null });
  },
  OpenListDirInExplorer: (side, mode, rel) => {
    window.__calls.push(['OpenListDirInExplorer', side, mode, rel]);
    return JSON.stringify({ success: true, message: 'ok', data: null });
  },
  GetZipmodDetail: (guid) => JSON.stringify({ success: true, message: '', data: { guid, path: 'C:/x.zipmod', base: 'x.zipmod', size: 100, cards: [], plugins: [], refCards: 0, refPlugins: 0, parseErr: '' } }),
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
  const title = await page.locator('#panel-info-title').innerText();
  check('右栏信息栏标题已更新', title.length > 0, `实际 "${title}"`);
  const panelSections = await page.locator('#panel-sections .panel-section').count();
  check('右栏面板已渲染区域', panelSections >= 1, `实际 ${panelSections} 个`);
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

console.log('\n【8】右栏面板：九宫格与区域规则（v2.1 §11.1）');
{
  // 场景：无子文件夹 + 有卡片 → 只有区域 B，区域 A 完全不出现
  await page.evaluate(() => {
    window.__panelData = {
      focus: { kind: 'folder', name: '精品卡2', abs: 'C:/KK/female', info: '115 张卡片 · 23 个子文件夹 · 750.6 MB' },
      sections: [
        { id: 'folders', title: '子文件夹', layout: 'grid', total: 0, tiles: [], more: 0, moreLabel: '', visible: false, empty: '' },
        { id: 'cards', title: '此文件夹的卡片', layout: 'grid', total: 5, tiles: [
          { kind: 'card', name: 'c1.png', rel: 'c1.png', abs: 'C:/KK/female/c1.png', badge: '', thumbKey: 'k1', thumb: '', pending: true },
          { kind: 'card', name: 'c2.png', rel: 'c2.png', abs: 'C:/KK/female/c2.png', badge: '', thumbKey: 'k2', thumb: '', pending: true },
          { kind: 'card', name: 'c3.png', rel: 'c3.png', abs: 'C:/KK/female/c3.png', badge: '', thumbKey: 'k3', thumb: '', pending: true },
          { kind: 'card', name: 'c4.png', rel: 'c4.png', abs: 'C:/KK/female/c4.png', badge: '', thumbKey: 'k4', thumb: '', pending: true },
          { kind: 'card', name: 'c5.png', rel: 'c5.png', abs: 'C:/KK/female/c5.png', badge: '', thumbKey: 'k5', thumb: '', pending: true },
        ], more: 0, moreLabel: '', visible: true, empty: '' },
      ],
      gen: 1,
    };
  });
  await page.evaluate(() => refreshPanel());
  await page.waitForTimeout(300);

  const sections = await page.locator('#panel-sections .panel-section').count();
  check('无子文件夹时只渲染 1 个区域', sections === 1, `实际 ${sections} 个`);

  const cells = await page.locator('#panel-sections .panel-cell').count();
  check('5 张卡片渲染 5 个格子', cells === 5, `实际 ${cells} 个`);

  // 行数自适应：5 格 = 2 行（网格 3 列 → 高度约 2 行）
  const grid = await page.evaluate(() => {
    const g = document.querySelector('#panel-sections .panel-grid');
    if (!g) return null;
    const tops = new Set(Array.from(g.children).map(c => Math.round(c.getBoundingClientRect().top)));
    return tops.size;
  });
  check('5 格只占 2 行（不占 3 行）', grid === 2, `实际 ${grid} 行`);

  const infoTitle = await page.locator('#panel-info-title').innerText();
  check('信息栏显示焦点名称', infoTitle.includes('精品卡2'), `实际 "${infoTitle}"`);
  const infoSub = await page.locator('#panel-info-sub').innerText();
  check('信息栏显示统计', infoSub.includes('115'), `实际 "${infoSub}"`);
}

console.log('\n【9】右栏面板：超过 9 个显示「还有 N」（v2.1 §11.1）');
{
  await page.evaluate(() => {
    const tiles = [];
    for (let i = 0; i < 8; i++) {
      tiles.push({ kind: 'card', name: 'c' + i + '.png', rel: '', abs: 'C:/x/c' + i + '.png', badge: '', thumbKey: 'k' + i, thumb: '', pending: true });
    }
    tiles.push({ kind: 'more', name: '还有 107 张角色卡', rel: '', abs: '', badge: '', thumbKey: '', thumb: '', pending: false });
    window.__panelData = {
      focus: { kind: 'folder', name: '大目录', abs: 'C:/KK/female', info: '115 张卡片' },
      sections: [
        { id: 'folders', title: '子文件夹', layout: 'grid', total: 0, tiles: [], more: 0, moreLabel: '', visible: false, empty: '' },
        { id: 'cards', title: '此文件夹的卡片', layout: 'grid', total: 115, tiles, more: 107, moreLabel: '还有 107 张角色卡', visible: true, empty: '' },
      ],
      gen: 2,
    };
  });
  await page.evaluate(() => refreshPanel());
  await page.waitForTimeout(300);

  const cells = await page.locator('#panel-sections .panel-cell').count();
  check('8 张 + 1 个「还有」= 9 格', cells === 9, `实际 ${cells} 个`);
  const moreText = await page.locator('#panel-sections .panel-cell.is-more .more-label').innerText().catch(() => '');
  check('「还有」格文案正确', moreText.includes('还有 107'), `实际 "${moreText}"`);
}

console.log('\n【10】日志窗口：按钮 + 弹窗（v2.1 §3.6）');
{
  const logBoxExists = await page.locator('#log-box').count();
  check('右栏不再有常驻日志框', logBoxExists === 0, `实际 ${logBoxExists} 个`);

  const btn = await page.locator('#btn-open-log').count();
  check('存在「查看日志」按钮', btn === 1);

  await page.locator('#btn-open-log').click();
  await page.waitForTimeout(400);
  const modalShown = await page.locator('#log-modal.show').count();
  check('点击后日志窗口弹出', modalShown === 1);

  const rows = await page.locator('#log-body .log-row').count();
  check('日志窗口渲染了条目', rows >= 2, `实际 ${rows} 行`);

  // 级别过滤
  await page.locator('#log-seg button[data-level="error"]').click();
  await page.waitForTimeout(400);
  const errRows = await page.locator('#log-body .log-row').count();
  const footText = await page.locator('#log-foot').innerText();
  check('级别过滤生效', errRows >= 1 && errRows < rows, `错误 ${errRows} / 全部 ${rows}`);
  check('底部显示统计', footText.includes('本次会话'), `实际 "${footText}"`);

  // Esc 关闭
  await page.keyboard.press('Escape');
  await page.waitForTimeout(300);
  const closed = await page.locator('#log-modal.show').count();
  check('Esc 可关闭日志窗口', closed === 0);
}

console.log('\n【11】thumb:ready 局部补图（v2.1 §10.3）');
{
  await page.evaluate(() => {
    // 模拟后端推送
    const handler = window.__thumbHandler;
    if (handler) handler([{ thumbKey: 'k1', dataUri: 'data:image/jpeg;base64,/9j/4AAQSkZJRg==' }]);
  });
  await page.waitForTimeout(200);
  const imgs = await page.locator('#panel-sections .panel-cell img').count();
  check('补图后出现 img 元素', imgs >= 1, `实际 ${imgs} 个`);
}

console.log('\n【12】zipmod 占用数：红色数字（v2.1 §11.3）');
{
  await page.selectOption('#mode-select', 'zipmod');
  await page.waitForTimeout(500);

  const counts = await page.locator('#left-list .ref-count').count();
  check('zipmod 行显示占用数列', counts === 2, `实际 ${counts} 个`);

  const texts = await page.locator('#left-list .ref-count').allInnerTexts();
  check('占用数正确（15 / 0）', texts[0] === '15' && texts[1] === '0', `实际 ${texts.join(',')}`);

  const colors = await page.evaluate(() => {
    const els = Array.from(document.querySelectorAll('#left-list .ref-count'));
    return els.map(e => getComputedStyle(e).color + '|' + e.className);
  });
  check('有引用的显示红色', colors[0].includes('rgb(198, 40, 40)'), `实际 ${colors[0]}`);
  check('0 引用的显示灰色', colors[1].includes('is-zero'), `实际 ${colors[1]}`);

  const tooltip = await page.locator('#left-list .ref-count').first().getAttribute('title');
  check('tooltip 含占用明细', tooltip && tooltip.includes('卡片') && tooltip.includes('插件'), `实际 "${tooltip}"`);

  // 不应显示文件大小
  const sizes = await page.locator('#left-list .size').count();
  check('zipmod 行不显示文件大小', sizes === 0, `实际 ${sizes} 个`);
}

console.log('\n【13】历史窗口：列表 + 撤销（v2.1 §11.6）');
{
  const btn = await page.locator('#btn-open-history').count();
  check('顶栏有「操作记录」按钮', btn === 1);

  await page.locator('#btn-open-history').click();
  await page.waitForTimeout(400);
  const shown = await page.locator('#history-modal.show').count();
  check('点击后历史窗口弹出', shown === 1);

  const rows = await page.locator('#history-body .history-row').count();
  check('历史列表有 3 条', rows === 3, `实际 ${rows} 条`);

  const foot = await page.locator('#history-foot').innerText();
  check('底部显示保留条数', foot.includes('保留最近'), `实际 "${foot}"`);

  // 撤销按钮
  const undoBtns = await page.locator('#history-body .btn-undo').count();
  check('每行有撤销按钮', undoBtns === 3, `实际 ${undoBtns} 个`);

  // 无步骤的记录（undo 类型）应禁用
  const disabled = await page.evaluate(() => {
    const btns = Array.from(document.querySelectorAll('#history-body .btn-undo'));
    return btns.map(b => b.disabled);
  });
  check('无步骤的记录撤销按钮禁用', disabled[2] === true, `实际 ${JSON.stringify(disabled)}`);

  // 筛选：可撤销
  await page.locator('#history-seg button[data-filter="undoable"]').click();
  await page.waitForTimeout(300);
  const undoableRows = await page.locator('#history-body .history-row').count();
  check('「可撤销」筛选生效', undoableRows === 2, `实际 ${undoableRows} 条`);

  // Esc 关闭
  await page.keyboard.press('Escape');
  await page.waitForTimeout(300);
  const closed = await page.locator('#history-modal.show').count();
  check('Esc 可关闭历史窗口', closed === 0);
}

console.log('\n【14】自由模式：按钮可见性与回收站（v2.1 §11.4）');
{
  // 安全模式下新建/重命名/删除按钮应隐藏
  let visible = await page.locator('#btn-new-folder').isVisible();
  check('安全模式下无「新建」入口', !visible);

  // 切到自由修改模式
  await page.locator('#btn-mode').click();
  await page.waitForTimeout(300);
  visible = await page.locator('#btn-new-folder').isVisible();
  check('自由模式下显示「新建」按钮', visible);
  visible = await page.locator('#btn-delete').isVisible();
  check('自由模式下显示「删除」按钮', visible);

  // 切回安全模式
  await page.locator('#btn-mode').click();
  await page.waitForTimeout(300);
  visible = await page.locator('#btn-new-folder').isVisible();
  check('切回安全模式后按钮隐藏', !visible);
}

console.log('\n【15】空白处右键 → 目录菜单（v2.1 §6.3）');
{
  await page.locator('#btn-mode').click();   // 自由模式
  await page.waitForTimeout(300);

  // 在列表空白处右键
  const box = await page.locator('#left-list').boundingBox();
  await page.mouse.click(box.x + box.width - 20, box.y + box.height - 20, { button: 'right' });
  await page.waitForTimeout(400);

  const shown = await page.locator('#context-menu.show').count();
  check('空白处右键弹出菜单', shown === 1);

  const items = await page.locator('#context-menu .menu-item').allInnerTexts();
  const joined = items.join('|');
  check('菜单含「新建文件夹」', joined.includes('新建文件夹'), joined);
  check('菜单含「全选」', joined.includes('全选'), joined);
  check('菜单含「刷新」', joined.includes('刷新'), joined);

  // 空白处左键 → 清除焦点与勾选
  await page.mouse.click(box.x + box.width - 20, box.y + box.height - 20);
  await page.waitForTimeout(300);
  const menuClosed = await page.locator('#context-menu.show').count();
  check('空白处左键关闭菜单', menuClosed === 0);
}

console.log('\n【16】回收站模式（v2.1 §6.4）');
{
  await page.selectOption('#mode-select', 'trash');
  await page.waitForTimeout(500);

  const rows = await page.locator('#left-list .list-item').count();
  check('回收站列表已渲染', rows >= 1, `实际 ${rows} 项`);

  const restoreVisible = await page.locator('#btn-trash-restore').isVisible();
  check('回收站显示「还原」按钮', restoreVisible);
  const purgeVisible = await page.locator('#btn-trash-purge').isVisible();
  check('回收站显示「彻底删除」按钮', purgeVisible);

  // 隔离/启用按钮应禁用
  const execDisabled = await page.locator('#btn-exec-left').isDisabled();
  check('回收站模式禁用隔离按钮', execDisabled);

  // 新建按钮应隐藏
  const newVisible = await page.locator('#btn-new-folder').isVisible();
  check('回收站模式隐藏「新建」按钮', !newVisible);
}


console.log('\n【17】FIX-05：删除确认框（不得静默失败）');
{
  // 回到角色卡模式 + 自由修改模式
  await page.selectOption('#mode-select', 'female_card');
  await page.waitForTimeout(500);
  await page.waitForTimeout(400);

  const opModeNow = await page.evaluate(() => opMode);
  if (opModeNow !== 'free') {
    await page.locator('#btn-mode').click();
    await page.waitForTimeout(300);
  }

  // 进入 FolderA（根目录只有文件夹）
  const folder = page.locator('#left-list .list-item[data-type="folder"]').first();
  await folder.dblclick();
  await page.waitForTimeout(500);

  const rows = await page.locator('#left-list .list-item').count();
  check('已进入 FolderA 且有条目', rows >= 1, `实际 ${rows} 行`);

  // 勾选第一张卡
  await page.locator('#left-list .list-item input[type=checkbox]').first().click();
  await page.waitForTimeout(300);

  // 点删除
  await page.locator('#btn-delete').click();
  await page.waitForTimeout(600);

  const modalShown = await page.locator('#modal-mask.show').count();
  check('点删除后确认框出现（不再毫无反应）', modalShown === 1, `实际 ${modalShown}`);

  const body = await page.locator('#modal-body').innerText();
  check('确认框文案含正确大小（MB）', /MB|B/.test(body) && !body.includes('NaN'), `实际 "${body}"`);
  check('确认框文案含条目数', body.includes('1 个项目'), `实际 "${body}"`);

  // 关闭
  await page.locator('#modal-foot button').first().click();
  await page.waitForTimeout(300);
  const closed = await page.locator('#modal-mask.show').count();
  check('取消后确认框关闭', closed === 0);
}

console.log('\n【18】FIX-05：全局异常兜底');
{
  // 制造一个未捕获的 Promise 拒绝 → 应弹阻断性提示
  await page.evaluate(() => {
    Promise.reject(new Error('测试用的异步异常'));
  });
  await page.waitForTimeout(500);

  const modalShown = await page.locator('#modal-mask.show').count();
  const body = modalShown ? await page.locator('#modal-body').innerText() : '';
  check('未捕获的异步异常弹出提示', modalShown === 1 && body.includes('测试用的异步异常'), `实际显示=${modalShown} 文案="${body}"`);

  if (modalShown) {
    await page.locator('#modal-foot button').first().click();
    await page.waitForTimeout(300);
  }
}


console.log('\n【19】FIX-02：卡片焦点单张大图');
{
  await page.evaluate(() => {
    window.__panelData = {
      focus: { kind: 'card', name: '87642255_p18.png', abs: 'C:/KK/female/22.7.15/87642255_p18.png', info: '5.7 MB · 依赖 6 个 zipmod' },
      sections: [
        { id: 'preview', title: '', layout: 'single', total: 1, tiles: [
          { kind: 'card', name: '87642255_p18.png', rel: '22.7.15/87642255_p18.png', abs: 'C:/KK/female/22.7.15/87642255_p18.png', badge: '', thumbKey: 'bigkey-l', thumb: '', thumbPreview: 'data:image/jpeg;base64,/9j/4AAQSkZJRg==', pending: true, selected: true }
        ], more: 0, moreLabel: '', visible: true, empty: '', lazy: false },
      ],
      gen: 10,
    };
  });
  await page.evaluate(async () => { await refreshPanel(); });
  await page.waitForTimeout(400);

  const single = await page.locator('#panel-sections .panel-single').count();
  check('渲染单卡大图容器', single === 1, `实际 ${single} 个`);

  const grids = await page.locator('#panel-sections .panel-grid').count();
  check('不渲染九宫格', grids === 0, `实际 ${grids} 个`);

  const cells = await page.locator('#panel-sections .panel-cell').count();
  check('不渲染网格格子', cells === 0, `实际 ${cells} 个`);

  const moreCells = await page.locator('#panel-sections .panel-cell.is-more').count();
  check('无「还有 N 张」格', moreCells === 0, `实际 ${moreCells} 个`);

  const imgs = await page.locator('#panel-sections .panel-single img').count();
  check('大图未就绪时用占位图顶上（不空白）', imgs === 1, `实际 ${imgs} 个`);

  const nameText = await page.locator('#panel-sections .single-name').innerText();
  check('大图下方显示文件名', nameText.includes('87642255_p18.png'), `实际 "${nameText}"`);

  const infoText = await page.locator('#panel-sections .single-info').innerText();
  check('信息行显示大小与依赖', infoText.includes('MB') && infoText.includes('依赖'), `实际 "${infoText}"`);

  // 收到大图后应原地替换
  await page.evaluate(() => {
    const h = window.__thumbHandler;
    if (h) h([{ thumbKey: 'bigkey-l', dataUri: 'data:image/jpeg;base64,BIGIMAGE' }]);
  });
  await page.waitForTimeout(250);
  const src = await page.evaluate(() => {
    const img = document.querySelector('#panel-sections .panel-single img');
    return img ? img.getAttribute('src') : '';
  });
  check('收到大图后原地替换', src.includes('BIGIMAGE'), `实际 "${src.slice(0, 40)}"`);

  const aspect = await page.evaluate(() => {
    const f = document.querySelector('.panel-single .single-frame');
    if (!f) return '';
    return getComputedStyle(f.querySelector('img')).objectFit;
  });
  check('大图使用 contain（不裁切卡片）', aspect === 'contain', `实际 "${aspect}"`);
}


console.log('\n【20】FIX-03：zipmod 引用全量显示 + 懒加载');
{
  // 33 个引用，全部应渲染，且没有「还有 N」
  await page.evaluate(() => {
    const tiles = [];
    for (let i = 0; i < 33; i++) {
      tiles.push({
        kind: 'card',
        name: 'card_' + String(i).padStart(2, '0') + '.png',
        rel: 'cards/card_' + i + '.png',
        abs: 'C:/KK/female/cards/card_' + i + '.png',
        badge: '',
        thumbKey: 'key-card_' + i + '.png',
        thumb: '',
        thumbPreview: '',
        pending: true,
      });
    }
    window.__panelData = {
      focus: { kind: 'mod', name: 'gyobobo.zipmod', abs: 'C:/KK/mods/gyobobo.zipmod', info: '被 33 个文件占用（卡片 33 / 插件 0）' },
      sections: [
        { id: 'cards', title: '使用此 zipmod 的卡片', layout: 'grid', total: 33, tiles, more: 0, moreLabel: '', visible: true, empty: '', lazy: true },
        { id: 'plugins', title: '引用此 GUID 的插件', layout: 'list', total: 0, tiles: [], more: 0, moreLabel: '', visible: true, empty: '未被任何插件硬编码引用', lazy: false },
      ],
      gen: 20,
    };
    window.__thumbBatchCalls.length = 0;
  });
  await page.evaluate(async () => { await refreshPanel(); });
  await page.waitForTimeout(600);

  const cells = await page.locator('#panel-sections .panel-cell').count();
  check('33 个引用渲染 33 个格子', cells === 33, `实际 ${cells} 个`);

  const moreCells = await page.locator('#panel-sections .panel-cell.is-more').count();
  check('zipmod 不出现「还有」格', moreCells === 0, `实际 ${moreCells} 个`);

  const bodyText = await page.locator('#panel-sections').innerText();
  check('页面文本不含「还有」', !bodyText.includes('还有'), `实际含"还有"`);

  // 行数：33 格 / 3 列 = 11 行
  const rows = await page.evaluate(() => {
    const g = document.querySelector('#panel-sections .panel-grid');
    if (!g) return 0;
    const tops = new Set(Array.from(g.children).map(c => Math.round(c.getBoundingClientRect().top)));
    return tops.size;
  });
  check('33 格排成 11 行', rows === 11, `实际 ${rows} 行`);

  // 懒加载：应通过 GetThumbBatch 分批请求，而不是后端预生成
  const calls = await page.evaluate(() => window.__thumbBatchCalls.length);
  check('触发 GetThumbBatch 懒加载', calls >= 1, `实际 ${calls} 次`);

  const maxBatch = await page.evaluate(() => {
    return window.__thumbBatchCalls.reduce((m, b) => Math.max(m, b.length), 0);
  });
  check('每批请求 ≤12 个', maxBatch <= 12, `实际最大 ${maxBatch}`);

  // 可视区先出图：首屏格子应有 img
  const imgs = await page.locator('#panel-sections .panel-cell img').count();
  check('可视区缩略图已补上', imgs >= 1, `实际 ${imgs} 个`);
}


console.log('\n【21】FIX-04：删除按钮自动择侧');
{
  await page.selectOption('#mode-select', 'female_card');
  await page.waitForTimeout(400);
  if (await page.evaluate(() => opMode) !== 'free') {
    await page.locator('#btn-mode').click();
    await page.waitForTimeout(300);
  }

  // 右侧注入两条隔离项
  await page.evaluate(async () => {
    window.__rightNodes = [
      { id: 'C:/DEL/female_card/iso1.png', name: 'iso1.png', type: 'card', path: '', mods: [], modNames: [], size: 2048 },
      { id: 'C:/DEL/female_card/iso2.png', name: 'iso2.png', type: 'card', path: '', mods: [], modNames: [], size: 4096 },
    ];
    leftSelected.clear();
    rightSelected.clear();
    await loadRight();
  });
  await page.waitForTimeout(400);

  const rightRows = await page.locator('#right-list .list-item').count();
  check('右侧已加载 2 条隔离项', rightRows === 2, `实际 ${rightRows} 行`);

  let disabled = await page.locator('#btn-delete').isDisabled();
  check('未勾选时删除按钮为禁用态', disabled);

  // 勾选右侧 2 项
  await page.locator('#right-list .list-item input[type=checkbox]').first().click();
  await page.waitForTimeout(200);
  await page.locator('#right-list .list-item input[type=checkbox]').nth(1).click();
  await page.waitForTimeout(300);

  disabled = await page.locator('#btn-delete').isDisabled();
  check('勾选后删除按钮可用', !disabled);

  const activeRight = await page.evaluate(() => document.getElementById('pane-right').classList.contains('is-active-side'));
  check('最近交互侧标记在右侧', activeRight);

  // 点删除 → 应出现确认框，而不是「请先勾选」
  await page.locator('#btn-delete').click();
  await page.waitForTimeout(600);

  const modalShown = await page.locator('#modal-mask.show').count();
  const body = modalShown ? await page.locator('#modal-body').innerText() : '';
  check('右侧勾选后删除弹确认框', modalShown === 1, `弹窗=${modalShown}`);
  check('确认框未出现「请先勾选」', !body.includes('请先勾选'), `实际 "${body}"`);
  check('确认框显示 2 个项目', body.includes('2 个项目'), `实际 "${body}"`);

  if (modalShown) {
    await page.locator('#modal-foot button').first().click();
    await page.waitForTimeout(300);
  }

  // 两侧都勾选 → 明确提示
  await page.evaluate(async () => { await loadLeft(); });
  await page.waitForTimeout(400);
  await page.locator('#left-list .list-item input[type=checkbox]').first().click();
  await page.waitForTimeout(300);

  await page.locator('#btn-delete').click();
  await page.waitForTimeout(600);

  const conflictShown = await page.locator('#modal-mask.show').count();
  const conflictBody = conflictShown ? await page.locator('#modal-body').innerText() : '';
  check('两侧都勾选时明确提示', conflictShown === 1 && conflictBody.includes('都有勾选'), `实际 "${conflictBody}"`);

  if (conflictShown) {
    await page.locator('#modal-foot button').first().click();
    await page.waitForTimeout(300);
  }

  // 清理勾选
  await page.evaluate(() => {
    leftSelected.clear();
    rightSelected.clear();
    window.__rightNodes = [];
    renderAll();
  });
}


console.log('\n【22】阶段F 死按钮收口：zipmod 两个管理入口');
{
  await page.selectOption('#mode-select', 'zipmod');
  await page.waitForTimeout(500);

  const fixVisible = await page.locator('#btn-fix-missing').isVisible();
  const extraVisible = await page.locator('#btn-isolate-extra').isVisible();
  check('zipmod 模式显示两个管理按钮', fixVisible && extraVisible);

  // 修复缺失
  await page.locator('#btn-fix-missing').click();
  await page.waitForTimeout(500);
  let shown = await page.locator('#modal-mask.show').count();
  check('点「修复缺失」弹计划对话框', shown === 1);
  let body = shown ? await page.locator('#modal-body').innerText() : '';
  check('计划列出可修复与无法修复', body.includes('可修复 2') && body.includes('无法修复 1'), `实际 "${body.slice(0, 100)}"`);
  const fixChecks = await page.locator('.fix-check').count();
  check('可修复项默认全选', fixChecks === 2, `实际 ${fixChecks} 个`);

  // 点确认 → 调用 CommitFixMissing
  await page.locator('#modal-foot button').nth(1).click();
  await page.waitForTimeout(500);
  const commitFix = await page.evaluate(() => window.__calls.filter(c => c[0] === 'CommitFixMissing').length);
  check('确认后调用 CommitFixMissing', commitFix === 1, `实际 ${commitFix} 次`);

  // 隔离多余
  await page.locator('#btn-isolate-extra').click();
  await page.waitForTimeout(500);
  shown = await page.locator('#modal-mask.show').count();
  check('点「隔离多余」弹计划对话框', shown === 1);
  body = shown ? await page.locator('#modal-body').innerText() : '';
  check('计划列出候选与永不自动隔离清单', body.includes('将隔离 2') && body.includes('永不自动隔离'), `实际 "${body.slice(0, 100)}"`);
  const extraChecks = await page.locator('.extra-check').count();
  check('候选默认全选', extraChecks === 2, `实际 ${extraChecks} 个`);

  await page.locator('#modal-foot button').nth(1).click();
  await page.waitForTimeout(500);
  const commitExtra = await page.evaluate(() => window.__calls.filter(c => c[0] === 'CommitIsolateExtra').length);
  check('确认后调用 CommitIsolateExtra', commitExtra === 1, `实际 ${commitExtra} 次`);
}


console.log('\n【23】JPG 角色卡：列表识别 + 右键菜单');
{
  await page.selectOption('#mode-select', 'female_card');
  await page.waitForTimeout(400);

  // 注入含 .JPG 的树：一个非空文件夹 + 一张 JPG 卡
  await page.evaluate(() => {
    window.__treeData = {
      mode: 'female_card', root: 'C://KK//female', rootName: 'UserData/chara/female',
      nodes: [
        { id: 'C://KK//female//22.7.16', name: '22.7.16', type: 'folder', path: '',
          mods: [], modNames: [], size: 0, folderStats: { totalFiles: 1, cardCount: 1, subfolderCount: 0, totalSize: 496668 } },
        { id: 'C://KK//female//22.7.16//card.JPG', name: 'card.JPG', type: 'card',
          path: '22.7.16', mods: [], modNames: [], size: 496668 },
        { id: 'C://KK//female//empty', name: 'empty', type: 'folder', path: '',
          mods: [], modNames: [], size: 0, folderStats: { totalFiles: 0, cardCount: 0, subfolderCount: 0, totalSize: 0 } },
      ],
    };
    window.__calls.length = 0;
  });
  await page.evaluate(async () => { await loadLeft(); });
  await page.waitForTimeout(400);

  const rows = await page.locator('#left-list .list-item').count();
  check('含 JPG 的目录树正常渲染', rows === 2, `实际 ${rows} 行`);

  // 进入含 JPG 的文件夹，确认 JPG 行可见
  await page.locator('#left-list .list-item[data-name="22.7.16"]').dblclick();
  await page.waitForTimeout(400);
  const jpgRow = await page.locator('#left-list .list-item[data-name="card.JPG"]').count();
  check('JPG 角色卡出现在列表里', jpgRow === 1, `实际 ${jpgRow} 行`);

  // 右键 JPG：应有「在文件资源管理器中打开」，且不应有「隐藏该空文件夹」
  await page.locator('#left-list .list-item[data-name="card.JPG"]').click({ button: 'right' });
  await page.waitForTimeout(400);
  let items = await page.locator('#context-menu .menu-item').allInnerTexts();
  let joined = items.join('|');
  check('行菜单含「在文件资源管理器中打开」', joined.includes('在文件资源管理器中打开'), joined);
  check('文件行菜单不含「隐藏该空文件夹」', !joined.includes('隐藏该空文件夹'), joined);

  // 点它 → 应传该 JPG 的绝对路径给后端
  const openItem = page.locator('#context-menu .menu-item', { hasText: '在文件资源管理器中打开' });
  await openItem.click();
  await page.waitForTimeout(300);
  const revealCalls = await page.evaluate(() => window.__calls.filter(c => c[0] === 'OpenFolderInExplorer'));
  check('打开项传的是焦点条目的绝对路径', revealCalls.length === 1 && String(revealCalls[0][1]).includes('card.JPG'), JSON.stringify(revealCalls));

  // 收起菜单，避免遮挡后续点击
  await page.evaluate(() => hideContextMenu());
  await page.waitForTimeout(150);

  // 返回上级，右键「非空文件夹」不应出现隐藏项
  await page.locator('#btn-back-left').click();
  await page.waitForTimeout(400);
  await page.locator('#left-list .list-item[data-name="22.7.16"]').click({ button: 'right' });
  await page.waitForTimeout(400);
  items = await page.locator('#context-menu .menu-item').allInnerTexts();
  joined = items.join('|');
  check('非空文件夹不出现「隐藏该空文件夹」', !joined.includes('隐藏该空文件夹'), joined);
  check('非空文件夹仍有「在文件资源管理器中打开」', joined.includes('在文件资源管理器中打开'), joined);

  // 空文件夹才显示隐藏项
  await page.evaluate(() => hideContextMenu());
  await page.waitForTimeout(150);
  await page.locator('#left-list .list-item[data-name="empty"]').click({ button: 'right' });
  await page.waitForTimeout(400);
  items = await page.locator('#context-menu .menu-item').allInnerTexts();
  joined = items.join('|');
  check('空文件夹显示「隐藏该空文件夹」', joined.includes('隐藏该空文件夹'), joined);

  // 清理，避免影响后续
  await page.evaluate(() => { hideContextMenu(); window.__treeData = null; });
}

console.log('\n【24】zipmod 插件引用改为文字说明');
{
  await page.evaluate(() => {
    window.__panelData = {
      focus: { kind: 'mod', name: 'x.zipmod', abs: 'C:/KK/mods/x.zipmod', info: '被 2 个文件占用' },
      sections: [
        { id: 'cards', title: '使用此 zipmod 的卡片', layout: 'grid', total: 1,
          tiles: [{ kind: 'card', name: 'card.JPG', abs: 'C:/KK/female/card.JPG', rel: '', badge: '', thumbKey: 'k', thumb: '', pending: true }],
          more: 0, moreLabel: '', visible: true, empty: '', lazy: true },
        { id: 'plugins', title: '插件引用', layout: 'summary', total: 2,
          tiles: [ { kind: 'text', name: '被 2 个 dll 插件使用' }, { kind: 'text', name: '例如：A.dll、B.dll' } ],
          more: 0, moreLabel: '', visible: true, empty: '', lazy: false },
      ],
      gen: 30,
    };
  });
  await page.evaluate(async () => { await refreshPanel(); });
  await page.waitForTimeout(400);

  const summary = await page.locator('#panel-sections .panel-summary').count();
  check('渲染文字说明区域', summary === 1, `实际 ${summary} 个`);
  const text = summary ? await page.locator('#panel-sections .panel-summary').innerText() : '';
  check('文案含「被 2 个 dll 插件使用」', text.includes('被 2 个 dll 插件使用'), `实际 "${text}"`);
  const pluginRows = await page.locator('#panel-sections .panel-list-row').count();
  check('不再渲染插件图标窗格', pluginRows === 0, `实际 ${pluginRows} 行`);
  const cardCells = await page.locator('#panel-sections .panel-cell').count();
  check('引用卡片（含 JPG）仍显示在区域 A', cardCells === 1, `实际 ${cardCells} 个`);
}

await page.screenshot({ path: path.join(__dirname, '..', 'build', 'frontend-test.png'), fullPage: false });
await browser.close();

console.log(`\n═══ 结果：${passed} 通过 / ${failed} 失败 ═══\n`);
process.exit(failed === 0 ? 0 : 1);
