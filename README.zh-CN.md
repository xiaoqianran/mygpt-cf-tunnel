# mygpt-cf-tunnel 中文说明

[![test](https://github.com/xiaoqianran/mygpt-cf-tunnel/actions/workflows/test.yml/badge.svg)](https://github.com/xiaoqianran/mygpt-cf-tunnel/actions/workflows/test.yml)

> 把 Custom GPT 连接到一台真实 VPS 的 **root shell**。对 GPT 暴露同一个 root shell 的同步与异步生命周期：短任务用 `runCommand`，长任务用异步 Job；服务器缺什么能力就安装什么，再继续完成工作流。

**仓库主页：** [README.md](./README.md) · **GPT Builder 指令：** [GPT_INSTRUCTIONS.md](./GPT_INSTRUCTIONS.md)

## 一句话理解

这个项目不是“给 GPT 准备很多专用 API”，而是只提供一个足够底层的执行原语，并为它提供同步与异步生命周期：

```text
runCommand / async job = 远程 VPS 上的 root /bin/bash -lc
```

因此能力不由 OpenAPI 中列出的工具决定，而由这台 VPS 最终能够安装、运行和访问的东西决定。

`git`、`gh`、Python、Node、Go、Docker、Modal、Kaggle、数据库客户端、云平台 CLI、系统包管理器等都只是可能用到的工具，**不是能力白名单，也不是功能边界**。

## 为什么只保留一个执行原语

传统做法往往为每种任务设计一个接口：

```text
readFiles
applyChanges
gitDiff
commitAndPush
createRelease
installPackage
restartService
...
```

这会把模型锁进预先设计好的能力集合。一旦出现新的工具或工作流，就需要继续扩展 API。

本项目选择另一种架构：

```text
Custom GPT
    │
    │ HTTPS + Bearer API_TOKEN
    ▼
Cloudflare Tunnel
    │
    ▼
127.0.0.1:8787
Universal VPS Root Shell Agent
    │
    ├── POST /v1/command/run
    └── POST /v1/command/start → job id
            │
            └── /bin/bash -lc <command>  (root)
                    │
                    ├── 使用服务器已有程序
                    ├── apt / pip / npm / cargo / go install / curl ...
                    ├── 安装新的 CLI、库、运行时或系统包
                    ├── 读写文件、编译代码、调用 API
                    ├── 管理进程、systemd、部署和网络任务
                    └── 组合服务器能执行的任意非交互式工作流
```

如果能力缺失，正确的思路不是“这个 Action 不支持”，而是：

```text
检查缺什么
  -> 在 VPS 上安装/构建/下载所需能力
  -> 继续原工作流
```

## 当前能力模型

`runCommand` 在真实宿主机上运行，不在仓库 sandbox 或临时容器中。

它可以：

- 以 root 身份执行 shell 命令和多行脚本；
- 在 root 可访问的任意主机目录中工作；
- 读取、创建、修改和删除文件；
- 使用已有 CLI、SDK、编译器和运行时；
- 通过 `apt`、语言包管理器、官方安装器、源码构建等方式获得新能力；
- 调用 HTTP/API、云平台、GitHub、数据库和其他远程服务；
- 执行测试、构建、发布、部署和系统维护；
- 管理进程和 systemd 服务；
- 使用 VPS 上已经配置好的环境变量和 CLI 凭据；
- 通过 `stdin` 给非交互式命令传入脚本、payload 或文件内容。

这些示例只用于说明能力模型，不代表固定支持列表。

## 30 秒快速开始

### 1. 准备 VPS

Agent 本身只要求：

- Linux VPS；
- root 权限；
- Go 1.23+；
- 一个可以把本地 `127.0.0.1:8787` 发布出去的 Cloudflare Tunnel。

克隆并安装：

```bash
git clone https://github.com/xiaoqianran/mygpt-cf-tunnel.git
cd mygpt-cf-tunnel
bash ./scripts/install.sh
```

首次安装会创建：

```text
/etc/mygpt-github-agent.env
```

并生成随机 `API_TOKEN`。

> `mygpt-github-agent` 是历史兼容名称。当前 Agent 已经不是 GitHub 专用服务，保留旧名称是为了让已有服务器可以原地升级。

### 2. 检查服务

```bash
curl http://127.0.0.1:8787/health
curl -s http://127.0.0.1:8787/openapi.json | jq '.paths'
systemctl status mygpt-github-agent
```

正常情况下 OpenAPI 应看到：

```text
/v1/command/run
/v1/command/start
/v1/command/jobs/{id}
/v1/command/jobs/{id}/cancel
```

### 3. 配置 Cloudflare Tunnel

Agent 默认只监听：

```text
127.0.0.1:8787
```

Cloudflare Tunnel 的 Published application 指向：

```text
http://localhost:8787
```

例如最终得到：

```text
https://agent.example.com
```

不要把 Agent 的监听端口直接暴露到公网。

### 4. 导入 Custom GPT Action

在 GPT Builder 的 Actions 中导入：

```text
https://agent.example.com/openapi.json
```

认证方式使用 Bearer API Key，值为服务器：

```text
/etc/mygpt-github-agent.env
```

中的 `API_TOKEN`。

推荐的 GPT 描述：

> 连接远程 VPS 的 root shell。通过 `runCommand`（短任务）或异步 Job（长任务）使用或自主安装所需软件，组合命令、程序、服务与网络能力，并完成服务器能够执行的任意工作流。

完整 Instructions 直接使用：

[`GPT_INSTRUCTIONS.md`](./GPT_INSTRUCTIONS.md)

## `runCommand` API

请求：

```json
{
  "command": "set -euo pipefail\nuname -a\ncommand -v jq || apt-get update && apt-get install -y jq\njq --version",
  "workdir": "/root",
  "stdin": "",
  "timeout_seconds": 300
}
```

字段：

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `command` | 是 | 交给 `/bin/bash -lc` 的命令或多行 shell 脚本 |
| `workdir` | 否 | 真实 VPS 工作目录；默认 `/root` |
| `stdin` | 否 | 传给命令的标准输入 |
| `timeout_seconds` | 否 | 单次调用超时，只能小于等于服务端上限 |

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

模型应该根据 `exit_code`、`stdout`、`stderr`、`timed_out` 和 `truncated` 判断下一步，而不是只看 HTTP 200。

## 运行环境

systemd 服务明确以：

```text
User=root
Group=root
HOME=/root
SHELL=/bin/bash
```

运行。

Agent 会为 shell 补充常见安装路径：

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

同时保留宿主机已有 PATH。

这意味着通过 `pip --user`、Cargo、Go、系统包管理器、手动安装到 `/usr/local/bin` 等方式获得的 CLI 都能自然进入后续工作流。

## 环境变量与凭据

默认配置：

```dotenv
API_TOKEN=<random secret>
LISTEN_ADDR=127.0.0.1:8787
COMMAND_TIMEOUT_SECONDS=86400
MAX_COMMAND_OUTPUT_CHARS=180000
```

systemd 的 EnvironmentFile 是：

```text
/etc/mygpt-github-agent.env
```

其中额外配置的环境变量也会被 `runCommand` 子进程继承，因此第三方服务凭据可以留在 VPS，而不是写进 GPT Instructions。

推荐原则：

- 凭据放在 VPS / 对应 CLI 配置目录；
- GPT 只检查认证状态，不读取完整 token；
- 不在最终回答中输出服务器秘密；
- 不把 Cloudflare Tunnel token、GitHub token、云平台 token 写进仓库。

## 超时、输出与子进程

### 超时

Custom GPT Action 层本身有 **45 秒 round-trip** 硬超时；因此同步 `runCommand` 即使服务端允许更久，也不适合可能接近 45 秒的工作。长任务应使用 `startCommand` + `getCommandJob`，把命令生命周期与单次 HTTP Action 解耦。

服务端默认：

```text
COMMAND_TIMEOUT_SECONDS=86400
```

请求中的 `timeout_seconds` 可以缩短单次调用，但不能突破服务器上限。

发生超时时，Agent 会终止当前 shell 的整个进程组，避免只杀掉外层 bash 后留下子进程继续运行。

### 大输出与 GPT Action 文件附件

GPT Actions 的普通 JSON request/response 有独立的平台 payload 上限，因此提高单个 JSON `stdout` 的尺寸并不能无限扩大 GPT 实际收到的输出。`MAX_COMMAND_OUTPUT_CHARS` 现在只控制内存中的滚动 stdout/stderr 预览：

```text
MAX_COMMAND_OUTPUT_CHARS=180000
```

对于能安全放进 Action JSON 的小输出，`runCommand` / 终态 `getCommandJob` 仍直接返回完整 `stdout` 和 `stderr`。一旦输出过大，Agent 会自动把完整命令输出保存为短期文本文件，通过 GPT Actions 官方 `openaiFileResponse` 机制返回，同时只在 JSON 中保留小型预览。

当前实现每个附件片段最多约 **9.5 MB**，stdout/stderr 合计最多 **10 个附件**，即一次命令最多约 **95 MB** 的完整文本输出可以进入对话；这是在 GPT Actions 官方“最多 10 个文件、每个最多 10 MB”约束下保留安全余量。同步命令生成的附件 URL 使用随机 256-bit token，默认 15 分钟失效。

返回字段的语义：

- `truncated: false` + `inline_truncated` 缺省/false：完整输出已直接内联；
- `truncated: false` + `inline_truncated: true` + `full_output_attached: true`：JSON 里的 stdout/stderr 只是预览，但**完整命令输出没有丢失，已经作为附件返回**；
- `truncated: true` / `capture_truncated: true`：命令输出超过附件捕获容量或捕获失败，此时才意味着完整输出确实没有全部保存。

因此，大日志不再因为 `MAX_COMMAND_OUTPUT_CHARS` 被永久丢掉，也不需要仅为了绕过传输上限而手工改成 `grep` / `tail`。定位型命令仍然适合减少无关上下文，但它变成效率选择，而不是防截断的必需操作。

## 非交互式工作流

`runCommand` 不是交互式 TTY。

因此 CLI 自动化时优先使用：

```text
--yes
--non-interactive
stdin
环境变量
配置文件
服务端认证状态
```

`stdin` 适合发送：

- JSON / YAML payload；
- SQL；
- 小型脚本；
- 配置片段；
- CLI 的非交互输入。

长任务应优先使用 `startCommand`。它立即返回 job id，不让 Cloudflare HTTP 请求等待命令结束；随后使用 `getCommandJob` 查询，必要时用 `cancelCommandJob` 取消。异步 Job 不绑定原始 HTTP request context，因此代理连接结束不会自动终止任务。终态 Job 最多保留 24 小时且最多保留最近 256 条；Agent 重启后会清空。更复杂的持久任务仍可交给 `systemd-run`、队列或云平台 Job。

## Agent 可以升级自己

安装脚本专门处理了一个重要场景：**通过当前 Agent 的 `runCommand` 升级 Agent 自己**。

如果 `scripts/install.sh` 检测到自己正在 `mygpt-github-agent.service` 的 cgroup 中执行，它不会同步 `systemctl restart` 把当前 HTTP 请求直接杀死，而是通过 transient systemd unit 延迟重启服务。

因此常见升级流程可以直接由远程 shell 完成：

```bash
cd /srv/mygpt/repos/xiaoqianran/mygpt-cf-tunnel
git pull --ff-only
bash ./scripts/install.sh
```

请求结束后再检查：

```bash
curl http://127.0.0.1:8787/health
systemctl is-active mygpt-github-agent
```

## OpenAPI 为什么刻意很短

GPT Instructions 和 OpenAPI 的职责不同：

```text
GPT Instructions
  -> 教模型怎样规划和使用能力

OpenAPI
  -> 只定义认证、输入、输出和副作用

Agent
  -> 真正执行 root shell
```

Custom GPT Builder 对 schema 结构和 description 长度有额外约束，所以项目不会把大量行为说明塞进 operation description。

当前 CI 会保护这些兼容约束：

- `components.schemas` 必须显式为 JSON object；
- OpenAPI description 保持在 Builder 可接受长度内；
- Action 面只包含同一个 shell 执行原语的同步与异步生命周期；
- `x-openai-isConsequential` 明确为 `false`，避免因 Action consequential 标记触发逐次确认。

## CI

GitHub Actions 会检查：

```text
gofmt
go test ./...
go vet ./...
Linux/ARM64 build
```

项目当前使用 `actions/checkout@v7` 和 `actions/setup-go@v7`。由于目前没有第三方 Go module 依赖，也没有 `go.sum`，setup-go cache 被刻意关闭，避免无意义的 cache restore 失败。

## 项目结构

```text
.
├── .github/workflows/test.yml       # CI
├── GPT_INSTRUCTIONS.md              # Custom GPT 推荐指令
├── README.md                        # 仓库主 README
├── README.zh-CN.md                  # 中文完整说明
├── cmd/mygpt-github-agent/          # Go 程序入口（历史名称）
├── deploy/                          # systemd unit
├── internal/agent/
│   ├── command.go                   # root shell 执行、超时、输出限制
│   ├── command_http.go              # runCommand HTTP handler
│   ├── config.go                    # 服务配置
│   ├── openapi.json                 # Builder Action schema
│   ├── openapi.go                   # embed OpenAPI
│   └── server.go                    # HTTP server / auth / health
└── scripts/install.sh               # 安装与原地升级
```

## 安全模型

这是一个**高权限远程代码执行入口**，不是受限代码沙箱。

```text
Internet
  -> Cloudflare Tunnel
  -> Bearer API_TOKEN
  -> loopback Agent
  -> root /bin/bash -lc
  -> VPS
```

因此必须把 `API_TOKEN` 当作 root 级远程执行凭据管理。

至少应做到：

- Agent origin 只监听 `127.0.0.1`；
- 只通过受控 Cloudflare Tunnel 暴露；
- 不在仓库、聊天记录或 GPT Instructions 中泄露服务器秘密；
- 对删除数据、覆盖系统配置、停止关键服务等不可逆操作保持明确目标和影响范围；
- 定期检查 systemd 日志、VPS 更新和 Cloudflare 访问策略。

OpenAPI 中：

```json
"x-openai-isConsequential": false
```

这是有意的，用于避免每次 `runCommand` 都因 consequential 标记触发重复确认。注意：该工具仍然拥有 root 级真实系统副作用，平台也可能对部分高风险操作继续要求确认。

## 常见故障排查

### Builder 提示 `components.schemas` 不是 object

线上 OpenAPI 应包含：

```json
"schemas": {}
```

检查：

```bash
curl -s https://agent.example.com/openapi.json | jq '.components.schemas'
```

### Builder 提示 description 超过 300 字符

不要继续把能力说明堆进 OpenAPI。保持 OpenAPI 简短，把行为策略写进 `GPT_INSTRUCTIONS.md`。

### 命令在 SSH 中存在，但 Agent 找不到

先检查：

```bash
command -v <tool>
printf '%s\n' "$PATH"
```

如果工具安装在非常规目录，可以加入 systemd EnvironmentFile 或在命令中显式扩展 PATH。

### 命令需要交互

改用 CLI 的 non-interactive 参数、配置文件、环境变量或 `stdin`。`runCommand` 不提供交互式终端会话。

### 更新后服务没有切换版本

检查：

```bash
systemctl status mygpt-github-agent
journalctl -u mygpt-github-agent -n 100 --no-pager
curl http://127.0.0.1:8787/health
```

## 设计边界

这个项目刻意保持 Agent 很薄：

```text
认证 + OpenAPI + root shell + 超时 + 输出限制 + 进程清理
```

它不试图重新实现 GitHub SDK、包管理器、云平台 SDK 或部署系统。

**操作系统和 CLI 生态本身就是能力层。**

这也是为什么项目最终只需要一个 shell 执行原语；短任务同步执行，长任务使用异步 Job。

## Cloudflare 524 超时事实

Cloudflare 当前官方文档与本地下载快照见 [`CLOUDFLARE_TIMEOUTS.md`](./CLOUDFLARE_TIMEOUTS.md)。默认 Proxy Read Timeout 当前为 **125 秒**，不要使用旧的 100 秒记忆。

长任务使用 `startCommand` 后，通过 `getCommandJob` 观察。任务处于 `running` 时会返回最新 rolling `stdout` / `stderr`、持续增长的 `duration_ms` 和单调递增的 `revision`。把上次 revision 作为 `after`，推荐 `wait_seconds=10&tail_chars=12000`：日志或状态变化会立即唤醒请求，不需要固定 `sleep`；完全无变化时才 heartbeat 返回。普通 GET 仍可立即取得完整 rolling snapshot。观察请求断开不会取消后台任务，需要停止时显式调用 `cancelCommandJob`。
