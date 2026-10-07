#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ADMIN_DOMAIN="${1:-}"
PICKUP_DOMAIN="${2:-}"

usage() {
  cat <<'EOF'
用法：
  sudo bash ./admin-server/deploy.sh admin.example.com mail.example.com

部署前请把两个域名的 A/AAAA 记录指向当前服务器，并开放 TCP 80、443
以及 UDP 443。域名参数不要带 http://、https:// 或路径。
EOF
}

valid_domain() {
  [[ "$1" =~ ^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$ ]] &&
    [[ "$1" == *.* ]] &&
    [[ "$1" != *..* ]]
}

if ! valid_domain "$ADMIN_DOMAIN" || ! valid_domain "$PICKUP_DOMAIN"; then
  usage
  exit 2
fi
if [[ "$ADMIN_DOMAIN" == "$PICKUP_DOMAIN" ]]; then
  echo "管理端域名和取件端域名必须不同。" >&2
  exit 2
fi

if [[ "${EUID}" -ne 0 ]]; then
  if command -v sudo >/dev/null 2>&1; then
    exec sudo --preserve-env=PATH bash "$0" "$@"
  fi
  echo "请使用 root 或 sudo 运行此脚本。" >&2
  exit 1
fi

install_curl() {
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update
    DEBIAN_FRONTEND=noninteractive apt-get install -y curl ca-certificates
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y curl ca-certificates
  elif command -v yum >/dev/null 2>&1; then
    yum install -y curl ca-certificates
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache curl ca-certificates
  else
    echo "无法自动安装 curl，请先手动安装 Docker 和 Docker Compose。" >&2
    exit 1
  fi
}

if ! command -v docker >/dev/null 2>&1; then
  command -v curl >/dev/null 2>&1 || install_curl
  installer="$(mktemp)"
  trap 'rm -f -- "$installer"' EXIT
  curl -fsSL https://get.docker.com -o "$installer"
  sh "$installer"
fi

if command -v systemctl >/dev/null 2>&1; then
  systemctl enable --now docker
fi
if ! docker compose version >/dev/null 2>&1; then
  echo "当前 Docker 未安装 Compose 插件，请先安装 docker-compose-plugin。" >&2
  exit 1
fi

random_hex() {
  od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
}
random_base64() {
  head -c 32 /dev/urandom | base64 | tr -d '\n'
}
set_env_value() {
  local key="$1"
  local value="$2"
  local env_file="$SCRIPT_DIR/.env"
  if grep -q "^${key}=" "$env_file" 2>/dev/null; then
    sed -i "s|^${key}=.*|${key}=${value}|" "$env_file"
  else
    printf '%s=%s\n' "$key" "$value" >> "$env_file"
  fi
}

ENV_FILE="$SCRIPT_DIR/.env"
if [[ ! -f "$ENV_FILE" ]]; then
  umask 077
  {
    printf 'ADMIN_DOMAIN=%s\n' "$ADMIN_DOMAIN"
    printf 'PICKUP_DOMAIN=%s\n' "$PICKUP_DOMAIN"
    printf 'POSTGRES_PASSWORD=%s\n' "$(random_hex)"
    printf 'CYMAIL_INTERNAL_API_KEY=%s\n' "$(random_hex)"
    printf 'ICLOUD_HME_DATA_KEY=%s\n' "$(random_base64)"
  } > "$ENV_FILE"
else
  set_env_value ADMIN_DOMAIN "$ADMIN_DOMAIN"
  set_env_value PICKUP_DOMAIN "$PICKUP_DOMAIN"
  chmod 600 "$ENV_FILE"
fi

cd "$SCRIPT_DIR"
docker compose pull db caddy
docker compose up -d --build --remove-orphans

echo
echo "CYMail 已启动。首次签发 HTTPS 证书可能需要 1～3 分钟。"
echo "管理端：https://$ADMIN_DOMAIN/login.html"
echo "取件端：https://$PICKUP_DOMAIN/pickup"
echo
docker compose ps
