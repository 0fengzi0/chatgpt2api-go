# ChatGPT2API 生图网关需求规格

版本：1.0

日期：2026-10-25

状态：已确认，首版已实现

## 1. 文档目的

本文定义 `chatgpt2api-go` 下一阶段的产品边界、OpenAI 兼容接口、账号池调度、管理端、运行配置和验收标准。

目标是将当前 Go 实现收敛为一个可独立部署的生图 API 网关。外部画图前端、图片资产管理和多租户额度由外部系统负责。

## 2. 产品定位

本产品是一个单机、单运营者、面向程序调用的 ChatGPT 官网生图网关。

核心职责：

- 提供 OpenAI 风格的 GPT Image 接口。
- 管理 ChatGPT 官网账号池。
- 调度账号执行图片生成和图片编辑。
- 把成功图片编码为 Base64 或 10 分钟临时 URL。
- 提供多 API Key、运行统计、日志和极简管理端。
- 为后续 Banana 生图渠道保留 Provider 扩展能力。

部署形态：

- Linux `amd64` 单二进制。
- SQLite 本地持久化。
- 依赖服务器安装 Chrome、Edge 或 Chromium，用于内置 Turnstile Solver。
- 默认由 Systemd 管理，不要求 Docker、Redis 或外部数据库。

## 3. 范围

### 3.1 保留

- OpenAI GPT Image 生成接口。
- OpenAI GPT Image 编辑接口。
- OpenAI 模型列表和单模型查询。
- 官网 `picture_v2` 生图链路。
- Access Token 和账号 JSON 导入。
- 多账号池、额度管理、Token 刷新、代理和并发控制。
- 同一上游链路下的多个 GPT Image 模型名称。
- Base64 和短期签名 URL 输出。
- 图片流式响应。
- 多个网关 API Key。
- 极简管理端。
- 调用日志、账号日志、运行统计和监控指标。
- 未来 Banana Provider 接口预留。

### 3.2 移除

- Codex 图片链路。
- 文本对话、Responses、Chat Completions、Anthropic Messages。
- Web Search。
- PPT 和 PSD 生成。
- 在线画图工作台和图片编辑画布。
- 图片资产管理、图片标签、下载中心、缩略图和历史图库。
- 多租户、用户角色、团队空间和 API Key 级配额。
- OAuth 登录、CPA 导入、Sub2API 导入。
- 外部 FlareSolverr 和手工 Cookie 清障模式。
- 内建 R2 备份和浏览器式备份管理。
- Banana 实际生成链路。

### 3.3 代码边界

- 继续使用当前分支 `feat/go-reimplementation`。
- 原 Python 后端保留作为参考；原 Next.js 管理端已移除，Go 网关使用内置管理控制台。
- 实际运行和发布以 Go 二进制为准。
- 删除或停用 Go 内嵌的生图、图片资产、文本、搜索、PPT、PSD 页面及对应路由。

## 4. OpenAI 协议范围

### 4.1 公共接口

| 方法 | 路径 | 协议 |
| --- | --- | --- |
| `GET` | `/v1/models` | 返回可用的 GPT Image 模型 |
| `GET` | `/v1/models/{model}` | 返回单个模型信息 |
| `POST` | `/v1/images/generations` | JSON 文生图 |
| `POST` | `/v1/images/edits` | Multipart 图片编辑 |
| `GET` | `/healthz` | 进程存活检查 |
| `GET` | `/readyz` | 数据库、账号池和 Solver 就绪检查 |
| `GET` | `/metrics` | Prometheus 指标 |

认证方式：

```http
Authorization: Bearer <gateway-api-key>
```

`/healthz`、`/readyz` 和 `/metrics` 默认不要求 API Key。如果通过公网暴露，应由反向代理限制访问。

### 4.2 模型名称

首版对外暴露以下模型名称：

