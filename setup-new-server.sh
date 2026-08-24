#!/usr/bin/env bash
# =====================================================================
# 新服务器一键部署脚本：cloudflared 隧道 + mygpt-cf-tunnel 服务
#
# 全部参数通过环境变量控制：
#   TUNNEL_TOKEN   （必填）你的 Cloudflare 隧道 token，无默认值
#   API_TOKEN      （可选）自定义 API 令牌，不传则自动生成随机 token
#   DOMAIN         （可选）默认 cnb.202820.xyz
#   LISTEN_ADDR    （可选）默认 127.0.0.1:8787，必须 = Dashboard 隧道回源地址
#   WORKSPACE_ROOT （可选）默认 /workspace/wk
#
# 用法（root 下）:
#   TUNNEL_TOKEN="<token>" bash setup-new-server.sh
#   TUNNEL_TOKEN="<token>" API_TOKEN="xxx" DOMAIN="a.com" \
#     LISTEN_ADDR="127.0.0.1:9000" WORKSPACE_ROOT="/data" bash setup-new-server.sh
#
# 安全：token 一律走环境变量，绝不写死进仓库文件。
# =====================================================================
set -euo pipefail

# ===== 配置项（带默认值，可被环境变量覆盖） =====
DOMAIN="${DOMAIN:-cnb.202820.xyz}"
LISTEN_ADDR="${LISTEN_ADDR:-127.0.0.1:8787}"
WORKSPACE_ROOT="${WORKSPACE_ROOT:-/workspace/wk}"
STATE_DIR="/var/lib/mygpt-cf-tunnel"
# ================================================

# 隧道 token：必填，从环境变量读
if [ -z "${TUNNEL_TOKEN:-}" ]; then
  echo "错误: 未提供隧道 token。" >&2
  echo "用法: TUNNEL_TOKEN='<token>' [API_TOKEN='xxx'] [DOMAIN='...'] [LISTEN_ADDR='...'] [WORKSPACE_ROOT='...'] bash $0" >&2
  exit 1
fi

# API_TOKEN：可选，不传则自动生成随机 token
if [ -n "${API_TOKEN:-}" ]; then
  TOKEN_SECRET="${API_TOKEN}"
  echo "使用自定义 API_TOKEN（环境变量传入）"
else
  TOKEN_SECRET="$(openssl rand -hex 32)"
  echo "未传入 API_TOKEN，已自动生成随机 token"
fi

echo "配置确认: DOMAIN=${DOMAIN}  LISTEN_ADDR=${LISTEN_ADDR}  WORKSPACE_ROOT=${WORKSPACE_ROOT}"

echo "===== [1/5] 安装 cloudflared ====="
mkdir -p --mode=0755 /usr/share/keyrings
curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg | tee /usr/share/keyrings/cloudflare-main.gpg >/dev/null
echo "deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main" > /etc/apt/sources.list.d/cloudflared.list
apt-get update -qq && apt-get install -y cloudflared

echo "===== [2/5] 注册隧道为系统服务 ====="
# 同一隧道用同一个 token；重复执行只是重配同一条隧道，不会冲突
cloudflared service install "${TUNNEL_TOKEN}"
systemctl enable cloudflared >/dev/null 2>&1 || true
systemctl restart cloudflared

echo "===== [3/5] 编译并安装 mygpt-cf-tunnel ====="
cd "$(dirname "$0")"
mkdir -p "${WORKSPACE_ROOT}"
make check build >/dev/null 2>&1 || make build
install -m 0755 bin/mygpt-cf-tunnel /usr/local/bin/mygpt-cf-tunnel
install -m 0755 bin/mygpt-audit /usr/local/bin/mygpt-audit

echo "===== [4/5] 生成配置并启动服务 ====="
if [ ! -f /etc/mygpt-cf-tunnel.env ]; then
  umask 077
  cat > /etc/mygpt-cf-tunnel.env <<EOF
API_TOKEN=${TOKEN_SECRET}
ACTION_BASE_URL=https://${DOMAIN}
LISTEN_ADDR=${LISTEN_ADDR}
WORKSPACE_ROOT=${WORKSPACE_ROOT}
STATE_DIR=${STATE_DIR}
COMMAND_TIMEOUT_SECONDS=38
INLINE_OUTPUT_CHARS=30000
MAX_ARTIFACT_BYTES=10000000
MAX_INPUT_FILE_BYTES=10000000
ARTIFACT_TTL_SECONDS=900
ALLOWED_GPT_IDS=
ALLOWED_UPLOAD_HOSTS=.oaiusercontent.com
AUDIT_ENABLED=true
AUDIT_DIR=${STATE_DIR}/audit
AUDIT_RETENTION_DAYS=30
AUDIT_FSYNC=true
AUDIT_OUTPUT_CHARS=4000
EOF
else
  echo "    /etc/mygpt-cf-tunnel.env 已存在，按需同步传入的参数"
  # 仅当显式传入时才覆盖对应配置；未传则保留原值
  if [ -n "${API_TOKEN:-}" ]; then
    sed -i "s|^API_TOKEN=.*|API_TOKEN=${TOKEN_SECRET}|" /etc/mygpt-cf-tunnel.env
    grep -q '^API_TOKEN=' /etc/mygpt-cf-tunnel.env || echo "API_TOKEN=${TOKEN_SECRET}" >> /etc/mygpt-cf-tunnel.env
  fi
  sed -i "s|^ACTION_BASE_URL=.*|ACTION_BASE_URL=https://${DOMAIN}|" /etc/mygpt-cf-tunnel.env
  grep -q '^ACTION_BASE_URL=' /etc/mygpt-cf-tunnel.env || echo "ACTION_BASE_URL=https://${DOMAIN}" >> /etc/mygpt-cf-tunnel.env
  sed -i "s|^LISTEN_ADDR=.*|LISTEN_ADDR=${LISTEN_ADDR}|" /etc/mygpt-cf-tunnel.env
  grep -q '^LISTEN_ADDR=' /etc/mygpt-cf-tunnel.env || echo "LISTEN_ADDR=${LISTEN_ADDR}" >> /etc/mygpt-cf-tunnel.env
  sed -i "s|^WORKSPACE_ROOT=.*|WORKSPACE_ROOT=${WORKSPACE_ROOT}|" /etc/mygpt-cf-tunnel.env
  grep -q '^WORKSPACE_ROOT=' /etc/mygpt-cf-tunnel.env || echo "WORKSPACE_ROOT=${WORKSPACE_ROOT}" >> /etc/mygpt-cf-tunnel.env
fi
pkill -f '/usr/local/bin/mygpt-cf-tunnel' 2>/dev/null || true
set -a; source /etc/mygpt-cf-tunnel.env; set +a
nohup /usr/local/bin/mygpt-cf-tunnel > /var/log/mygpt-cf-tunnel.log 2>&1 &
sleep 3

echo "===== [5/5] 验证 ====="
curl -fsS "http://127.0.0.1:${LISTEN_ADDR##*:}/health" && echo " <- 本地 OK"
sleep 2
curl -fsS -m 20 "https://${DOMAIN}/health" && echo " <- 公网 OK"

TOKEN=$(grep '^API_TOKEN=' /etc/mygpt-cf-tunnel.env | cut -d= -f2-)
echo ""
echo "===== 完成 ====="
echo "公网健康检查: curl -sS https://${DOMAIN}/health"
echo "导入 Schema : https://${DOMAIN}/openapi.json"
echo "API Token   : ${TOKEN}"
