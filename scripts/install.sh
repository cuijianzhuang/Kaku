#!/usr/bin/env bash
# Kaku shop: one-step install and management (Linux + systemd).
#
#   bash <(curl -fsSL https://github.com/cuijianzhuang/Kaku/releases/latest/download/install.sh) install --domain shop.example.com
#
# Commands (after installing, this script is available as `kaku-ctl`):
#   install [--domain NAME] [--port N] [--version TAG] [--no-caddy]
#                         install the latest (or given) release; with --domain, Caddy serves HTTPS
#   update [--version TAG]  upgrade the binary (data and keys kept; rolls back if it does not start)
#   status | log | restart | stop | start
#   info                  show the shop and admin URLs and where the configuration is
#   backup                database + configuration (includes the master key) -> /root
#   reset-password        new random admin password (two-step verification is turned off)
#   run ARGS…             run kaku as the service, e.g. run version
#   uninstall [--purge]   remove; --purge also deletes the configuration, the master key and the data
#
# Downloads come from GitHub Releases. Where github.com is slow, set GH_PROXY
# to a download proxy prefix, e.g. GH_PROXY=https://ghproxy.example.com/
set -euo pipefail

REPO="cuijianzhuang/Kaku"
BIN=/usr/local/bin/kaku
CTL=/usr/local/bin/kaku-ctl
ETC=/etc/kaku
ENV_FILE=$ETC/kaku.env
DATA=/var/lib/kaku
UNIT=/etc/systemd/system/kaku.service
SVC_USER=kaku
GH_PROXY="${GH_PROXY:-}"

ACTION="" DOMAIN="" PORT=8080 VERSION="" CADDY=1 PURGE=0

if [ -t 1 ]; then C_R=$'\e[31m' C_G=$'\e[32m' C_Y=$'\e[33m' C_B=$'\e[1m' C_0=$'\e[0m'; else C_R="" C_G="" C_Y="" C_B="" C_0=""; fi
ok() { printf '  %s✓%s %s\n' "$C_G" "$C_0" "$*"; }
warn() { printf '  %s!%s %s\n' "$C_Y" "$C_0" "$*"; }
step() { printf '%s==>%s %s\n' "$C_B" "$C_0" "$*"; }
die() { printf '  %s✗ %s%s\n' "$C_R" "$*" "$C_0" >&2; exit 1; }
usage() { sed -n '2,19p' "$0" 2>/dev/null || echo "用法：install | update | status | log | restart | stop | start | info | backup | reset-password | uninstall"; }

need_root() { [ "$(id -u)" = 0 ] || die "请用 root 运行"; }
installed() { [ -x "$BIN" ] && [ -f "$ENV_FILE" ]; }
need_installed() { installed || die "还没有安装：先执行 install"; }
env_get() { sed -n "s/^$1=//p" "$ENV_FILE" 2>/dev/null | tail -n1; }
rand() { head -c 64 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | cut -c1-"$1"; }
rand_lower() { head -c 64 /dev/urandom | base64 | tr -dc 'a-z0-9' | cut -c1-"$1"; }

arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo amd64 ;;
    aarch64 | arm64) echo arm64 ;;
    armv7l | armv7*) echo armv7 ;;
    i386 | i686) echo 386 ;;
    *) die "不支持的架构：$(uname -m)（支持 amd64 / arm64 / armv7 / 386）" ;;
  esac
}

# --- GitHub Releases (public repository, no token needed) ---

gh_url() { printf '%s%s' "$GH_PROXY" "$1"; }

# latest_tag prints the tag the "latest release" page redirects to.
latest_tag() {
  local u
  u="$(curl -fsSLI -o /dev/null -w '%{url_effective}' --retry 2 "https://github.com/$REPO/releases/latest" 2>/dev/null)" || return 1
  u="${u%/}"
  printf '%s' "${u##*/}"
}

download() { # FILE DEST
  curl -fL -# --retry 2 --connect-timeout 15 -o "$2" "$(gh_url "https://github.com/$REPO/releases/download/$VERSION/$1")"
}