- `gpt-image-2`
- `gpt-image-2.5`
- `gpt-image-2.5-sunburst`
- `gpt-image-2.5-flare`

其中：

- `gpt-image-2.5` 是兼容原项目的便捷别名。
- `gpt-image-2.5-sunburst` 和 `gpt-image-2.5-flare` 是官方当前名称。
- 四个名称当前全部路由到同一个 ChatGPT 官网 `picture_v2` 链路。
- 请求中的模型名称只用于协议、日志和返回元数据，不表示底层可切换到四个真实模型。
- 实际底层模型由 ChatGPT 官网内部决定，调用方不能通过网关强制选择。
- 后续实现 Banana 时增加独立 Provider，不改变现有接口。

### 4.3 `POST /v1/images/generations`

请求格式：

```http
Content-Type: application/json
Authorization: Bearer <gateway-api-key>
```

支持的请求字段：

| 字段 | 类型 | 规则 |
| --- | --- | --- |
| `model` | string | 必填，值必须在模型列表中 |
| `prompt` | string | 必填，去除首尾空白后不能为空 |
| `n` | integer | 默认 `1`，范围 `1-4` |
| `size` | string | `auto` 或 `宽x高`，默认 `auto` |
| `quality` | string | `auto`、`low`、`medium`、`high`；2.5 名称额外接受 `xhigh`、`max` |
| `background` | string | `auto`、`opaque`、`transparent` |
| `moderation` | string | `auto`、`low` |
| `output_format` | string | `png`、`jpeg`、`webp` |
| `output_compression` | integer | 范围 `0-100`，仅 `jpeg` 和 `webp` 有效 |
| `response_format` | string | `b64_json` 或 `url`，默认 `b64_json` |
| `stream` | boolean | 默认 `false` |
| `partial_images` | integer | 范围 `0-3`，仅在 `stream=true` 时有效 |
| `safety_identifier` | string | 可选，最大 255 字符，用于日志关联 |

不支持或无法兼容的官方字段：

- 必须返回 OpenAI 风格 `400 unsupported_parameter`。
- 不允许静默忽略。
- 对于 `gpt-image-2`，传入 `input_fidelity` 返回 `400 unsupported_parameter`。

尺寸规则：

- `auto` 不附加尺寸提示。
- `宽x高` 必须为正整数。
- 默认单边最大 `4096`，总像素最大 `16,777,216`，可在配置中调整。
- 为完整支持 1K/2K 预设，配置加载时会把单边下限提升到 `3840`，总像素下限提升到 `8,294,400`。
- 尺寸会作为上游提示词和生成参数使用。
- 接口返回实际输出尺寸，不承诺官网链路严格输出请求尺寸。

网页端内置尺寸使用 16 像素倍数，覆盖以下比例：

- 1K：`1024x1024`、`1536x1024`、`1024x1536`、`1824x1024`、`1024x1824`、`1360x1024`、`1024x1360`、`2384x1024`、`1024x2384`。
- 2K：`2048x2048`、`2160x1440`、`1440x2160`、`2560x1440`、`1440x2560`、`1920x1440`、`1440x1920`、`3360x1440`、`1440x3360`。
- `随原图` 发送 `size=auto`，由上游保持参考图比例。

质量规则：

- 质量会转换为上游提示词和可用参数。
- 接口返回请求质量，实际视觉效果由官网链路决定。

### 4.4 `POST /v1/images/edits`

请求格式：

```http
Content-Type: multipart/form-data
Authorization: Bearer <gateway-api-key>
```

支持的请求字段：

