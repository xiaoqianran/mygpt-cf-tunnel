# mygpt-cf-tunnel

[![test](https://github.com/xiaoqianran/mygpt-cf-tunnel/actions/workflows/test.yml/badge.svg)](https://github.com/xiaoqianran/mygpt-cf-tunnel/actions/workflows/test.yml)

> **中文完整文档：** [`README.zh-CN.md`](./README.zh-CN.md) · **Custom GPT 指令：** [`GPT_INSTRUCTIONS.md`](./GPT_INSTRUCTIONS.md)

把 Custom GPT 连接到一台真实 VPS 的 **通用 root shell**。Action 面保留同步 `runCommand`，并为长任务提供异步 `startCommand` / `getCommandJob` / `cancelCommandJob`。

它不是 GitHub 工具集合，也不是预装 CLI 的固定能力列表。模型可以直接使用服务器已有能力；缺什么，就通过 root shell 安装什么，然后继续组合完成工作流。

> **推荐 GPT 描述**：连接远程 VPS 的 root shell。通过 `runCommand`（短任务）或异步 Job（长任务）使用或自主安装所需软件，组合命令、程序、服务与网络能力，并完成服务器能够执行的任意工作流。

可直接粘贴到 GPT Builder 的完整指令见 [`GPT_INSTRUCTIONS.md`](./GPT_INSTRUCTIONS.md)。

## 架构

```text
Custom GPT
    │  HTTPS + Bearer API_TOKEN
    ▼
Cloudflare Tunnel
    ▼
127.0.0.1:8787
Universal VPS Root Shell Agent (systemd, root)
    │
    └── runCommand
          │
          └── /bin/bash -lc <command>
                 │
                 ├── 使用服务器已有程序
                 ├── apt / pip / npm / cargo / go install / curl ...
                 ├── 安装新的运行时、CLI、库或系统包
                 ├── 写脚本 / 编译程序 / 调 API / 管服务
                 └── 组合成服务器能够执行的任意非交互式工作流
```

`gh`、`git`、`modal`、`kaggle`、Python、Node、Go、Docker、数据库客户端或云平台 CLI 都只是示例。**它们不是能力边界。**

## 设计原则

### 一个执行原语，两种生命周期

OpenAPI 只围绕一个执行原语公开同步与异步生命周期：

```text
POST /v1/command/run
POST /v1/command/start
GET  /v1/command/jobs/{id}
POST /v1/command/jobs/{id}/cancel
```

过去的 `syncRepository`、`readFiles`、`applyChanges`、`gitDiff`、`commitAndPush`、`createRelease` 等专用 API 已从代码中删除。

仓库任务直接由 shell 完成，例如：

```bash
gh repo clone owner/repo /srv/work/repo
cd /srv/work/repo
rg 'target'
python3 scripts/change.py
go test ./...
git diff --check
git add -A
git commit -m 'change'
git push
```

如果 `gh` 不存在，可以先安装；如果项目需要另一个运行时，也可以先安装那个运行时。工具选择属于工作流的一部分，而不是 Agent API 的一部分。

### 能力可以在运行时扩展

`runCommand` 是 root shell，所以模型可以先获取能力，再完成任务。例如：

```bash
apt-get update && apt-get install -y jq
python3 -m pip install --user some-cli
npm install -g some-cli
cargo install some-cli
go install example.com/tool@latest
curl -fsSL https://example.com/install.sh | bash
```

上述方式仍然只是示例。只要 VPS 的操作系统、网络和权限允许，可以采用适合目标的安装或构建方式。

### 真实主机，不是仓库 sandbox

`workdir` 可以是 root 能访问的任意真实主机目录，默认 `/root`。命令可以管理系统包、进程、systemd 服务、文件、网络请求、代码、数据和部署目标。

因此 `runCommand` 在 OpenAPI 中明确标记为：

```json
"x-openai-isConsequential": false
```

这是为了让已明确授权该 VPS 的用户无需对每次 `runCommand` 重复确认；`runCommand` 仍具有真实系统副作用，应仅连接到你信任并明确授权的 VPS。

## OpenAPI 与 GPT 指令的职责边界

OpenAPI 刻意保持简短。它只描述认证、命令执行的同步/异步生命周期、输入输出和真实副作用，不承担“教模型如何规划工作流”的职责。Custom GPT Builder 对 schema 结构和 description 长度有额外约束，因此复杂的能力说明放在 [`GPT_INSTRUCTIONS.md`](./GPT_INSTRUCTIONS.md)，而不是不断扩张 operation description。

当前兼容约束包括：

- `components.schemas` 显式描述命令结果与 Job 结果；大输出字段包含 GPT Actions 官方 `openaiFileResponse` 文件返回契约。
- OpenAPI 中关键 `description` 保持在 300 字符以内。
- Action 面只提供同一个 shell 执行原语的同步与异步生命周期；“缺工具就安装、按目标组合任意工作流”的行为策略由 GPT 指令表达。
- CI 对上述约束做回归测试，避免 Builder 导入在后续修改中再次失效。

## 长任务与 524

> Custom GPT 的同步 Action 更早受 OpenAI **45 秒 round-trip** 限制，普通 Action request/response 还必须分别少于 **100,000 字符**。Cloudflare 默认 Proxy Read Timeout 是 **125 秒**。完整边界与来源见 [`CLOUDFLARE_TIMEOUTS.md`](./CLOUDFLARE_TIMEOUTS.md)。

短任务使用 `runCommand`。可能接近 45 秒的任务使用 `startCommand`，立即取得 job id。`getCommandJob` 在 `running` 阶段暴露 rolling `stdout` / `stderr` 和单调递增的 `revision`。把上次 revision 作为 `after` 并设置 `wait_seconds=10`，服务端会在日志/状态变化时立即返回，而不是让客户端固定 sleep；完全无变化时才按约 10 秒 heartbeat 返回。长轮询默认只返回最近 12000 个 Unicode 字符，可用 `tail_chars` 调整。终态输出若能安全放进 Action JSON 就直接完整内联；过大时自动通过 `openaiFileResponse` 返回完整文本附件。Job 使用独立 context，观察请求断开不会取消任务；需要停止时显式调用 `cancelCommandJob`。终态 Job 最多保留 24 小时且最多保留最近 256 条；Agent 重启后会清空。

## Action 请求

```json
{
  "command": "set -euo pipefail\nuname -a\ncommand -v jq || apt-get update && apt-get install -y jq\njq --version",
  "workdir": "/root",
  "stdin": "",
  "timeout_seconds": 300
}
```

执行语义：

```text
root user
  -> /bin/bash -lc
  -> real VPS filesystem / network / processes / credentials
```

返回：

```json
{
  "workdir": "/root",
  "exit_code": 0,
  "stdout": "...",
  "stderr": "...",
  "timed_out": false,
  "truncated": false,
  "duration_ms": 123
}
```

`stdin` 可用于非交互式 CLI 输入、脚本、payload 或文件内容。

当 stdout/stderr 太大而不适合放进单个 GPT Action JSON 时，返回体会保留约 6000 字符预览，并增加 `openaiFileResponse`、`inline_truncated`、`full_output_attached`、`capture_truncated`。当前实现按每片约 9.5 MB、最多 10 片捕获，约可完整携带 95 MB 文本输出。`truncated: false`、`inline_truncated: true`、`full_output_attached: true`、`capture_truncated: false` 表示只有内联预览被缩短，完整输出已经作为附件返回，并没有丢失；只有 `truncated: true` 才代表输出确实有丢失。

`MAX_COMMAND_OUTPUT_CHARS` 只控制内存中的 rolling inline preview，不再是完整命令输出的总容量。不要通过无限提高该值试图突破 GPT Actions 的 JSON payload 限制。

`timeout_seconds` 只能缩短本次调用的时间，不能突破服务器端 `COMMAND_TIMEOUT_SECONDS`。超时后 Agent 会终止整个 shell 进程组，避免留下意外的子进程。

## 环境与凭据

systemd 服务以 `root:root` 运行，并提供常见工具安装路径：

```text
/root/.local/bin
/root/.cargo/bin
/root/go/bin
/usr/local/go/bin
/usr/local/sbin
/usr/local/bin
/usr/sbin
/usr/bin
/sbin
/bin
/snap/bin
```

同时保留宿主机已有 PATH，并使用 root 的 login shell 配置。

`/etc/mygpt-github-agent.env` 中除了 Agent 自己的配置外，其他环境变量也会被 `runCommand` 子进程继承。这样第三方 CLI 的 token 或非秘密配置可以留在 VPS，而不需要写进 GPT 指令。

不要让模型为了“检查配置”而输出完整 `env`、token 文件或密钥。认证状态应尽量通过对应 CLI 的 status/auth 命令检查。

## 安装 / 升级

当前仍保留历史二进制和 systemd 单元名称 `mygpt-github-agent`，这是为了兼容已部署服务器的原地升级；它们不再代表 Agent 的能力边界。

服务本身构建只要求 Go 1.23+：

```bash
git clone https://github.com/xiaoqianran/mygpt-cf-tunnel.git
cd mygpt-cf-tunnel
bash ./scripts/install.sh
```

默认配置：

```dotenv
API_TOKEN=<random secret>
LISTEN_ADDR=127.0.0.1:8787
COMMAND_TIMEOUT_SECONDS=86400
MAX_COMMAND_OUTPUT_CHARS=180000
```

安装后检查：

```bash
curl http://127.0.0.1:8787/health
curl -s http://127.0.0.1:8787/openapi.json | jq '.paths'
systemctl status mygpt-github-agent
```

## Cloudflare Tunnel

Agent 默认只监听：

```text
127.0.0.1:8787
```

Cloudflare Tunnel 的 Published application 指向：

```text
http://localhost:8787
```

然后在 Custom GPT Actions 中导入：

```text
https://<你的域名>/openapi.json
```

认证使用 Bearer API Key，值为 `/etc/mygpt-github-agent.env` 中的 `API_TOKEN`。

## 安全模型

这是有意设计的高权限执行入口：

```text
Cloudflare Tunnel
  -> Bearer API_TOKEN
  -> root systemd service
  -> /bin/bash -lc
  -> VPS root 权限
```

因此：

- `API_TOKEN` 等价于高权限远程执行凭据，必须保密。
- Agent origin 应继续只监听 loopback，不要直接暴露公网。
- root shell 可以安装软件、修改系统、删除文件、停止服务、访问 root 可读取的凭据；这是设计目标，不是 sandbox。
- OpenAPI 把操作标记为 non-consequential，以避免已授权场景下的逐次确认；工具本身仍具有真实 root 级副作用。
- `MAX_COMMAND_OUTPUT_CHARS` 只限制 rolling 内联预览；大型完整输出通过短期 `openaiFileResponse` 附件返回。超时由 `COMMAND_TIMEOUT_SECONDS` 控制。
