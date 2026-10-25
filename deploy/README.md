# Linux amd64 部署

发布包包含：

- `chatgpt2api`
- `config.example.yaml`
- `deploy/chatgpt2api.service`
- `deploy/cleanup-images.sh`
- 配置与 Turnstile 说明文档

## 安装

```bash
sudo useradd --system --home /var/lib/chatgpt2api --shell /usr/sbin/nologin chatgpt2api
sudo install -d -m 0755 -o chatgpt2api -g chatgpt2api /opt/chatgpt2api /etc/chatgpt2api /var/lib/chatgpt2api
sudo install -m 0755 chatgpt2api /opt/chatgpt2api/chatgpt2api
sudo install -m 0600 -o chatgpt2api -g chatgpt2api config.yaml /etc/chatgpt2api/config.yaml
sudo install -m 0644 deploy/chatgpt2api.service /etc/systemd/system/chatgpt2api.service
sudo systemctl daemon-reload
sudo systemctl enable --now chatgpt2api
```

配置放在 `/etc/chatgpt2api/config.yaml` 时，运行数据要使用绝对路径：

```yaml
data-dir: "/var/lib/chatgpt2api/data"
```

不需要 `auth-dir`。账号通过管理端导入，SQLite 保存账号、API Key、日志和临时图片索引。

## 浏览器依赖

内置 Solver 需要 Chromium、Chrome 或 Edge。设置：

```yaml
sentinel:
  browser-path: "/usr/bin/chromium"
```

也可以使用环境变量：

```bash
SENTINEL_BROWSER_BIN=/usr/bin/chromium
```

缺少浏览器时 `/readyz` 返回未就绪。

## 检查

```bash
sudo journalctl -u chatgpt2api -n 80 --no-pager
sudo systemctl status chatgpt2api
curl http://127.0.0.1:3000/healthz
curl http://127.0.0.1:3000/readyz
```

首次启动会打印管理密钥、客户端 API Key 和 `signing-key`。这些值只出现一次。

## 宝塔 Nginx

应用层已经处理 CORS，Nginx 需要传递 `Origin`，不得重复添加
`Access-Control-Allow-Origin`：

```nginx
location / {
    proxy_pass http://127.0.0.1:3000;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header Origin $http_origin;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_buffering off;
}
```

`proxy_buffering off` 用于图片流式响应。

检查已有 CORS 配置：

```bash
grep -R "Access-Control-Allow-Origin" /www/server/panel/vhost/nginx/
```

## 资源

systemd 单元默认：

- `GOMEMLIMIT=200MiB`
- `GOGC=75`
- cgroup 内存上限 `1000M`

Chromium 会占用数百 MB 内存。低配机器建议把 `performance.image-workers`
设置为 `1`，在 `memory` 或监控确认后再提高。

## 备份

需要一起备份：

- `/etc/chatgpt2api/config.yaml`
- `/var/lib/chatgpt2api/data/chatgpt2api.db`
- WAL 文件或在线 SQLite 备份文件

`signing-key` 丢失后，已有 Access Token 无法解密。