| 字段 | 类型 | 规则 |
| --- | --- | --- |
| `image[]` | file[] | 必填，1 到 4 张 |
| `mask` | file | 可选，PNG，最多 1 张 |
| `model` | string | 必填，值必须在模型列表中 |
| `prompt` | string | 必填，去除首尾空白后不能为空 |
| `n` | integer | 默认 `1`，范围 `1-4` |
| `size` | string | 与生成接口一致 |
| `quality` | string | 与生成接口一致 |
| `background` | string | 与生成接口一致 |
| `moderation` | string | 与生成接口一致 |
| `output_format` | string | 与生成接口一致 |
| `output_compression` | integer | 与生成接口一致 |
| `response_format` | string | 与生成接口一致 |
| `stream` | boolean | 默认 `false` |
| `partial_images` | integer | 范围 `0-3` |
| `safety_identifier` | string | 可选 |

图片限制：

- 允许格式：PNG、JPEG、WebP。
- 单张最大 25 MiB。
- 最多 4 张参考图。
- 上传后必须使用图片解码器验证真实格式、宽高和像素数，不能只信任文件名或 Content-Type。
- Mask 会合成为图片透明通道后上传给上游。
- 不接受 JSON 中的任意远程图片 URL。

### 4.5 成功响应

非流式生成和编辑统一返回 OpenAI 风格对象：

```json
{
  "created": 1780000000,
  "background": "opaque",
  "data": [
    {
      "b64_json": "..."
    }
  ],
  "output_format": "png",
  "quality": "high",
  "size": "1024x1024",
  "usage": {
    "input_tokens": 0,
    "input_tokens_details": {
      "image_tokens": 0,
      "text_tokens": 0
    },
    "output_tokens": 0,
    "total_tokens": 0
  }
}
```

规则：

- `response_format=b64_json` 时，每个 `data` 项包含 `b64_json`。
- `response_format=url` 时，每个 `data` 项包含 `url`，不得同时返回长期公开地址。
- `revised_prompt` 可选返回；如果上游只返回原提示词，可以返回原提示词。
- `usage` 使用本地估算值，保持标准字段结构，并在内部标记为估算数据。
- 响应头包含 `X-Request-ID`。

### 4.6 临时 URL

URL 响应规则：

- 有效期固定为 10 分钟，可通过运行配置调整。
- URL 使用 HMAC 签名，不要求 API Key。
- URL 只能下载对应文件，不暴露文件系统路径。
- 文件存放在本地临时目录，不写入长期图片资产表。
- 后台清理器每分钟删除过期文件。
- 服务重启后，未过期文件仍可访问。
- `b64_json` 模式不落盘。
- `public-base-url` 未配置时，使用请求 Host 生成 URL；生产环境要求显式配置。

### 4.7 流式响应

当 `stream=true`：

- 使用 `text/event-stream`。
- 事件格式遵循 OpenAI 图片流式事件。
- `partial_images=0` 时只返回最终完成事件。
- `partial_images` 大于 0 时，最多返回请求数量的中间帧。
- 官网链路拿不到中间帧时允许只返回最终完成事件，不伪造中间图片。
- 流结束前必须发送完成事件或标准错误事件。

### 4.8 错误格式

统一错误结构：

```json
{
  "error": {
    "message": "descriptive message",
    "type": "invalid_request_error",
    "param": "n",
    "code": "invalid_value"
  }
}
```

约定：

- `400`：参数无效、参数不支持、上传格式错误。
- `401`：缺失或无效 API Key。
- `403`：API Key 被禁用。
- `404`：模型不存在。
- `429`：并发队列已满、账号池无可用额度或上游限流。
- `500`：内部错误。
- `502`：上游返回无效结果或上游协议错误。
- `503`：Solver、浏览器或关键依赖未就绪。
- `504`：上游请求超时。

错误信息不得暴露 Access Token、Cookie、代理认证信息、账号密码或内部堆栈。

## 5. 上游能力与降级

### 5.1 官网 `picture_v2`

- 只使用 ChatGPT 官网 Web 链路。
- 使用 `system_hints=["picture_v2"]`。
- 上游对话模型使用配置项 `default-upstream-model`，默认 `gpt-5-5`。
- 所有模型名称当前共用该链路。

### 5.2 可保证能力

