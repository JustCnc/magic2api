#!/usr/bin/env node
/*
 * Export the Magic desktop client's login state into a magic2api account JSON.
 *
 * The client keeps everything in the renderer's localStorage, which Electron
 * stores as a plain (unencrypted) LevelDB under %APPDATA%\Magic. This script
 * scans those files for the two keys the plugin needs and prints a record that
 * the console accepts verbatim.
 *
 *   node scripts/export_credentials.js
 *   node scripts/export_credentials.js > account.json
 *
 * Nothing is uploaded; the file is read locally and only written to stdout.
 */
const fs = require("fs");
const path = require("path");
const os = require("os");

const ORIGIN = "https://www.letsmagic.cn";

function candidateDirs() {
  const home = os.homedir();
  const dirs = [];
  if (process.env.MAGIC_USER_DATA) dirs.push(process.env.MAGIC_USER_DATA);
  if (process.platform === "win32") {
    if (process.env.APPDATA) dirs.push(path.join(process.env.APPDATA, "Magic"));
  } else if (process.platform === "darwin") {
    dirs.push(path.join(home, "Library", "Application Support", "Magic"));
  } else {
    dirs.push(path.join(home, ".config", "Magic"));
  }
  return dirs;
}

function collectFiles(root) {
  const out = [];
  const walk = (dir, depth) => {
    if (depth > 4) return;
    let entries;
    try {
      entries = fs.readdirSync(dir, { withFileTypes: true });
    } catch {
      return;
    }
    for (const entry of entries) {
      const full = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        if (/leveldb|Local Storage|Session Storage/i.test(entry.name) || depth === 0) walk(full, depth + 1);
        continue;
      }
      if (/\.(log|ldb|sst)$/i.test(entry.name)) out.push(full);
    }
  };
  walk(root, 0);
  return out;
}

// localStorage rows are written as {"key":"...","value":"..."} next to the
// origin string, so a plain scan over the raw bytes is enough.
function extract(contents) {
  const found = new Map();
  const re = /\{"key":"([a-zA-Z0-9_:\-.]+)","value":(null|"(?:[^"\\]|\\.)*"|\{[\s\S]*?\}|\[[\s\S]*?\])\}/g;
  let match;
  while ((match = re.exec(contents))) {
    const key = match[1];
    if (!/authorization|organizationCode|teamshareOrganizationCode|userInfo/.test(key)) continue;
    let value = match[2];
    try {
      value = JSON.parse(value);
    } catch {
      continue;
    }
    // Later writes win: the log is append-only.
    found.set(key, value);
  }
  return found;
}

function main() {
  const files = [];
  for (const dir of candidateDirs()) files.push(...collectFiles(dir));
  if (!files.length) {
    console.error("找不到 Magic 客户端的 Local Storage，可用 MAGIC_USER_DATA 指定 userData 目录");
    process.exit(2);
  }
  const state = new Map();
  for (const file of files) {
    let raw;
    try {
      raw = fs.readFileSync(file, "latin1");
    } catch {
      continue;
    }
    for (const [key, value] of extract(raw)) state.set(key, value);
  }
  const authorization = state.get("authorization") || "";
  if (!authorization) {
    console.error("没有找到登录态：请先在 Magic 客户端里登录一次（扫描了 " + files.length + " 个文件）");
    process.exit(3);
  }
  const record = {
    type: "magic2api",
    provider: "magic2api",
    alias: "desktop",
    authorization,
    organization_code: state.get("organizationCode") || state.get("teamshareOrganizationCode") || "",
    note: "exported from " + ORIGIN,
  };
  const claims = decodeJWT(authorization);
  if (claims && claims.exp) {
    record.authorization_expires_at = new Date(claims.exp * 1000).toISOString();
  }
  process.stdout.write(JSON.stringify(record, null, 2) + "\n");
  console.error("已导出登录态（JWT 有效至 " + (record.authorization_expires_at || "未知") + "）");
}

function decodeJWT(token) {
  const parts = token.split(".");
  if (parts.length < 2) return null;
  try {
    const payload = parts[1].replace(/-/g, "+").replace(/_/g, "/");
    const padded = payload + "=".repeat((4 - (payload.length % 4)) % 4);
    return JSON.parse(Buffer.from(padded, "base64").toString("utf8"));
  } catch {
    return null;
  }
}

main();
