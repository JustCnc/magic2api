# 发布说明（Agent Runbook）

本文件给后续 agent / 维护者使用。目标：把 `magic2api` 的新版本发布到 GitHub Release，
让 CPA 插件商店能安装/更新，并保持 `cpa_plugins` registry 一致。

## 0. 先看结论

- 版本唯一来源是 `schema.go` 里的 `pluginVersion`，Git tag 必须是 `v<pluginVersion>`。
- 插件商店读取**最新 GitHub Release**；只推 tag 不发 Release，商店无法安装/更新。
- Release 必须是正式版：不要 draft，不要勾选 prerelease。
- Release 资产名、zip 结构、`checksums.txt` 由 workflow 自动生成，不要手工改名。
- registry 的 `version` 只是展示兜底；正常发版不需要改 registry，但建议同步更新。

## 1. 关键文件与地址

| 项 | 值 |
| --- | --- |
| 插件项目 | `C:/Users/test/Desktop/cpa/magic2api` |
| registry 项目 | `C:/Users/test/Desktop/cpa/cpa_plugins` |
| 插件仓库 | `https://github.com/JustCnc/magic2api` |
| registry 仓库 | `https://github.com/JustCnc/cpa_plugins` |
| registry raw URL | `https://raw.githubusercontent.com/JustCnc/cpa_plugins/main/registry.json` |
| Release workflow | `.github/workflows/release.yml` |
| 插件 ID / provider key | `magic2api` |
| 支持平台 | `linux/amd64`、`linux/arm64`、`windows/amd64` |

本机路径（其他环境按实际调整）：

| 用途 | 路径 |
| --- | --- |
| CPA 核心目录 | `C:/Users/test/Downloads/EasyCLIProxyAPI-v0.3.53-Windows-amd64/cpa-core` |
| CPA 配置 | `cpa-core/config.yaml` |
| 插件数据 | `cpa-core/plugin-data/magic2api` |
| 本地 Go | `/c/go/bin` |
| 本地 mingw | `/c/mingw64/bin` |
| 管理 API | `http://127.0.0.1:8317`（请求头 `X-Management-Key`） |

## 2. 发版前检查

先把版本号改好，再提交、打 tag。顺序反了会导致 tag 指向旧代码。

- [ ] 改 `schema.go`：

  ```go
  pluginVersion = "0.1.4"
  ```

- [ ] 如果作者/仓库信息变了，同步 `plugin.go` 的 `Author`、`GitHubRepository`。
- [ ] 如果配置项变了，更新 `README.md` 与 `config.example.yaml`。
- [ ] 如果上游客户端升级了，重新核对协议文档里的域名、路径、请求头（本地文档，不入仓库）。
- [ ] 确认 `cpa_plugins/registry.json` 的 `id` 仍是 `magic2api`，`repository` 仍是
      `https://github.com/JustCnc/magic2api`。

本地检查：

```bash
cd C:/Users/test/Desktop/cpa/magic2api
export PATH="/c/go/bin:/c/mingw64/bin:$PATH"

gofmt -l .             # 应该没有输出
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test ./...
git status --short
git diff --check
```

本地编译 Windows DLL 做冒烟测试（产物在 `dist/`，已被 `.gitignore` 忽略）：

```bash
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc \
  go build -buildmode=c-shared -trimpath -ldflags='-s -w' \
  -o dist/windows/amd64/magic2api.dll .
```

**不要提交** `dist/`、`*.exe`、`*.dll`、`*.so`、`*.h`、`accounts.json`、cookie、token、管理密钥。
`.gitignore` 已覆盖这些，但提交前仍要看一眼 `git status`。

## 3. 提交并打 tag

```bash
cd C:/Users/test/Desktop/cpa/magic2api

VERSION=0.1.4
TAG="v$VERSION"

git add -A
git commit -m "release: $TAG"
git push origin main

git tag -a "$TAG" -m "$TAG"
git push origin "$TAG"
```

规则：

- tag 必须指向版本号已经更新过的那个 commit。
- tag 和 `pluginVersion` 必须完全一致；workflow 在不一致时直接失败。
- 已发布的 tag 不要移动、不要复用；需要修复就发新 patch 版本。
- workflow 只接受点分数字 tag（`v0.1.4`），不接受 `v0.1.4-beta`。

## 4. 发布 GitHub Release

### 方式 A：agent 有 `gh` 授权

```bash
gh auth status

gh release create "$TAG" \
  --repo JustCnc/magic2api \
  --verify-tag \
  --title "$TAG" \
  --generate-notes
```

不要加 `--draft` 或 `--prerelease`。正文留空也可以，workflow 会在正文为空时写入自动生成的更新内容。

### 方式 B：人工网页

1. 打开 https://github.com/JustCnc/magic2api/releases
2. **Draft a new release** → 选择刚推的 tag
3. **不要**勾选 prerelease，直接 **Publish release**

### 方式 C：重跑已有 tag 的构建

```bash
gh workflow run release.yml \
  --repo JustCnc/magic2api \
  -f tag="$TAG"
```

## 5. 等待并核对 Release 资产