fetch_release() { # sets VERSION and TMP/kaku
  local a want got
  a="$(arch)"
  if [ -z "$VERSION" ]; then
    VERSION="$(latest_tag)" || true
    [[ "$VERSION" =~ ^v[0-9][0-9A-Za-z.+-]{0,39}$ ]] || die "读取最新版本失败：检查服务器能否访问 github.com（可设置 GH_PROXY）"
  fi
  [[ "$VERSION" =~ ^v[0-9][0-9A-Za-z.+-]{0,39}$ ]] || die "版本号格式不正确：$VERSION"
  TMP="$(mktemp -d)"
  trap 'rm -rf "$TMP"' EXIT
  step "下载 kaku $VERSION ($a)"
  download "kaku-linux-$a" "$TMP/kaku" || die "下载失败：$VERSION 中没有 kaku-linux-$a？"
  download checksums.txt "$TMP/checksums.txt" || die "下载 checksums.txt 失败"
  want="$(awk -v f="kaku-linux-$a" '$2 == f || $2 == "*"f {print $1}' "$TMP/checksums.txt")"
  got="$(sha256sum "$TMP/kaku" | awk '{print $1}')"
  [ -n "$want" ] && [ "$want" = "$got" ] || die "SHA-256 校验失败（期望 ${want:-无}，实际 $got）"
  chmod 0755 "$TMP/kaku"
  "$TMP/kaku" version >/dev/null 2>&1 || die "新程序无法在本机运行"
  ok "SHA-256 校验通过"
}

# --- service ---

write_unit() {
  cat > "$UNIT" <<EOF
[Unit]
Description=Kaku shop
After=network-online.target
Wants=network-online.target

[Service]
User=$SVC_USER
Group=$SVC_USER
EnvironmentFile=$ENV_FILE
ExecStart=$BIN serve
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=$DATA

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
}

listen_addr() { env_get LISTEN; }
health_url() { local l; l="$(listen_addr)"; l="${l:-:8080}"; [ "${l%%:*}" = "" ] && l="127.0.0.1$l"; echo "http://$l/healthz"; }

wait_healthy() {
  for _ in $(seq 1 20); do
    curl -fsS --max-time 2 "$(health_url)" >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}

as_service() { # run the binary as the service user with the service environment
  # shellcheck source=/dev/null
  ( set -a; . "$ENV_FILE"; set +a; exec setpriv --reuid="$SVC_USER" --regid="$SVC_USER" --init-groups "$BIN" "$@" )
}

install_caddy() {
  command -v caddy >/dev/null 2>&1 && return 0
  step "安装 Caddy（自动申请 HTTPS 证书）"
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq caddy >/dev/null || true
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y -q caddy >/dev/null || true
  fi
  command -v caddy >/dev/null 2>&1 || die "无法自动安装 Caddy：请先手动安装（https://caddyserver.com/docs/install），或加 --no-caddy 自备反向代理"
  systemctl enable --now caddy >/dev/null 2>&1 || true
}

configure_caddy() {
  local f=/etc/caddy/Caddyfile
  mkdir -p /etc/caddy
  if [ -f "$f" ] && grep -q "^${DOMAIN}[[:space:]]*{" "$f"; then
    ok "Caddyfile 中已有 $DOMAIN，保持不变"
  else
    # the packaged default only serves a placeholder page on :80: replace it
    if [ -f "$f" ] && grep -q 'The Caddyfile is an easy way to practice' "$f"; then : > "$f"; fi
    printf '\n%s {\n\tencode gzip\n\treverse_proxy 127.0.0.1:%s\n}\n' "$DOMAIN" "$PORT" >> "$f"
  fi
  systemctl reload caddy 2>/dev/null || systemctl restart caddy
  ok "Caddy 已配置 https://$DOMAIN"
}

open_firewall() {
  local ports=("$@") p
  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
    for p in "${ports[@]}"; do ufw allow "$p/tcp" >/dev/null; done
  elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
    for p in "${ports[@]}"; do firewall-cmd -q --permanent --add-port="$p/tcp"; done
    firewall-cmd -q --reload
  fi
}

public_url() {
  local d; d="$(env_get KAKU_DOMAIN)"
  if [ -n "$d" ]; then echo "https://$d"; else echo "http://$(curl -fsS --max-time 5 https://api.ipify.org 2>/dev/null || hostname -I | awk '{print $1}'):$(listen_addr | sed 's/.*://')"; fi
}

# install_ctl installs this script, from the release being installed, as
# kaku-ctl (when run through `bash <(curl …)` there is no file to copy).
install_ctl() {
  local t; t="$(mktemp)"
  if download install.sh "$t" >/dev/null 2>&1 && head -1 "$t" | grep -q bash; then
    install -m 0755 "$t" "$CTL"
  elif [ -f "$0" ]; then
    install -m 0755 "$0" "$CTL"
  fi
  rm -f "$t"
}

