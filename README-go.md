# chatgpt2api-go Image Gateway

这是 `feat/go-reimplementation` 分支上的单机生图 API 网关。

网关只负责：

- OpenAI GPT Image 兼容 API。
- ChatGPT 官网账号池和 `picture_v2` 生图链路。
- 多 API Key、Token 自动刷新、账号调度、额度和并发控制。
- Base64 或 10 分钟签名 URL 图片输出。
- 极简账号与运行管理控制台。
- 调用日志、健康检查和 Prometheus 指标。

不包含在线画图、图片资产库、文本模型、Web Search、PPT/PSD、Codex 图片链路、CPA 或 Sub2API。

## 系统要求

- Go 1.26 或兼容版本。
- Linux `amd64` 部署目标。
- Chromium、Chrome 或 Edge，用于内置 Turnstile Solver。
- 建议服务器至少 1 GB 内存。

## 构建

```bash
go build -tags nodynamic -trimpath -ldflags="-s -w" \
  -o bin/chatgpt2api ./cmd/chatgpt2api
```

Linux amd64 发布包：

```powershell
powershell -ExecutionPolicy Bypass -File scripts/build_go_linux.ps1
```

输出：`dist/chatgpt2api-image-gateway-linux-amd64.tar.gz`。

## 启动

```bash
cp config.example.yaml config.yaml
./bin/chatgpt2api -config config.yaml
```

首次启动会生成：

- 管理密钥。
- 默认客户端 API Key。
- Cookie 和账号 Token 加密使用的 `signing-key`。

这些值只在首次启动时打印一次。配置文件中只保存管理密钥和客户端 Key 的哈希。

管理端：

```text
http://127.0.0.1:3000/admin/
```

## OpenAI API

### 模型

```http
GET /v1/models
GET /v1/models/{model}
```

当前暴露：

- `gpt-image-2`
- `gpt-image-2.5`
- `gpt-image-2.5-sunburst`
- `gpt-image-2.5-flare`

四个名称当前共用 ChatGPT 官网 `picture_v2` 链路。模型名称用于本地路由、日志和响应元数据，不表示四个不同底层模型。实际底层模型由 ChatGPT 官网决定。

### 文生图

```http
POST /v1/images/generations
Authorization: Bearer <api-key>
Content-Type: application/json
```

```json
{
  "model": "gpt-image-2",
  "prompt": "A clean product photo of a ceramic cup",
  "n": 1,
  "size": "1024x1024",
  "quality": "high",
  "background": "opaque",
  "moderation": "auto",
  "output_format": "png",
  "response_format": "b64_json"
}
```

支持：

- `n`：1 到 4。
- `size`：`auto` 或 `宽x高`。
- `quality`：`auto`、`low`、`medium`、`high`；2.5 名称还接受 `xhigh`、`max`。
- `background`：`auto`、`opaque`、`transparent`。
- `moderation`：`auto`、`low`。
- `output_format`：`png`、`jpeg`、`webp`。
- `output_compression`：JPEG 和 WebP 使用，范围 0 到 100。
- `response_format`：`b64_json` 或 `url`。
- `stream` 和 `partial_images`。
- `safety_identifier`。

未知参数返回 `400 unsupported_parameter`，不会被静默忽略。

### 图片编辑

```http
POST /v1/images/edits
Authorization: Bearer <api-key>
Content-Type: multipart/form-data
```

```bash
curl http://127.0.0.1:3000/v1/images/edits \
  -H "Authorization: Bearer $CHATGPT2API_KEY" \
  -F "model=gpt-image-2" \
  -F "prompt=Change the background to blue" \
  -F "image[]=@input.png"
```

限制：

- 1 到 4 张参考图。
- PNG、JPEG、WebP。
- 单张最大 25 MiB。
- `mask` 必须是 PNG，且尺寸必须与第一张参考图一致。

### 返回格式

非流式响应：

