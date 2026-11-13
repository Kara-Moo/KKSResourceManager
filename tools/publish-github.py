#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
publish-github.py —— 通过 GitHub Git Data API 发布整个仓库（单 commit）

为什么需要这个脚本：
    某些网络环境下 https://github.com 的 git 智能 HTTP 协议被代理阻断
    （CONNECT tunnel failed / schannel missing close_notify），但
    https://api.github.com 可以访问。此时 `git push` 不可用，
    可以改用 Git Data API 直接构造 blob / tree / commit 并更新 ref。

特点：
    - 只产生 **1 个** 新 commit，父提交为远端 main 当前的 HEAD
    - 用 base_tree 保留远端已有文件（例如 LICENSE），不覆盖未列出的文件
    - 不修改本地 git 历史，也不需要 gh CLI
    - token 只从环境变量读取，绝不写入文件或日志

用法：
    export GITHUB_TOKEN=ghp_xxxxxxxxxxxx        # 需要 repo 写权限
    python tools/publish-github.py --dry-run    # 只检查，不上传
    python tools/publish-github.py              # 执行发布

可选参数：
    --repo owner/name   默认 Kara-Moo/KKSResourceManager
    --branch BRANCH     默认 main
    --message MSG       commit 信息
"""

import argparse
import base64
import json
import os
import subprocess
import sys
import urllib.error
import urllib.request

API = "https://api.github.com"
MAX_BLOB = 99 * 1024 * 1024  # GitHub blob 上限 100MB，留一点余量


def gh(token, method, path, payload=None):
    """调用 GitHub REST API，返回解析后的 JSON。"""
    url = API + path
    data = None
    headers = {
        "Authorization": "Bearer " + token,
        "Accept": "application/vnd.github+json",
        "X-GitHub-Api-Version": "2022-11-28",
        "User-Agent": "KKSResourceManager-publisher",
    }
    if payload is not None:
        data = json.dumps(payload).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=180) as resp:
            body = resp.read()
            return json.loads(body) if body else {}
    except urllib.error.HTTPError as e:
        detail = e.read().decode("utf-8", "replace")[:500]
        raise RuntimeError(f"{method} {path} -> HTTP {e.code}: {detail}") from None


def tracked_files():
    """返回本地 git 追踪的文件列表（相对路径，UTF-8）。"""
    out = subprocess.check_output(
        ["git", "ls-files", "-z"], stderr=subprocess.DEVNULL
    )
    return [p.decode("utf-8") for p in out.split(b"\0") if p]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--repo", default="Kara-Moo/KKSResourceManager")
    ap.add_argument("--branch", default="main")
    ap.add_argument("--message", default="Release: KKS Resource Manager v2.2 (source + Windows build)")
    ap.add_argument("--dry-run", action="store_true", help="只检查，不上传")
    args = ap.parse_args()

    token = os.environ.get("GITHUB_TOKEN") or os.environ.get("GH_TOKEN")
    if not token and not args.dry_run:
        print("错误：请设置环境变量 GITHUB_TOKEN（需要 repo 写权限）", file=sys.stderr)
        return 2

    files = tracked_files()
    total = 0
    for rel in files:
        if os.path.isfile(rel):
            total += os.path.getsize(rel)
    print(f"本地追踪文件 {len(files)} 个，合计 {total/1024/1024:.1f} MB")

    oversized = [f for f in files if os.path.isfile(f) and os.path.getsize(f) > MAX_BLOB]
    if oversized:
        print("超过 GitHub blob 上限的文件：", oversized, file=sys.stderr)
        return 2

    if args.dry_run:
        print("dry-run：文件检查通过，未发起任何网络写操作。")
        return 0

    base = f"/repos/{args.repo}"

    # 1. 取远端分支当前 HEAD
    ref = gh(token, "GET", f"{base}/git/ref/heads/{args.branch}")
    head_sha = ref["object"]["sha"]
    commit = gh(token, "GET", f"{base}/git/commits/{head_sha}")
    base_tree = commit["tree"]["sha"]
    print(f"远端 {args.branch} HEAD = {head_sha[:12]}，base_tree = {base_tree[:12]}")

    # 2. 为每个文件创建 blob
    tree = []
    for i, rel in enumerate(files, 1):
        if not os.path.isfile(rel):
            continue
        with open(rel, "rb") as f:
            raw = f.read()
        blob = gh(token, "POST", f"{base}/git/blobs", {
            "content": base64.b64encode(raw).decode("ascii"),
            "encoding": "base64",
        })
        # Windows 可执行文件不需要 exec 位；统一 100644
        tree.append({
            "path": rel.replace(os.sep, "/"),
            "mode": "100644",
            "type": "blob",
            "sha": blob["sha"],
        })
        if i % 10 == 0 or i == len(files):
            print(f"  blob {i}/{len(files)} …")

    # 3. 构造树（base_tree 保留远端未列出的文件，如 LICENSE）
    new_tree = gh(token, "POST", f"{base}/git/trees", {
        "base_tree": base_tree,
        "tree": tree,
    })
    print(f"新 tree = {new_tree['sha'][:12]}")

    # 4. 创建 commit（父为远端 HEAD，不改写历史）
    new_commit = gh(token, "POST", f"{base}/git/commits", {
        "message": args.message,
        "tree": new_tree["sha"],
        "parents": [head_sha],
    })
    print(f"新 commit = {new_commit['sha'][:12]}")

    # 5. 移动分支指针
    gh(token, "PATCH", f"{base}/git/refs/heads/{args.branch}", {
        "sha": new_commit["sha"],
        "force": False,
    })

    print("\n发布完成：")
    print(f"  https://github.com/{args.repo}/tree/{args.branch}")
    print(f"  可执行文件：https://github.com/{args.repo}/raw/{args.branch}/dist/KKSResourceManager.exe")
    return 0


if __name__ == "__main__":
    sys.exit(main())
