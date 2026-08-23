# GPT Action and Cloudflare timeout facts for this project

GPT Actions limits were verified against OpenAI official documentation on **2026-08-24**. Cloudflare limits were verified against Cloudflare official documentation on **2026-08-23**.

## The first timeout that matters in Custom GPT

OpenAI currently documents a **45-second round-trip timeout for GPT Action API calls**. It also documents that normal Action request and response payloads must each be **less than 100,000 characters**.

For this project that means the Custom GPT path normally hits the OpenAI Action timeout before Cloudflare's 125-second proxy read timeout. Use `runCommand` only for work that is expected to finish comfortably below the Action boundary. Use `startCommand` for builds, deploys, installs, model jobs, large exports, or any workflow that may approach 45 seconds; observe it with `getCommandJob`.

Large output is a separate transport problem. Do not increase inline JSON until it crosses the 100,000-character Action limit. The Agent keeps the Action JSON small and returns oversized stdout/stderr through GPT Actions' official `openaiFileResponse` file mechanism.

OpenAI source: `https://developers.openai.com/api/docs/actions/production`.

## The number that matters for HTTP 524

For proxied HTTP/HTTPS traffic, Cloudflare's default **Proxy Read Timeout is 125 seconds**. If Cloudflare has connected to the origin but the origin does not provide an HTTP response within that read timeout, Cloudflare can return **HTTP 524**.

Do not rely on the older, commonly repeated **100-second** figure for this project. Cloudflare's current official 524 documentation says **125 seconds by default**.

Cloudflare also documents a **Proxy Write Timeout of 30 seconds** for 524-related origin writes. This write timeout is not adjustable.

For **Enterprise** zones, Cloudflare documents that the Proxy Read Timeout can be increased up to **6,000 seconds**. Cloudflare notes that the observed 524 may differ by roughly one second from the configured value because of its Pingora proxy implementation.

## Important interpretation

A 524 does **not** mean the shell command, VPS, `cloudflared`, or downstream CLI necessarily failed. It means Cloudflare connected to the origin, but the proxied HTTP transaction did not produce the required response activity before Cloudflare's timeout boundary.

For this repository, a long synchronous call such as:

```text
POST /v1/command/run
    -> bash -lc "modal run ..."
    -> wait for command exit
    -> HTTP response
```

can therefore outlive Cloudflare's proxy read window even when the command is still running correctly on the VPS.

Cloudflare's own 524 guidance recommends **status polling for large HTTP processes**. That is why this project provides asynchronous command jobs:

```text
POST /v1/command/start        -> returns job id immediately
GET  /v1/command/jobs/{id}    -> poll status/result
POST /v1/command/jobs/{id}/cancel
```

Use `runCommand` for short commands. Prefer `startCommand` for builds, deploys, installs, model jobs, large exports, or anything that might approach the earlier **45-second GPT Action timeout**; the Cloudflare 125-second boundary still matters for non-Action/direct clients.

For live observation, `getCommandJob` uses revision-based long polling with a 10-second default and 20-second maximum wait. A request returns sooner whenever stdout, stderr, or job status changes. These waits stay intentionally far below Cloudflare's documented 125-second default Proxy Read Timeout.

## Other Cloudflare connection limits worth distinguishing

Cloudflare's current connection-limits table lists these Cloudflare-to-origin defaults:

| Limit | Default | Typical status |
| --- | ---: | --- |
| Complete TCP connection | 19 s | 522 |
| TCP ACK timeout | 90 s | 522 |
| Proxy read timeout | **125 s** | **524** |
| Proxy write timeout | **30 s** | **524** |
| Proxy idle timeout | 900 s | 520 |

These are different failure modes. Do not diagnose every long request as the same timeout.

## Cloudflare Tunnel wording

This repository reaches the origin through Cloudflare Tunnel, but **524 is an HTTP proxy timeout**, not evidence that the `cloudflared` tunnel process itself timed out or crashed. Cloudflare's connection-limits documentation explicitly separates proxied HTTP limits from Tunnel origin-connection settings.

## Official sources

- Cloudflare Support: `Error 524`  
  https://developers.cloudflare.com/support/troubleshooting/http-status-codes/cloudflare-5xx-errors/error-524/
- Cloudflare Fundamentals: `Connection limits`  
  https://developers.cloudflare.com/fundamentals/reference/connection-limits/

Downloaded snapshots captured on 2026-08-23 are stored in:

```text
docs/references/cloudflare-error-524.html
docs/references/cloudflare-connection-limits.html
```

Cloudflare may change these limits in the future. When this file and the live official documentation disagree, re-check the official pages and update this record rather than trusting stale model memory.