- OpenAI 路由、请求字段验证和错误结构。
- 多图请求拆分为多个独立上游任务。
- Base64 返回。
- 10 分钟签名 URL。
- PNG、JPEG、WebP 的本地编码和压缩。
- 本地敏感词检查和上游审核结果透传。
- 账号池调度、Token 刷新、额度扣减和失败重试。

### 5.3 尽力实现能力

- 精确尺寸。
- 精确质量等级。
- 透明背景。
- 中间帧数量。
- 模型语义与官方 `gpt-image-2.5-sunburst/flare` 的底层差异。

这些字段必须被正确解析、记录和返回，但最终视觉结果由上游决定。

## 6. 账号池

### 6.1 导入

支持：

- 原始 Access Token 文本。
- 原项目账号 JSON。

账号 JSON 兼容字段：

- `access_token`
- `refresh_token`
- `id_token`
- `email`
- `user_id`
- `type`
- `source_type`
- `quota`
- `restore_at`
- `proxy`

导入规则：

- Access Token 必填。
- 相同 Access Token 去重。
- `email` 和 `user_id` 可从 JWT 补齐。
- 导入后立即执行一次账号信息刷新。
- 刷新失败保留账号并标记 `error`，不得静默丢弃。

### 6.2 状态

- `normal`：可调度。
- `limited`：额度为 0 或上游限流。
- `error`：Token、网络或账号信息刷新失败。
- `disabled`：运营者手动禁用。

定时刷新不得将 `disabled` 改回 `normal`。

### 6.3 Token 生命周期

- Access Token 剩余不足 24 小时时自动刷新。
- 存在 Refresh Token 时每周执行 keepalive。
- 刷新成功立即原子写回 SQLite。
- 刷新 401 时最多重试一次。
- 生图请求遇到 Token 失效时，强制刷新后再重试一次。
- Token 使用 AES-GCM 加密保存。
- 配置中的 `signing-key` 丢失后无法恢复账号 Token，必须在部署文档中明确说明。

### 6.4 额度

- 每次成功生成一张图片扣减对应账号 1 点 `image_gen` 额度。
- 一次 HTTP 请求包含 `n` 张图片时，按实际成功张数扣减。
- 失败不扣减账号额度，但记录失败次数。
- 额度为 0 时状态改为 `limited`。
- 到达 `restore_at` 后，下一次调度或定时刷新触发额度更新。

### 6.5 账号管理

极简管理端支持：

- 粘贴 Access Token 导入。
- 上传账号 JSON 导入。
- 单个和批量刷新。
- 搜索、按状态筛选和分页。
- 修改状态、代理和备注。
- 单个和批量删除。
- 查看额度、恢复时间、成功次数、失败次数和最近错误。
- 导出账号 JSON。

默认不自动删除无效账号；可在配置中启用。

## 7. 调度

### 7.1 选号

候选账号必须同时满足：

- 状态为 `normal`。
- `quota > 0`。
- 当前在途数小于账号并发上限。

选号排序：

1. 最近使用时间最早。
2. 剩余额度较高。
3. 账号 ID 稳定排序。

### 7.2 并发

默认值：

- 全局同时执行 HTTP 生图请求数：`3`，后台可调整。
- 单账号并发：`1`，后台可调整。
- 等待队列上限：`100`。
- 请求总超时：`300` 秒。

队列满时立即返回 OpenAI 风格 `429`。

### 7.3 `n`

- `n=1` 直接执行一次上游任务。
- `n>1` 拆分为 `n` 个独立上游任务。
- 每个子任务独立选号和扣减额度。
- 允许子任务并行，但受全局并发和账号并发限制。
- 任意子任务失败时，整次 HTTP 请求返回错误。
- 日志记录成功张数、失败张数和各子任务耗时。

### 7.4 重试

