# systemd deployment

The unit runs the binary from `/opt/chatgpt2api`, reads its configuration from
`/etc/chatgpt2api/config.yaml`, and keeps the working directory and all runtime
data in `/var/lib/chatgpt2api`.

```bash
sudo useradd --system --home /var/lib/chatgpt2api --shell /usr/sbin/nologin chatgpt2api
sudo install -d -m 0755 -o chatgpt2api -g chatgpt2api /opt/chatgpt2api /etc/chatgpt2api /var/lib/chatgpt2api
sudo install -m 0755 chatgpt2api /opt/chatgpt2api/chatgpt2api
sudo install -m 0600 -o chatgpt2api -g chatgpt2api config.yaml /etc/chatgpt2api/config.yaml
sudo install -m 0644 deploy/chatgpt2api.service /etc/systemd/system/chatgpt2api.service
sudo systemctl daemon-reload
sudo systemctl enable --now chatgpt2api
```

Relative paths in `config.yaml` are resolved from the directory containing the
configuration file. Because the systemd unit keeps the configuration in
`/etc/chatgpt2api`, set absolute paths in `config.yaml`:

```yaml
data-dir: "/var/lib/chatgpt2api/data"
auth-dir: "/var/lib/chatgpt2api/data/auth"
```

For a 宝塔-style deployment where the configuration and data live together, the
relative defaults `./data` and `./data/auth` are suitable.

Check the first-start credentials and service state without copying secrets
into shell history:

```bash
sudo journalctl -u chatgpt2api -n 50 --no-pager
sudo systemctl status chatgpt2api
curl http://127.0.0.1:3000/healthz
```

The management panel is always enabled and every management request still
requires `remote-management.secret-key`. Remote exposure is controlled by the
listen address, firewall, or reverse proxy.

## 宝塔 Nginx

The Go service already handles the CORS allowlist. Nginx should pass the
request `Origin` through and must not add a second `Access-Control-Allow-Origin`
header. Remove any existing global CORS `add_header` rules for this site, then
use the normal reverse-proxy directives:

```nginx
location / {
    proxy_pass http://127.0.0.1:3000;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header Origin $http_origin;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

Check the site configuration for an existing header:

```bash
grep -R "Access-Control-Allow-Origin" /www/server/panel/vhost/nginx/
```

Duplicate CORS headers cause browsers to reject the response even when the
application allowlist is correct.

The service unit uses `GOMEMLIMIT=200MiB`, `GOGC=75`, and `MemoryMax=1000M`.
The cgroup limit includes the embedded Chromium process tree. Keep
`performance.image-workers: 1` and `parallel-generation: false` on a small
server.

Real image generation uses the built-in Go browser solver. Install a full
Chromium, Chrome, or Edge executable on the host; the service detects common
paths automatically. No second solver service, Python, or Playwright is needed.
Set `sentinel.browser-path` when the executable is installed elsewhere, and use
`proxy-url` for both upstream HTTP and the browser.