# --- commands ---

do_install() {
  need_root
  command -v systemctl >/dev/null 2>&1 || die "需要 systemd"
  command -v curl >/dev/null 2>&1 || die "需要 curl"
  installed && die "已经安装：升级用 update，重装请先 uninstall"
  [ -z "$DOMAIN" ] || [[ "$DOMAIN" =~ ^[A-Za-z0-9.-]+\.[A-Za-z]{2,}$ ]] || die "域名格式不正确：$DOMAIN"
  [[ "$PORT" =~ ^[0-9]+$ ]] && [ "$PORT" -ge 1 ] && [ "$PORT" -le 65535 ] || die "端口不正确：$PORT"

  fetch_release
  step "安装"
  id "$SVC_USER" >/dev/null 2>&1 || useradd --system --home-dir "$DATA" --shell /usr/sbin/nologin "$SVC_USER"
  install -d -m 0700 -o "$SVC_USER" -g "$SVC_USER" "$DATA"
  install -d -m 0700 "$ETC"
  install -m 0755 "$TMP/kaku" "$BIN"

  local pass="" key listen proxy=0 admin_path
  if [ -f "$ENV_FILE" ]; then
    ok "沿用已有配置 $ENV_FILE（主密钥和管理员不变）"
  else
    key="$("$BIN" keygen)"
    [ -n "$key" ] || die "生成主密钥失败"
    pass="$(rand 20)"
    admin_path="/admin-$(rand_lower 8)"
    # behind a reverse proxy (Caddy or your own) only listen locally
    if [ -n "$DOMAIN" ]; then listen="127.0.0.1:$PORT"; proxy=1; else listen=":$PORT"; fi
    umask 077
    cat > "$ENV_FILE" <<EOF
# Kaku shop. KAKU_MASTER_KEY encrypts every stored card and payment secret:
# back up this file offline. Without it the stored cards cannot be decrypted.
KAKU_MASTER_KEY=$key
ADMIN_USER=admin
ADMIN_PASSWORD=$pass
ADMIN_PATH=$admin_path
DATA_DIR=$DATA
LISTEN=$listen
TRUST_PROXY=$proxy
SECURE_COOKIE=$proxy
KAKU_DOMAIN=$DOMAIN
EOF
    chmod 600 "$ENV_FILE"
    ok "已生成主密钥和管理员密码"
  fi
  write_unit
  systemctl enable --now kaku >/dev/null 2>&1
  wait_healthy || die "服务未能启动：journalctl -u kaku -n 50"
  ok "服务已启动"

  if [ -n "$DOMAIN" ] && [ "$CADDY" = 1 ]; then
    install_caddy
    configure_caddy
    open_firewall 80 443
  elif [ -z "$DOMAIN" ]; then
    open_firewall "$PORT"
  fi
  install_ctl
  print_info "$pass"
}

print_info() {
  local pass="${1:-}" url
  url="$(public_url)"
  echo
  printf '%s安装完成：kaku %s%s\n\n' "$C_B" "$("$BIN" version | awk '{print $2}')" "$C_0"
  printf '  商店地址    %s\n' "$url"
  printf '  后台地址    %s%s\n' "$url" "$(env_get ADMIN_PATH)"
  printf '  管理员      %s\n' "$(env_get ADMIN_USER)"
  if [ -n "$pass" ]; then printf '  后台密码    %s%s%s（只显示这一次；忘记了执行 kaku-ctl reset-password）\n' "$C_B" "$pass" "$C_0"; fi
  printf '  配置文件    %s（含主密钥，请离线备份）\n' "$ENV_FILE"
  printf '  数据目录    %s\n\n' "$DATA"
  printf '下一步：登录后台 → 设置，填写站点地址（%s）、支付和邮件；对接 Polaris 时在 polaris-license 设置\n' "$url"
  printf '  KAKU_HOOK_URL=%s/hooks/polaris\n  KAKU_RENEW_URL=%s/renew?lid={license_id}\n\n' "$url" "$url"
  [ -z "$(env_get KAKU_DOMAIN)" ] && warn "没有使用域名：商店和后台走 HTTP 明文，支付回调和 Telegram 也需要 HTTPS，正式使用请加 --domain"
  printf '管理：kaku-ctl status | log | update | backup | info | reset-password\n'
}