- Token 失效：刷新后重试一次。
- 连接重置、TLS 临时错误和超时：可切换账号重试一次。
- 403、内容审核失败和明确的上游拒绝：不自动重试。
- 每次 HTTP 请求最多两个上游尝试。

## 8. API Key 与管理认证

### 8.1 网关 API Key

- 支持多个 API Key。
- 每个 Key 包含名称、哈希、启用状态、创建时间和最后使用时间。
- 明文只在创建时展示一次。
- 不设置 Key 级 RPM、并发数、模型权限或图片额度。
- New API 负责客户端、租户和额度管理。

### 8.2 管理密钥

- 使用独立管理密钥。
- 配置中保存 bcrypt 哈希。
- 管理端登录后只在当前标签页 `sessionStorage` 保存。
- 管理 API 使用 `X-Management-Key` 或 Bearer 管理密钥。
- 管理密钥与网关 API Key 不共享。

## 9. 管理端

保留六个页面：

### 9.1 登录

- 输入管理密钥。
- 不创建服务端 Session。

### 9.2 概览

- 可用账号数。
- 限流、异常和禁用账号数。
- 当前运行请求数。
- 排队请求数。
- 今日成功和失败图片数。
- 最近 24 小时平均耗时。
- Solver 浏览器状态。

### 9.3 账号池

- 第 6.5 节列出的全部账号管理能力。

### 9.4 API Key

- 创建、重命名、启用、禁用和删除。
- 显示最后使用时间。
- 不显示已有明文。

### 9.5 运行设置

至少包括：

- 全局并发数。
- 单账号并发数。
- 请求超时。
- 临时 URL 有效期。
- 日志保留天数。
- 全局代理。
- Solver 浏览器路径。
- 默认上游模型。
- 账号刷新间隔。
- 自动移除无效账号开关。
- `public-base-url`。

### 9.6 日志与统计

- 调用日志。
- 账号事件。
- 按时间范围、API Key、账号、模型和状态筛选。
- 请求 ID、模型、账号、耗时、成功张数、失败原因和额度消耗。
- 日志保留天数由配置决定，默认 30 天。
- 支持手动删除筛选结果。

不允许出现：

- 在线画图。
- 图片历史。
- 图片资产库。
- 文本聊天。
- 搜索。
- PPT/PSD。
- CPA。
- Sub2API。

## 10. 日志与可观测性

### 10.1 调用日志

记录：

- 请求 ID。
- API Key ID。
- 账号 ID 和邮箱脱敏值。
- 模型名称。
- 请求模式：生成或编辑。
- 请求图片数。
- 成功图片数。
- 耗时。
- HTTP 状态。
- OpenAI 错误码。
- 额度消耗。
- 临时 URL 过期时间。

默认不保存完整 Base64、完整 prompt 或上传图片。

### 10.2 账号事件

记录：

- 导入。
- 刷新。
- Token 刷新。
- 额度变化。
- 限流。
- 错误。
- 启用和禁用。
- 删除。

### 10.3 Prometheus

至少提供：

- HTTP 请求数。
- HTTP 错误数。
- HTTP 请求耗时。
- 当前运行请求数。
- 排队请求数。
- 账号状态数量。
- 账号剩余额度总量。
- 上游失败数。
- 临时 URL 文件数和磁盘占用。

标签不得包含 Token、API Key 明文或账号邮箱。

### 10.4 健康检查

`/healthz`：

- 只表示进程存活。

`/readyz`：

- SQLite 可读写。
- 配置可读取。
- Solver 浏览器路径可用。
- 至少一个账号处于 `normal` 或 `limited`。

## 11. 数据与存储

SQLite 保存：

- 账号元数据。
- 加密 Token。
- API Key 哈希。
- 调用日志。
- 账号事件。
- 配置审计记录。
- 临时 URL 文件索引。

本地临时目录保存：

- `response_format=url` 生成的图片。
- 未完成的编辑上传临时文件。
- 过期后自动清理。

