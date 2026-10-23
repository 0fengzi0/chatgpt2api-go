# chatgpt2api-go

这是 `feat/go-reimplementation` 分支上的轻量 Go 实现。它保留上游 Python 项目在 `main`
分支不动，后续可以独立同步上游变更。

当前目标是单操作员、低内存部署：

- Go 标准库 HTTP 服务
- SQLite 保存账号、任务和图片索引
- 本地图片文件存储，签名 URL 下载
- 单管理密钥访问管理端，独立客户端 API Key
- 默认单图片 worker、单账号并发

## 构建

需要 Go 1.26 或兼容版本：

```bash
go build -trimpath -ldflags="-s -w" -o bin/chatgpt2api ./cmd/chatgpt2api
```

Linux `amd64` 和 `arm64` 包放在 `dist/`，包含主服务、部署文件和
`SHA256SUMS.txt`。Windows 构建机可运行
`powershell -ExecutionPolicy Bypass -File scripts/build_go_linux.ps1`
重新生成两个架构的 tar.gz 和校验和。

## 启动

```bash
cp config.example.yaml config.yaml
./bin/chatgpt2api -config config.yaml
```

`data-dir` 和 `auth-dir` 支持相对路径，并相对配置文件所在目录解析。例如配置文件在
`/www/wwwroot/chatgpt2api/config.yaml` 时，`./data` 会指向
`/www/wwwroot/chatgpt2api/data`。

Linux + systemd 部署可直接使用 [deploy/chatgpt2api.service](deploy/chatgpt2api.service)。
服务的工作目录是 `/var/lib/chatgpt2api`，配置从 `/etc/chatgpt2api/config.yaml`
读取。systemd 场景建议在配置里填写绝对路径
`/var/lib/chatgpt2api/data`，避免数据写入只读的 `/etc` 目录。
完整安装步骤见 [deploy/README.md](deploy/README.md)。

第一次启动会生成管理密钥和默认客户端 API Key，并在 stdout 打印一次。配置文件中只会
保存它们的哈希。管理端地址默认为：

```text
http://127.0.0.1:3000/admin/
```

管理请求使用：

```http
X-Management-Key: <management-key>
```

## 认证模型

这是单操作员服务，没有用户表、角色、租户或管理端 Session。启动时只加载一个
`remote-management.secret-key`，它同时代表管理员身份。配置文件中只保存 bcrypt 哈希；
把明文写进配置文件时，进程启动后会就地替换为哈希。

管理页面不创建 `admin_sessions`。浏览器只在当前标签页的 `sessionStorage` 中保存管理密钥，
每次管理 API 请求都通过 `X-Management-Key`（或 `Authorization: Bearer`）发送。关闭标签页
即丢失该前端状态；服务端没有需要清理的会话记录，也不需要额外的会话数据库。

`GET/PUT /api/access/settings` 对应 `config.yaml` 或 `config.json` 中的以下字段，修改后
原子写回同一份配置文件，重启后仍然生效：

```yaml
port: 3000
proxy-url: "http://127.0.0.1:10808"
remote-management:
  secret-key: "$2a$..."
```

使用 `config.json` 时字段名改为下划线形式，例如 `remote_management.secret_key`。
`proxy-url` 保存后会立即用于新的上游请求，并触发内置 Solver 浏览器重建；端口修改仍需重启进程。

管理接口始终要求有效管理密钥。是否暴露到远程由监听地址、防火墙或反向代理决定。

浏览器跨域白名单配置在 `cors.allowed-origins`。默认允许 `5ixf.com`、`isancha.com`
及其 HTTPS 子域：

```yaml
cors:
  allowed-origins:
    - "https://5ixf.com"
    - "https://www.5ixf.com"
    - "https://*.5ixf.com"
    - "https://isancha.com"
    - "https://www.isancha.com"
    - "https://*.isancha.com"
```

服务会回显请求的 `Origin` 并添加 `Vary: Origin`，不会使用不安全的固定 `*`。

客户端 API Key 与管理密钥分离，只用于 `/v1/*` 和任务查询。API Key 以 SHA-256 哈希保存，
在管理页创建时只显示一次明文。

客户端调用 `/v1/*` 使用：

```http
Authorization: Bearer <api-key>
```

如果丢失了管理密钥，可以在本机重新设置，不需要清空账号数据：

```bash
./bin/chatgpt2api -config config.yaml -set-management-key "new-strong-key"
```

从现有 Python 版本迁移账号和普通用户 Key：

```bash
./bin/chatgpt2api -config config.yaml -migrate-legacy
```

