#!/usr/bin/env bash
# =====================================================================
# 新服务器一键部署脚本：cloudflared 隧道 + mygpt-cf-tunnel 服务
# 用法（root 下）:
#   TUNNEL_TOKEN="<你的隧道token>" API_TOKEN="<你的自定义API令牌>" bash setup-new-server.sh
#   自定义 API_TOKEN 可选：不传则自动生成随机 token。
# 铁律: LISTEN_ADDR 必须等于 Cloudflare Dashboard 里隧道的回源地址
# =====================================================================
set -euo pipefail

# ===== 唯一需要改的配置 =====
DOMAIN="cnb.202820.xyz"                 # 你的公网域名
LISTEN_ADDR="127.0.0.1:8787"            # 必须 = Dashboard 隧道回源地址
WORKSPACE_ROOT="/workspace/wk"
STATE_DIR="/var/lib/mygpt-cf-tunnel"
# =============================

# 隧道 token 从环境变量读，绝不写死在仓库文件里（避免凭据泄露）
if [ -z "${TUNNEL_TOKEN:-}" ]; then
  echo "错误: 未提供隧道 token。用法: TUNNEL_TOKEN='<token>' bash $0" >&2
  exit 1
fi

# 自定义 API_TOKEN：可用环境变量 API_TOKEN 传入，否则自动生成随机 token。
if [ -n "${API_TOKEN:-}" ]; then
  TOKEN_SECRET="${API_TOKEN}"
  echo "使用自定义 API_TOKEN（通过环境变量传入）"
else
  TOKEN_SECRET="$(openssl rand -hex 32)"
fi

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
  echo "    /etc/mygpt-cf-tunnel.env 已存在"
  # 若显式传入了自定义 API_TOKEN，则同步更新；否则保留原 token
  if [ -n "${API_TOKEN:-}" ]; then
    echo "    同步自定义 API_TOKEN"
    sed -i "s|^API_TOKEN=.*|API_TOKEN=${TOKEN_SECRET}|" /etc/mygpt-cf-tunnel.env
    grep -q '^API_TOKEN=' /etc/mygpt-cf-tunnel.env || echo "API_TOKEN=${TOKEN_SECRET}" >> /etc/mygpt-cf-tunnel.env
  fi
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