不保存：

- 长期图片资产库。
- 用户图片历史。
- 在线画布会话。
- 文本聊天记录。

备份策略：

- 不在 Web 管理端实现 R2 备份。
- 使用外部定时任务备份 SQLite 数据和配置文件。
- 文档提供停写备份、恢复和权限设置步骤。

## 12. 安全

- 所有管理 API 和管理端静态资源使用同源部署。
- API Key 只保存 SHA-256 或同等级不可逆哈希。
- Access、Refresh 和 ID Token 使用 AES-GCM 加密。
- 日志对 Token、Cookie、代理密码和 Authorization 脱敏。
- 上传图片验证真实格式、大小、宽高和像素数。
- 临时 URL 使用 HMAC、过期时间和常量时间签名比较。
- 临时文件路径必须使用服务端 ID，不接受用户提供路径。
- 远程代理地址允许 HTTP、HTTPS、SOCKS5 和 SOCKS5H。
- 不提供任意远程图片 URL 下载能力。
- 管理端修改配置必须校验字段范围并原子写回。

## 13. 配置默认值

| 配置 | 默认值 |
| --- | --- |
| 监听地址 | `127.0.0.1` |
| 端口 | `3000` |
| 数据库 | `data/gateway.db` |
| 全局生图并发 | `3` |
| 单账号并发 | `1` |
| 队列上限 | `100` |
| 请求超时 | `300s` |
| 临时 URL 有效期 | `10m` |
| 临时文件清理间隔 | `1m` |
| 日志保留 | `30d` |
| 默认上游模型 | `gpt-5-5` |
| 账号刷新间隔 | `30m` |
| Access Token 提前刷新 | `24h` |
| Refresh Token keepalive | `7d` |
| 默认 Solver 时区 | `Asia/Shanghai` |
| Solver headless | `true` |
| Solver no-sandbox | `true` |

## 14. 架构要求

Provider 接口至少包含：

- 模型能力描述。
- 图片生成。
- 图片编辑。
- 流式中间事件。
- 上游错误分类。

当前 Provider：

- `chatgpt-web`
- 渠道前缀 `picture_v2`

未来 Provider：

- `banana`
- 接入时不修改 OpenAI 路由和公共响应结构。

建议处理流程：

1. HTTP Handler 验证 OpenAI 请求。
2. API Key 中间件认证。
3. 输入验证和图片解码。
4. 全局并发限制器排队。
5. 账号调度器选号。
6. Provider 执行生成或编辑。
7. 图片格式转换和尺寸校验。
8. Base64 返回或写入临时目录并签名。
9. 写入调用日志。
10. 返回标准响应或标准错误。

## 15. 验收标准

### 15.1 OpenAI 协议

- `GET /v1/models` 返回四个已确认模型名称。
- `GET /v1/models/{model}` 对存在的模型返回标准对象。
- 不存在的模型返回 OpenAI 风格 `404`。
- 生成接口接受全部约定字段。
- 编辑接口按 OpenAI multipart 协议工作。
- 参数错误返回标准错误结构。
- 未知或不支持字段返回 `400 unsupported_parameter`。
- 响应包含 `X-Request-ID`。

### 15.2 图片输出

- `b64_json` 能返回有效 PNG、JPEG 或 WebP。
- `url` 返回 10 分钟有效签名地址。
- URL 过期后返回 `404` 或 `410`。
- `output_format` 和 `output_compression` 生效。
- `n=4` 能按账号池并发生成。
- 部分子任务失败时整次请求返回错误。
- `partial_images` 和 `stream` 返回合法 SSE。

### 15.3 账号池

- Access Token 导入成功。
- 账号 JSON 导入成功。
- 重复 Token 不重复入库。
- Token 即将过期时自动刷新。
- `disabled` 账号不会被定时刷新重新启用。
- 额度为 0 后账号进入 `limited`。
- 恢复时间到达后可重新调度。
- Token 不会以明文写入 SQLite。