do_update() {
  need_root; need_installed
  local cur; cur="$("$BIN" version | awk '{print $2}')"
  fetch_release
  if [ "$cur" = "$VERSION" ]; then ok "已是最新版本 $cur"; return 0; fi
  step "升级 $cur → $VERSION"
  cp -p "$BIN" "$BIN.bak"
  install -m 0755 "$TMP/kaku" "$BIN"
  systemctl restart kaku
  if wait_healthy; then
    rm -f "$BIN.bak"
    install_ctl
    ok "已升级到 $VERSION"
  else
    warn "新版本未能启动，回滚到 $cur"
    mv -f "$BIN.bak" "$BIN"
    systemctl restart kaku
    die "升级失败（已回滚）：journalctl -u kaku -n 50"
  fi
}

do_backup() {
  need_root; need_installed
  local ts dir out driver
  driver="$(env_get DB_DRIVER)"
  ts="$(date +%Y%m%d-%H%M%S)"
  dir="$(mktemp -d)"
  chown "$SVC_USER" "$dir"
  if [ -z "$driver" ] || [ "$driver" = sqlite ]; then
    as_service backup "$dir/kaku.db" >/dev/null
  else
    warn "数据库是 $driver：只备份配置，数据库请用其自带工具（如 pg_dump）"
  fi
  cp -p "$ENV_FILE" "$dir/"
  out="/root/kaku-backup-$ts.tar.gz"
  (umask 077; tar -C "$dir" -czf "$out" .)
  rm -rf "$dir"
  ok "已备份到 $out（含主密钥，请妥善保管）"
}

do_reset_password() {
  need_root; need_installed
  local pass; pass="$(rand 20)"
  sed -i "s/^ADMIN_PASSWORD=.*/ADMIN_PASSWORD=$pass/" "$ENV_FILE"
  as_service reset-admin >/dev/null || die "重置失败"
  ok "新后台密码：$pass（两步验证已关闭，已登录的会话全部失效）"
}

do_uninstall() {
  need_root
  local domain; domain="$(env_get KAKU_DOMAIN 2>/dev/null || true)"
  systemctl disable --now kaku >/dev/null 2>&1 || true
  rm -f "$UNIT" "$BIN" "$BIN.bak"
  systemctl daemon-reload
  if [ "$PURGE" = 1 ]; then
    rm -rf "$ETC" "$DATA" "$CTL"
    userdel "$SVC_USER" >/dev/null 2>&1 || true
    ok "已卸载并删除配置、主密钥和数据"
  else
    ok "已卸载；配置（含主密钥）保留在 $ETC，数据保留在 $DATA（重新 install 会沿用）"
  fi
  [ -n "$domain" ] && warn "Caddyfile 中的 $domain 站点配置未删除，如不再需要请手动移除"
  return 0
}

RUN_ARGS=()
while [ $# -gt 0 ]; do
  case "$1" in
    run) ACTION=run; shift; RUN_ARGS=("$@"); break ;;
    install | update | status | log | restart | stop | start | info | backup | reset-password | uninstall) ACTION="$1"; shift ;;
    --domain) DOMAIN="${2:-}"; shift 2 ;;
    --port) PORT="${2:-}"; shift 2 ;;
    --version) VERSION="${2:-}"; shift 2 ;;
    --no-caddy) CADDY=0; shift ;;
    --purge) PURGE=1; shift ;;
    -h | --help | help) usage; exit 0 ;;
    *) die "未知参数：$1（--help 查看用法）" ;;
  esac
done

case "$ACTION" in
  install) do_install ;;
  update) do_update ;;
  status) need_installed; systemctl status kaku --no-pager -l ;;
  log) need_installed; journalctl -u kaku -f -n 100 ;;
  restart | stop | start)
    need_root; need_installed; systemctl "$ACTION" kaku
    case "$ACTION" in restart) ok "已重启" ;; stop) ok "已停止" ;; start) ok "已启动" ;; esac ;;
  info) need_root; need_installed; print_info ;;
  backup) do_backup ;;
  reset-password) do_reset_password ;;
  run) need_root; need_installed; [ ${#RUN_ARGS[@]} -gt 0 ] || die "用法：kaku-ctl run <命令> [参数]"; as_service "${RUN_ARGS[@]}" ;;
  uninstall) do_uninstall ;;
  *) usage; exit 1 ;;
esac
