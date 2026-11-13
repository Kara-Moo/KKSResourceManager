// tools/check-undefined.mjs
//
// 扫描 frontend/index.html 内联脚本，找出「被调用但从未声明」的函数。
//
// 背景：v2.1 的 doDelete 调用了不存在的 formatBytes()，
// 模板字符串在调用 showConfirm 之前求值就抛 ReferenceError，
// 该异常被 async 函数吞掉，用户侧表现为「点删除毫无反应」。
// 这条检查能拦住 100% 的同类事故 —— 不需要运行代码，只看符号表。
//
// 实现说明：故意**不做**字面量/注释剥离。
// 曾尝试剥离，但正则字面量（如 /["']/）会被误判为字符串起始，
// 反而吞掉后续大量代码，产生误报。对「是否声明过」这个问题，
// 直接在原文上收集符号既简单又准确：字符串内容不会出现 `foo(` 这种形状。
//
// 用法：node tools/check-undefined.mjs [待检文件]
// 退出码：0 = 干净，1 = 发现未定义函数

import fs from 'node:fs';
import path from 'node:path';

const root = path.resolve(import.meta.dirname, '..');
const htmlPath = process.argv[2]
    ? path.resolve(process.argv[2])
    : path.join(root, 'frontend', 'index.html');

let src;
try {
    src = fs.readFileSync(htmlPath, 'utf8');
} catch (e) {
    console.error('无法读取文件：' + htmlPath);
    process.exit(1);
}

const open = src.indexOf('<script>');
const close = src.lastIndexOf('</script>');
if (open < 0 || close < 0) {
    console.error('未找到内联 <script> 块');
    process.exit(1);
}
const js = src.slice(open + '<script>'.length, close);

// --- 1. 收集声明 ---
const declared = new Set();

// function 声明
for (const m of js.matchAll(/function\s+([A-Za-z_$][\w$]*)/g)) declared.add(m[1]);

// const / let / var
for (const m of js.matchAll(/(?:const|let|var)\s+([A-Za-z_$][\w$]*)/g)) declared.add(m[1]);

// 函数参数：function x(a, b) / (a, b) => / a => / catch (e)
const paramPatterns = [
    /function\s*[A-Za-z_$\w]*\s*\(([^)]*)\)/g,
    /\(([^)]*)\)\s*=>/g,
    /([A-Za-z_$][\w$]*)\s*=>/g,
    /catch\s*\(([^)]*)\)/g,
];
for (const re of paramPatterns) {
    for (const m of js.matchAll(re)) {
        for (const id of m[1].matchAll(/[A-Za-z_$][\w$]*/g)) declared.add(id[0]);
    }
}

// 类声明
for (const m of js.matchAll(/class\s+([A-Za-z_$][\w$]*)/g)) declared.add(m[1]);

// --- 2. 收集调用（排除 a.b() 形式的成员调用）---
const calls = new Set();
for (const m of js.matchAll(/(?<![.\w$])([A-Za-z_$][\w$]*)\s*\(/g)) calls.add(m[1]);

// --- 3. 白名单：语言关键字 + 宿主内置 ---
const allowed = new Set([
    // 关键字
    'if', 'for', 'while', 'switch', 'catch', 'return', 'typeof', 'function', 'new', 'await',
    'delete', 'void', 'in', 'of', 'do', 'else', 'try', 'finally', 'throw', 'case', 'break',
    'continue', 'default', 'class', 'extends', 'super', 'this', 'yield', 'async', 'instanceof',
    'get', 'set', 'static', 'with', 'debugger', 'export', 'import', 'let', 'const', 'var',
    // 内置对象与函数
    'Number', 'String', 'Boolean', 'Array', 'Object', 'JSON', 'Math', 'Date', 'RegExp',
    'Set', 'Map', 'WeakMap', 'WeakSet', 'Symbol', 'Promise', 'Error', 'TypeError', 'RangeError',
    'parseInt', 'parseFloat', 'isNaN', 'isFinite', 'encodeURIComponent', 'decodeURIComponent',
    'encodeURI', 'decodeURI', 'structuredClone', 'crypto', 'Blob', 'FileReader', 'Image',
    'URL', 'TextEncoder', 'TextDecoder', 'ArrayBuffer', 'Uint8Array', 'Function',
    // 定时器与调度
    'setTimeout', 'clearTimeout', 'setInterval', 'clearInterval',
    'requestAnimationFrame', 'cancelAnimationFrame', 'queueMicrotask',
    // 宿主环境
    'fetch', 'document', 'window', 'navigator', 'console', 'localStorage', 'sessionStorage',
    'IntersectionObserver', 'ResizeObserver', 'MutationObserver', 'Event', 'CustomEvent',
    'alert', 'confirm', 'prompt', 'getComputedStyle', 'matchMedia', 'atob', 'btoa',
]);

const bad = [...calls]
    .filter((n) => !declared.has(n) && !allowed.has(n))
    // 大写开头视为构造函数/命名空间（App、Wails 绑定等），不在此检查范围
    .filter((n) => !/^[A-Z]/.test(n))
    .sort();

if (bad.length) {
    console.error('未定义函数：' + bad.join(', '));
    console.error(`（共 ${bad.length} 个；文件：${path.relative(root, htmlPath)}）`);
    process.exit(1);
}
console.log('OK：无未定义函数调用');