### 15.4 调度

- 全局并发达到上限时，新请求排队。
- 队列满时返回 `429`。
- 单账号不超并发。
- 账号 401 时刷新并重试。
- 上游 429 时账号进入冷却。
- 没有可用账号时返回 `429`。

### 15.5 API Key 与管理端

- 创建 Key 后只展示一次明文。
- 禁用 Key 后请求返回 `403`。
- 管理密钥无法用于客户端 API。
- 管理端只显示六个确认页面。
- 并发数和日志保留时间修改后持久化。

### 15.6 日志与运维

- 调用日志包含请求 ID、模型、账号、耗时和图片数。
- 日志到期自动清理。
- `/healthz`、`/readyz` 和 `/metrics` 可访问。
- 临时文件到期自动删除。
- 服务重启后账号、API Key、配置和未过期 URL 仍可用。

### 15.7 构建与部署

- 生成 Linux `amd64` 单一二进制。
- 提供 `config.example.yaml`。
- 提供 systemd service。
- 不依赖 Python、Node、Redis 或 PostgreSQL。
- 缺少浏览器时 `/readyz` 明确报告未就绪。

## 16. 实施顺序

1. 裁剪 Go 路由和内嵌前端，建立生图网关边界。
2. 补齐 OpenAI 生成、编辑、模型、错误和响应协议。
3. 实现 Base64、临时 URL、格式转换和 SSE。
4. 重构账号池调度、Token 刷新、额度和失败切换。
5. 实现多 API Key、管理登录、六个管理页面。
6. 实现调用日志、账号事件、统计和 Prometheus。
7. 增加 Provider 注册机制，为 Banana 留接口。
8. 完成单元测试、模拟上游测试、真实账号测试和 Linux amd64 构建。

## 17. 已确认默认决策

- 完整实现 OpenAI 的请求、响应和错误协议。
- 官网 Web 无法精确控制的上游语义采用尽力实现。
- 不提供自定义异步任务接口。
- 不提供 `/v1/chat/completions`、`/v1/responses` 或其他文本接口。
- `n` 范围为 `1-4`，部分失败时整次请求失败。
- 参考图最多 4 张，单张最大 25 MiB。
- URL 有效期 10 分钟，签名公开访问。
- 多个 API Key，但不做 Key 级配额。
- 日志默认保留 30 天，可配置。
- 只构建 Linux `amd64`。
- 不新建分支，继续当前分支。
- 原 Python 项目文件保留作参考。

## 18. 实现状态

截至 2026-10-25：

- 已完成 OpenAI 模型、生成、编辑、Base64、临时 URL、SSE、usage 和标准错误结构。
- 已完成 PNG、JPEG、WebP 输出转换和压缩参数。
- 已完成参考图、PNG mask、真实格式和像素限制校验。
- 已完成全局动态并发、队列上限、请求超时和 `n=1-4`。
- 已完成账号池、Access Token/JSON 导入、额度扣减、限流状态、Token 刷新和 disabled 保护。
- 已完成多 API Key、管理登录、账号池、概览、运行设置和日志页面。
- 已完成请求日志、API Key 使用统计、账号事件、过滤、保留期清理和 Prometheus 指标。
- 已完成 Linux amd64 单二进制构建和发布包。
- 已使用真实 ChatGPT 账号验证文生图、图片编辑和 10 分钟签名 URL。
- 真实文生图和编辑均返回 `1254x1254` PNG，确认官网链路尺寸为尽力实现而非精确保证。
- 管理员页面已通过桌面和 390px 移动端视觉检查。
- `partial_images > 0` 已接受；官网链路没有中间帧时只返回最终完成事件。
- `input_fidelity` 等当前无法可靠映射的官方字段返回 `400 unsupported_parameter`。
- Banana Provider 尚未实现，仅保留扩展边界。