手动执行一次过期图片清理：

```bash
./bin/chatgpt2api -config config.yaml -cleanup-images
```

## 运行前准备

服务固定使用 ChatGPT Web 上游生成真实图片。首次运行后，在管理页“账号池”导入
Access Token 或完整账号 JSON，再提交图片任务。

2026-10 起上游还要求 Turnstile token 由真实 chatgpt.com 页面计算。主服务现在直接内置
Go 浏览器 Solver，不再需要外部 solver、Python、Playwright、Node 或 Docker：

```yaml
sentinel:
  browser-path: ""
  timezone: "Asia/Shanghai"
  headless: true
  no-sandbox: true
```

内置 Solver 仍使用系统 Chromium、Chrome 或 Edge，启动时会自动检测；也可以设置
`sentinel.browser-path` 或环境变量 `SENTINEL_BROWSER_BIN`。`proxy-url` 会同时用于
上游 HTTP 请求和 Solver 浏览器，并可在管理端“设置”页修改；修改后下一次请求会重建浏览器页面。

实测内置 Solver 可以完成 `/chat-requirements/prepare`、真实页面 Turnstile 求解、
`finalize` 和生图全链路。2026-10-22 使用 Edge 155 连续两次生图成功。Chromium 的驻留内存
仍然无法消除，低配服务器建议给浏览器留 500-800 MB，1 GB 主机更稳妥。

## 主要接口

- `GET /healthz`
- `GET /api/status`
- `GET/POST/DELETE /api/accounts`
- `POST /api/accounts/import-dir`
- `POST /api/accounts/import-json`
- `POST /api/accounts/export`
- `POST /api/accounts/{id}/refresh`
- `POST /api/accounts/refresh`
- `GET/POST/PATCH/DELETE /api/access/keys`
- `GET/PUT /api/access/settings`
- `GET /api/models`
- `POST /api/image-tasks/generations`
- `GET /api/image-tasks`
- `GET /api/images`
- `POST /api/images/delete`
- `GET /v1/models`
- `POST /v1/images/generations`
- `POST /v1/images/edits`
- `GET /images/{signed-path}`

生图页和模型接口只提供两个固定图片模型别名：

```text
gpt-image-2
gpt-image-2.5
```

两个别名都不会作为上游对话模型直接发送，而是映射到 `default-upstream-model` 并使用
`system_hints: ["picture_v2"]`。目前上游没有公开独立的 `gpt-image-2.5` 图片通道，因此
`gpt-image-2.5` 暂时与 `gpt-image-2` 使用相同的 `picture_v2` 链路。其他对话模型会由
provider 明确拒绝，不再出现在 `/api/models` 或 `/v1/models` 中。

## 低配建议

- 使用 systemd 直接运行二进制，不建议在低内存机器上额外常驻 Docker、Node 或 Redis。
- 保持 `performance.image-workers: 1` 和 `parallel-generation: false`。
- 默认返回图片 URL，不要默认使用 `b64_json`。
- 不在应用内执行全量图片备份，使用外部 cron 和对象存储。
- 图片 worker 的 timeout 和上游轮询参数都在 `config.yaml` 中。

## 图片保留

默认保留 7 天，进程内清理器每 6 小时检查一次。使用宝塔面板时，也可以把清理任务交给
面板的计划任务执行，调用内置命令而不是直接删文件，这样 SQLite 记录会同步删除：

```bash
/www/wwwroot/chatgpt2api/chatgpt2api -config /www/wwwroot/chatgpt2api/config.yaml -cleanup-images
```

仓库附带的 [deploy/cleanup-images.sh](deploy/cleanup-images.sh) 可直接作为宝塔 shell 计划任务使用，
建议每天执行一次：

```bash
APP_DIR=/www/wwwroot/chatgpt2api /bin/sh /www/wwwroot/chatgpt2api/deploy/cleanup-images.sh
```

## 当前限制

- ChatGPT provider 依赖上游 Web 接口和 Sentinel/PoW，仍需要继续用真实账号验证和适配。
- 内置 Solver 没有 `vm.js` 兼容回退；页面自带的 Turnstile 模块发现失败时会重建页面并重试。
- `codex-gpt-image-2` 尚未接入轻量 provider。
- PPT/PSD 可编辑文件生成暂未进入 MVP。
- 账号密码登录恢复、批量导出和完整 CPA/Sub2API 导入尚未实现。
- 账号 Token 使用 AES-GCM 加密后保存；丢失 `signing-key` 将无法解密已有账号，请把配置文件作为敏感备份保存。