```json
{
  "created": 1780000000,
  "background": "opaque",
  "data": [
    {
      "b64_json": "...",
      "revised_prompt": "..."
    }
  ],
  "output_format": "png",
  "quality": "high",
  "size": "1024x1024",
  "usage": {
    "input_tokens": 12,
    "input_tokens_details": {
      "image_tokens": 0,
      "text_tokens": 12
    },
    "output_tokens": 4096,
    "total_tokens": 4108
  }
}
```

`response_format=url` 时，`b64_json` 替换为短期签名 `url`。

### 临时 URL

- 默认 10 分钟有效，可在管理端调整。
- 使用 HMAC 签名，不需要 API Key 即可下载。
- 文件放到 `data/temporary-images/`。
- 过期后自动删除。
- 不建立长期图片资产库。

### 流式响应

请求 `stream=true` 后使用 SSE：

```text
event: image_generation.completed
data: {"type":"image_generation.completed",...}
```

`partial_images` 接受 0 到 3。ChatGPT 官网链路没有中间帧时，只返回最终完成事件，不伪造中间图片。

### 错误

```json
{
  "error": {
    "message": "n must be between 1 and 4",
    "type": "invalid_request_error",
    "param": "n",
    "code": "invalid_value"
  }
}
```

所有响应包含 `X-Request-ID`。

## 账号池

支持导入：

- 每行一个 Access Token。
- 原项目账号 JSON。

兼容字段包括 `access_token`、`refresh_token`、`id_token`、`email`、`user_id`、`type`、`source_type`、`quota`、`restore_at` 和 `proxy`。

账号状态：

- `normal`：可调度。
- `limited`：额度为 0 或上游限流。
- `error`：刷新或凭据异常。
- `disabled`：手动禁用，不会被定时刷新重新启用。

Token 使用 AES-GCM 加密保存。配置中的 `signing-key` 丢失后，已有 Token 将无法解密，请把配置文件与 SQLite 数据一起备份。

`image.auto-remove-invalid-accounts` 默认关闭。开启后，只有明确返回 401 或 Token 刷新失败的账号才会被自动删除。

## 调度

- 默认全局上游任务并发数为 3，可在管理端调整。
- 默认单账号并发为 1。
- 等待队列默认 100。
- 请求超时默认 300 秒。
- 账号按状态、剩余额度、在途数和最近使用时间选择。
- `n > 1` 时拆分为多个上游任务。
- 任意一张失败时整次 HTTP 请求失败，但日志保留已成功张数和额度消耗。

## API Key

- 支持多个客户端 Key。
- 可创建、重命名、启用、禁用和删除。
- 明文只在创建时出现一次。
- 配额、租户和客户端管理由 New API 等上层系统负责。

## 管理端

```text
/admin/
```

只保留：

- 概览。
- 账号池。
- API Key。
- 运行设置。
- 请求日志。

管理端不包含画图、图片历史、图片资产、文本对话、搜索或 PPT/PSD。

## 健康检查与监控

```text
GET /healthz
GET /readyz
GET /metrics
```

`/readyz` 检查 SQLite、可用账号和 Solver 状态。

## CLI

```bash
# 重新设置管理密钥
./bin/chatgpt2api -config config.yaml -set-management-key "new-strong-key"

# 导入旧 Python 项目的 accounts.json 和 auth_keys.json
./bin/chatgpt2api -config config.yaml -migrate-legacy

# 手动清理过期临时图片和日志
./bin/chatgpt2api -config config.yaml -cleanup-images
```

## 官网链路限制

以下字段会被正确接收、记录和返回，但最终视觉结果由 ChatGPT 官网决定：

- 精确尺寸。
- 精确质量等级。
- 透明背景。
- 中间帧数量。
- 各模型名称对应的底层模型差异。

## 后续 Banana 接入

Provider 接口已与 HTTP 协议解耦。接入 Banana 时新增 Provider 和模型路由，不修改现有 OpenAI 路由和响应结构。
