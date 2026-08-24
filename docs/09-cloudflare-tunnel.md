# Cloudflare Tunnel (cloudflared) 极限部署

本文档描述如何用 **Cloudflare Tunnel (`cloudflared`)** 替代 Caddy 作为公网入口，配合 **Unix Domain Socket** 本地回源，将 OpenAI Custom GPT Action 的传输延迟、抗干扰能力与吞吐压榨到技术极限。

> 与 Caddy 方案的差异：不再依赖公网 IP、开放 443 端口或公共 CA 证书。`cloudflared` 主动向 Cloudflare 边缘建立 **QUIC (HTTP/3)** 出站连接，本地通过 **Unix Domain Socket** 回源到 `mygpt-cf-tunnel`，彻底消除本地 TCP 握手与内核协议栈开销。

## 技术规格与边界矩阵

| 维度 | OpenAI Custom GPT Action 极限 | Cloudflare Edge / Tunnel 极限 | 本项目压榨目标配置 |
| :--- | :--- | :--- | :--- |
| **单次请求硬超时** | **45.0 秒**（网络往返总耗时） | **100 秒**（Edge 524 超时） | **命令上限 38.0 秒**（留出 7 秒网络与附件传输裕量） |
| **请求/响应 JSON 大小** | **100,000 字符**（约 98 KB） | **100 MB**（CF 边缘上传上限） | **Inline 限制 30,000 字符**，超出自动切附件 |
| **文件传输** | 最大 10 个文件，每个 **10 MB** | 隧道支持流式传输，无单文件限制 | **10 MB 严格对齐**，URL 超时 10 秒 |
| **公网接入协议** | 仅支持公网 HTTPS (443) | 支持 HTTP/3 (QUIC)、HTTP/2、0-RTT | **启用 HTTP/3 + 0-RTT 连接复用** |
| **隧道通信协议** | N/A | 支持 QUIC (UDP 7844) / HTTP2 (TCP 443) | **强制 QUIC 协议**（抗丢包、无队头阻塞） |
| **本地 IPC 传输** | N/A | 支持 TCP Localhost 或 **Unix Domain Socket** | **采用 Unix Domain Socket**（消除 TCP 握手开销） |

## 架构拓扑

```text
┌──────────────────────────────────────────────────────────┐
│ Custom GPT / ChatGPT Action                              │
│  - 导入 /openapi.json                                    │
│  - Bearer token                                           │
└──────────────────────────────┬───────────────────────────┘
                               │ HTTPS :443 (HTTP/3 + 0-RTT)
                               ▼
┌──────────────────────────────────────────────────────────┐
│ Cloudflare Edge (全球节点)                               │
│  - WAF/Bot 全量 Skip（零拦截）                           │
│  - Cache Rules 穿透                                       │
│  - CF-Ray -> X-Request-Id Transform Rule                 │
└──────────────────────────────┬───────────────────────────┘
                               │ QUIC (UDP 7844) 出站
                               ▼
┌──────────────────────────────────────────────────────────┐
│ cloudflared（VPS 上主动出站）                            │
│  - protocol: quic                                         │
│  - 长连接池 256、Keep-Alive 90s                          │
└──────────────────────────────┬───────────────────────────┘
                               │ Unix Domain Socket
                               ▼
┌──────────────────────────────────────────────────────────┐
│ mygpt-cf-tunnel Go 服务（监听 /run/mygpt-cf-tunnel.sock）        │
│  - Bearer 鉴权 / GPT ID 白名单                          │
│  - 38s 命令执行 / 文件管道 / HMAC 附件                   │
└──────────────────────────────────────────────────────────┘
```

## 一、内核网络栈调优

`cloudflared` 运行 QUIC 时，Linux 默认 UDP 缓冲区会导致丢包。部署内核调优：

```bash
sudo install -m 0644 deploy/cloudflared/99-cloudflared-fast.conf /etc/sysctl.d/99-cloudflared-fast.conf
sudo sysctl --system
```

`99-cloudflared-fast.conf` 内容要点：UDP 收发缓冲区 2.5 MB、`somaxconn=4096`、TCP BBR 拥塞控制（隧道降级至 TCP 443 时生效）。

## 二、部署 mygpt-cf-tunnel（Unix Socket 监听）

```bash
make check build
sudo install -m 0755 bin/mygpt-cf-tunnel /usr/local/bin/mygpt-cf-tunnel
sudo install -m 0755 bin/mygpt-audit /usr/local/bin/mygpt-audit
sudo install -m 0644 deploy/mygpt-cf-tunnel.service /etc/systemd/system/mygpt-cf-tunnel.service
sudo install -m 0600 deploy/mygpt-cf-tunnel.env.example /etc/mygpt-cf-tunnel.env
```

编辑 `/etc/mygpt-cf-tunnel.env`，设置随机 `API_TOKEN`、真实 `ACTION_BASE_URL`，并确认：

```env
LISTEN_ADDR=unix:/var/run/mygpt-cf-tunnel.sock
```

