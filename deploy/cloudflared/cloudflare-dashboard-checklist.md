# Cloudflare 边缘层“零限制”配置清单

这些规则用于彻底放行 OpenAI Custom GPT Action 的流量，消除 `ClientResponseError`、`403 Forbidden`、`HTML parse error` 的常见根因，并压榨网络协议极限。

以下均在 **Cloudflare Dashboard -> 你的域名** 下操作，`action.你的域名.com` 为 Action 子域名。

## 1. WAF 终极“免死金牌”规则 (Skip All Security)

前往 **Security (安全性) -> WAF -> Custom rules (自定义规则)**，创建一条优先级第一的规则：

- **Rule name**: `Allow-OpenAI-Action-Endpoint`
- **Expression (匹配表达式)**:
  ```text
  (http.host eq "action.你的域名.com")
  ```
- **Action**: **Skip**
- **WAF components to skip (勾选全部跳过项)**:
  - ☑ **All remaining custom rules**
  - ☑ **All rate limiting rules**
  - ☑ **All managed rules** (跳过 OWASP 等托管规则集，防止命令文本触发 SQLi/XSS 拦截)
  - ☑ **All Super Bot Fight Mode Rules**
  - ☑ **Browser Integrity Check (BIC)**
  - ☑ **Security Level** (直接置为 Off)
  - ☑ **User Agent Blocking**

## 2. 关闭全局 Bot Fight Mode

前往 **Security -> Bots**，将 **Bot Fight Mode** 设为 **Off**（免费版必须关闭；Pro 以上可通过上面的 Skip 规则免除）。

## 3. 边缘缓存穿透规则 (Cache Rules)

前往 **Caching -> Cache Rules**：

- **Rule 1: Bypass Action Command**
  - Expression: `(http.host eq "action.你的域名.com" and http.request.uri.path eq "/v1/command/run")`
  - Action: **Bypass cache**

- **Rule 2: Dynamic Health Check**
  - Expression: `(http.host eq "action.你的域名.com" and http.request.uri.path eq "/health")`
  - Action: **Bypass cache**

## 4. 链路追踪透传 (Transform Rules - Header Rewrite)

把 Cloudflare 全球唯一 `CF-Ray` 透传为 `X-Request-Id`：

前往 **Rules -> Transform Rules -> Modify Request Header**：

- **Rule name**: `Map-CF-Ray-To-Request-Id`
- **Expression**: `(http.host eq "action.你的域名.com")`
- **Action**: **Set dynamic**
  - Header name: `X-Request-Id`
  - Value: `http.request.headers["cf-ray"][0]`

> 服务端 `trustedRequestID` 优先读取 `X-Request-Id`，缺失时回退读取 `CF-Ray`。若未配置此 Transform Rule，服务端仍可直接读取 `CF-Ray`。

## 5. 开启最快网络协议 (Network Settings)

前往 **Speed -> Optimization -> Protocol Optimization**（或 **Network** 标签）：

- **HTTP/3 (with QUIC)**: **Enabled**
- **0-RTT Connection Resumption**: **Enabled** (零往返建连加速)
- **gRPC / WebSockets**: **Enabled**
- **Brotli / Early Hints**: **Enabled**
