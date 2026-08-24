#!/usr/bin/env bash
# mygpt-cf-tunnel 一键部署/重启脚本（容器内直跑版，无 systemd）
# 说明：不依赖 Caddy。公网入口由已存在的 cloudflared 隧道负责，
#       本脚本只负责把 mygpt-cf-tunnel 跑起来并监听匹配的回源地址。
#
# 全部参数通过环境变量控制：
#   API_TOKEN      （可选）自定义 API 令牌，不传则自动生成随机 token
#   DOMAIN         （可选）默认 cnb.202820.xyz
#   LISTEN_ADDR    （可选）默认 127.0.0.1:8787，必须 = Dashboard 隧道回源地址
#   WORKSPACE_ROOT （可选）默认 /workspace/wk
#
# 用法：
#   sudo ./run.sh
#   API_TOKEN="xxx" DOMAIN="a.com" LISTEN_ADDR="127.0.0.1:9000" WORKSPACE_ROOT="/data" sudo ./run.sh
set -euo pipefail

# ===== 配置项（带默认值，可被环境变量覆盖） =====
DOMAIN="${DOMAIN:-cnb.202820.xyz}"
LISTEN_ADDR="${LISTEN_ADDR:-127.0.0.1:8787}"
WORKSPACE_ROOT="${WORKSPACE_ROOT:-/workspace/wk}"
STATE_DIR="/var/lib/mygpt-cf-tunnel"
# ================================================

# API_TOKEN：可选，不传则自动生成随机 token
if [ -n "${API_TOKEN:-}" ]; then
  TOKEN_SECRET="${API_TOKEN}"
else
  TOKEN_SECRET="$(openssl rand -hex 32)"
fi

cd "$(dirname "$0")"

echo "配置确认: DOMAIN=${DOMAIN}  LISTEN_ADDR=${LISTEN_ADDR}  WORKSPACE_ROOT=${WORKSPACE_ROOT}"

echo "[0/4] 创建工作目录"
mkdir -p "${WORKSPACE_ROOT}"

echo "[1/4] 编译二进制"
make check build >/dev/null 2>&1 || make build
install -m 0755 bin/mygpt-cf-tunnel  /usr/local/bin/mygpt-cf-tunnel
install -m 0755 bin/mygpt-audit  /usr/local/bin/mygpt-audit

echo "[2/4] 生成配置 /etc/mygpt-cf-tunnel.env"
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

echo "[3/4] 启动 mygpt-cf-tunnel"
pkill -f '/usr/local/bin/mygpt-cf-tunnel' 2>/dev/null || true
sleep 1
rm -f /var/run/mygpt-cf-tunnel.sock
set -a; source /etc/mygpt-cf-tunnel.env; set +a
nohup /usr/local/bin/mygpt-cf-tunnel > /var/log/mygpt-cf-tunnel.log 2>&1 &
sleep 2

echo "[4/4] 验证"
curl -fsS "http://127.0.0.1:${LISTEN_ADDR##*:}/health" && echo "  <- 本地 OK"

TOKEN=$(grep '^API_TOKEN=' /etc/mygpt-cf-tunnel.env | cut -d= -f2-)
echo ""
echo "===== 完成 ====="
echo "公网健康检查: curl -sS https://${DOMAIN}/health"
echo "导入 Schema : https://${DOMAIN}/openapi.json"
echo "API Token   : ${TOKEN}"
