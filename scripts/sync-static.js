#!/usr/bin/env node
// 跨平台 static/ -> frontend/dist/static/ 镜像同步（rsync 的 Node 替代，
// Windows 没有 rsync）。规则与原 rsync 命令一致：
//   - 镜像：目标中多余的条目删除
//   - 排除：output.css（Tailwind 产物，单独生成，不得覆盖/删除）与 .DS_Store
"use strict";
const fs = require("fs");
const path = require("path");

const SRC = path.join(__dirname, "..", "static");
const DST = path.join(__dirname, "..", "frontend", "dist", "static");
const KEEP_IN_DST = new Set(["output.css"]); // 目标侧保留（构建产物）
const SKIP_IN_SRC = new Set([".DS_Store"]);  // 源侧不拷贝

function mirror(src, dst) {
  fs.mkdirSync(dst, { recursive: true });
  const srcEntries = fs.readdirSync(src, { withFileTypes: true });
  const srcNames = new Set(srcEntries.map((e) => e.name));

  for (const name of fs.readdirSync(dst)) {
    if (KEEP_IN_DST.has(name)) continue;
    if (!srcNames.has(name)) {
      fs.rmSync(path.join(dst, name), { recursive: true, force: true });
    }
  }

  for (const entry of srcEntries) {
    if (SKIP_IN_SRC.has(entry.name)) continue;
    const s = path.join(src, entry.name);
    const d = path.join(dst, entry.name);
    if (entry.isDirectory()) {
      mirror(s, d);
    } else {
      fs.copyFileSync(s, d);
    }
  }
}

mirror(SRC, DST);
console.log("static/ -> frontend/dist/static/ 同步完成");