```bash
gh run list --repo JustCnc/magic2api --workflow release.yml --limit 5
gh run watch <run-id> --repo JustCnc/magic2api

gh release view "$TAG" --repo JustCnc/magic2api \
  --json tagName,isDraft,isPrerelease,assets
```

一次成功发布应包含（`X.Y.Z` 为不带 `v` 的版本）：

```text
magic2api_X.Y.Z_linux_amd64.zip
magic2api_X.Y.Z_linux_arm64.zip
magic2api_X.Y.Z_windows_amd64.zip
checksums.txt
magic2api-X.Y.Z-linux-amd64.so
magic2api-X.Y.Z-linux-arm64.so
magic2api-X.Y.Z-windows-amd64.dll
magic2api-X.Y.Z-linux.tar.gz
SHA256SUMS
RELEASE_NOTES.md
```

下载并校验：

```bash
rm -rf /c/tmp/magic-release
gh release download "$TAG" --repo JustCnc/magic2api --dir /c/tmp/magic-release --clobber

cd /c/tmp/magic-release
sha256sum -c checksums.txt

for z in magic2api_*.zip; do
  echo "== $z"
  unzip -l "$z"
  count=$(unzip -Z1 "$z" | grep -Ec '\.(so|dll)$' || true)
  test "$count" -eq 1
done
```

检查点：

- `checksums.txt` 只包含三个 zip，且 hash 全部通过。
- 每个 zip 的**根目录**只有 `magic2api.so` / `magic2api.dll`，不能有子目录或多个库。
- Release 不是 draft、不是 prerelease，并且是最新 Release。
- tag 是 `vX.Y.Z`，资产里的版本是 `X.Y.Z`（没有 `v`）。

## 6. 本机安装并实测

```bash
cd /c/tmp/magic-release
unzip -o magic2api_X.Y.Z_windows_amd64.zip -d /c/tmp/magic-release/bin

CPA="C:/Users/test/Downloads/EasyCLIProxyAPI-v0.3.53-Windows-amd64/cpa-core"
cp /c/tmp/magic-release/bin/magic2api.dll "$CPA/plugins/windows/amd64/"

# 重启 core（只杀 core 进程，不要关 GUI）
PID=$(netstat -ano | grep ":8317" | grep LISTENING | awk '{print $5}' | head -1)
taskkill //F //PID "$PID"
cd "$CPA" && (nohup ./cli-proxy-api.exe -config "$CPA/config.yaml" > magic2api-run.log 2>&1 &)
sleep 10
grep "magic2api" magic2api-run.log | head -3   # 必须看到 version=X.Y.Z
```

冒烟测试（把 `<key>` 换成管理密钥、`<api>` 换成 api-keys 里的一个）：

```bash
curl -s -H "X-Management-Key: <key>" http://127.0.0.1:8317/v0/management/magic2api/overview
curl -s -H "Authorization: Bearer <api>" http://127.0.0.1:8317/v1/models \
  | node -e "let d='';process.stdin.on('data',c=>d+=c).on('end',()=>console.log((JSON.parse(d).data||[]).filter(m=>m.id.startsWith('magic/')).length))"
curl -s -X POST http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer <api>" -H "Content-Type: application/json" \
  -d '{"model":"magic/glm-5.3-flash","messages":[{"role":"user","content":"say OK"}],"max_tokens":16}'
```

## 7. 更新 registry

```bash
cd C:/Users/test/Desktop/cpa/cpa_plugins

# 1) 子模块指到新的 release tag commit
cd magic2api
git remote set-url origin git@github.com:JustCnc/magic2api.git   # 若 HTTPS 握手失败就用 SSH
git fetch --tags origin
git checkout "$(git rev-list -n 1 vX.Y.Z)"
cd ..

# 2) registry.json 里的 version 同步成 X.Y.Z（不带 v）
node -e "
const fs=require('fs');
const r=JSON.parse(fs.readFileSync('registry.json','utf8'));
for(const p of r.plugins) if(p.id==='magic2api') p.version='X.Y.Z';
fs.writeFileSync('registry.json', JSON.stringify(r,null,2)+'\n');
"

git add -A
git commit -m "magic2api: X.Y.Z"
git push
```

商店刷新有 CDN 延迟（几分钟），不需要重启 CPA。

## 8. 常见故障

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| workflow 失败：`tag does not match pluginVersion` | 先打 tag 后改版本，或忘了改 `schema.go` | 改版本 → 删 tag → 重打（未发布时）或发新 patch |
| workflow 失败：`must contain exactly one dynamic library` | zip 里带了子目录或 `.h` | 检查 workflow 的 `zip -j` 与 `rm -f *.h` |
| 符号校验失败 | `abi.go` 的 build tag 或导出名被改坏 | 本地 `nm -D dist/.../*.so | grep cliproxyPlugin` 复现 |
| 商店看不到新版本 | 发成了 draft/prerelease，或资产缺失 | 改为正式 Release；必要时 `gh workflow run` 重跑 |
| 装上新 DLL 但 CPA 里还是旧版本 | core 没重启 | 按第 6 节重启 core，确认日志里的 version |
| `git fetch` 报 schannel handshake | 代理/证书问题 | 子模块 remote 换成 `git@github.com:...` |