> `/var/run` 是 `/run` 的符号链接，等价于 `/run/mygpt-cf-tunnel.sock`。服务以 root 启动时在 `/run` 下创建 socket 并 `chmod 0666`，`cloudflared` 以非 root 用户运行时仍可读写。

启动服务：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now mygpt-cf-tunnel
```

## 三、部署 cloudflared

### 1. 安装并登录

```bash
# 安装 cloudflared（以官方发布版为准）
curl -L https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 \
  -o /tmp/cloudflared && sudo install -m 0755 /tmp/cloudflared /usr/local/bin/cloudflared
cloudflared tunnel login
```

### 2. 创建命名隧道

```bash
cloudflared tunnel create mygpt-cf-tunnel
# 输出隧道 UUID，并生成 ~/.cloudflared/<TUNNEL-UUID>.json
```

### 3. 配置 DNS 路由

```bash
cloudflared tunnel route dns mygpt-cf-tunnel action.你的域名.com
```

### 4. 安装配置与凭据

```bash
sudo install -d -m 0755 /etc/cloudflared
sudo install -m 0600 deploy/cloudflared/config.yml /etc/cloudflared/config.yml
sudo install -m 0600 ~/.cloudflared/<TUNNEL-UUID>.json /etc/cloudflared/<TUNNEL-UUID>.json
```

编辑 `/etc/cloudflared/config.yml`，将 `<你的-TUNNEL-UUID>`、凭据文件名与域名替换为真实值，确认 `ingress` 使用：

```yaml
ingress:
  - hostname: action.你的域名.com
    service: unix:/var/run/mygpt-cf-tunnel.sock
    originRequest:
      httpHostHeader: action.你的域名.com
  - service: http_status:404
```

### 5. 创建 systemd 服务

```bash
sudo useradd --system --home-dir /etc/cloudflared --shell /usr/sbin/nologin cloudflared 2>/dev/null || true
sudo install -m 0644 deploy/cloudflared/cloudflared.service /etc/systemd/system/cloudflared.service
```

编辑服务文件，将 `ExecStart` 中的 `<你的-TUNNEL-UUID>` 替换为真实 UUID，然后：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now cloudflared
```

## 四、Cloudflare 边缘层“零限制”配置

按 [deploy/cloudflared/cloudflare-dashboard-checklist.md](../deploy/cloudflared/cloudflare-dashboard-checklist.md) 在 Cloudflare Dashboard 完成：

1. WAF Skip 规则（跳过全部安全组件）。
2. 关闭 Bot Fight Mode。
3. Cache Rules 穿透 `/v1/command/run` 与 `/health`。
4. Transform Rule 将 `CF-Ray` 透传为 `X-Request-Id`。
5. 开启 HTTP/3、0-RTT、gRPC/WebSockets、Brotli/Early Hints。

## 五、验证端到端

```bash
# 本地 socket 探活
curl -sS --unix-socket /var/run/mygpt-cf-tunnel.sock http://localhost/health

# 公网端到端
curl -sS https://action.你的域名.com/health
curl -sS https://action.你的域名.com/openapi.json | jq .servers

# 隧道状态
systemctl status cloudflared --no-pager
journalctl -u cloudflared -n 50 --no-pager

# 服务状态
systemctl status mygpt-cf-tunnel --no-pager
ss -xl | rg mygpt-cf-tunnel.sock
```

## 六、请求 ID 与客户端 IP 的透传

- **Request ID**：`cloudflared` 回源时，服务端 `trustedRequestID` 优先读取 `X-Request-Id`（由 Dashboard Transform Rule 将 `CF-Ray` 透传），缺失时回退读取 `CF-Ray` 头。由于请求来自 Unix Socket（可信本地源），这些头会被信任。
- **客户端 IP**：服务端 `clientIP` 优先读取 `CF-Connecting-IP`（cloudflared 自动注入的真实客户端地址），其次回退 `X-Forwarded-For`，最后回退 socket 对端地址。

## 七、故障排查

| 现象 | 原因与处理 |
| :--- | :--- |
| `connection refused` 访问 socket | `mygpt-cf-tunnel` 未启动或 socket 路径不符，检查 `journalctl -u mygpt-cf-tunnel` |
| `permission denied` | socket 权限非 0666，或 cloudflared 用户无 `/run` 访问权限 |
| 公网 502/504 | 隧道未路由、DNS 未配置，或 ingress 兜底规则缺失 |
| 403 / HTML parse error | 未配置 WAF Skip 规则，按 checklist 放行 |
| 附件下载超时 | `ARTIFACT_TTL_SECONDS` 过短，或 Cache 未穿透 `/v1/files/download/*` |
| QUIC 连接不稳定 | 检查 UDP 7844 出站是否被防火墙拦截；检查 sysctl 是否生效 |

## 八、回滚到 Caddy 方案

如需回退到 Caddy 公网方案：

1. 停止并禁用 `cloudflared`。
2. 将 `/etc/mygpt-cf-tunnel.env` 的 `LISTEN_ADDR` 改回 `127.0.0.1:8787`。
3. 重启 `mygpt-cf-tunnel`，恢复 Caddyfile 反代到 `127.0.0.1:8787`。
