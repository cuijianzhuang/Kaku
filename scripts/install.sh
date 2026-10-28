#!/usr/bin/env bash
# Kaku shop: one-step install and management (Linux + systemd).
#
#   bash <(curl -fsSL https://github.com/cuijianzhuang/Kaku/releases/latest/download/install.sh) install --domain shop.example.com
#
# Commands (after installing, this script is available as `kaku-ctl`):
#   install [--domain NAME] [--port N] [--version TAG] [--no-caddy]
#                         install the latest (or given) release; with --domain, Caddy serves HTTPS.
#                         Default port 8090, or the next free one when it is taken
#   port N                move the service to port N (updates Caddy and the firewall)
#   update [--version TAG]  upgrade the binary (data and keys kept; rolls back if it does not start);
#                         also enables online upgrades from the admin backend
#   status | log | restart | stop | start
#   info                  show the shop and admin URLs and where the configuration is
#   backup                database + configuration (includes the master key) + uploaded images -> /root
#   reset-password        new random admin password (two-step verification is turned off)
#   run ARGS…             run kaku as the service, e.g. run version
#   uninstall [--purge]   remove; --purge also deletes the configuration, the master key and the data
#
# Downloads come from GitHub Releases. Where github.com is slow, set GH_PROXY
# to a download proxy prefix, e.g. GH_PROXY=https://ghproxy.example.com/
# (remembered in the configuration and used for online upgrades too).
#
# Online upgrades: the shop runs unprivileged and only writes a request file
# ($DATA/update-request). The root-owned kaku-update.path unit then runs
# `kaku-ctl apply-request`, which upgrades as `update` does and writes the
# result to $UPD_DIR (readable, not writable, by the shop).
set -euo pipefail

REPO="cuijianzhuang/Kaku"
BIN=/usr/local/bin/kaku
CTL=/usr/local/bin/kaku-ctl
ETC=/etc/kaku
ENV_FILE=$ETC/kaku.env
DATA=/var/lib/kaku
UNIT=/etc/systemd/system/kaku.service
SVC_USER=kaku
UPD_DIR=/var/lib/kaku-updater
UPD_PATH=/etc/systemd/system/kaku-update.path
UPD_UNIT=/etc/systemd/system/kaku-update.service
GH_PROXY="${GH_PROXY:-}"

DEFAULT_PORT=8090
ACTION="" DOMAIN="" PORT="" VERSION="" CADDY=1 PURGE=0 NEW_PORT=""

if [ -t 1 ]; then C_R=$'\e[31m' C_G=$'\e[32m' C_Y=$'\e[33m' C_B=$'\e[1m' C_0=$'\e[0m'; else C_R="" C_G="" C_Y="" C_B="" C_0=""; fi
ok() { printf '  %s✓%s %s\n' "$C_G" "$C_0" "$*"; }
warn() { printf '  %s!%s %s\n' "$C_Y" "$C_0" "$*"; }
step() { printf '%s==>%s %s\n' "$C_B" "$C_0" "$*"; }
die() { printf '  %s✗ %s%s\n' "$C_R" "$*" "$C_0" >&2; exit 1; }
usage() { sed -n '2,22p' "$0" 2>/dev/null || echo "用法：install | update | port | status | log | restart | stop | start | info | backup | reset-password | uninstall"; }

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

# write_updater installs the units that run upgrades requested from the
# admin backend.
write_updater() {
  install -d -m 0755 "$UPD_DIR"
  touch "$UPD_DIR/enabled"
  cat > "$UPD_PATH" <<EOF
[Unit]
Description=Kaku online upgrade requests

[Path]
PathExists=$DATA/update-request

[Install]
WantedBy=multi-user.target
EOF
  cat > "$UPD_UNIT" <<EOF
[Unit]
Description=Kaku online upgrade

[Service]
Type=oneshot
ExecStart=$CTL apply-request
TimeoutStartSec=15min
EOF
  systemctl daemon-reload
  systemctl enable --now kaku-update.path >/dev/null 2>&1 || warn "未能启用在线升级服务（kaku-update.path）"
}

# remember_proxy stores GH_PROXY in the configuration for later upgrades.
remember_proxy() {
  [ -n "$GH_PROXY" ] || return 0
  [[ "$GH_PROXY" =~ ^https?://[A-Za-z0-9._:/-]+$ ]] || die "GH_PROXY 格式不正确：$GH_PROXY"
  if grep -q '^GH_PROXY=' "$ENV_FILE"; then
    sed -i "s|^GH_PROXY=.*|GH_PROXY=$GH_PROXY|" "$ENV_FILE"
  else
    echo "GH_PROXY=$GH_PROXY" >> "$ENV_FILE"
  fi
}

listen_addr() { env_get LISTEN; }
health_url() { local l; l="$(listen_addr)"; l="${l:-:$DEFAULT_PORT}"; [ "${l%%:*}" = "" ] && l="127.0.0.1$l"; echo "http://$l/healthz"; }

wait_healthy() {
  for _ in $(seq 1 20); do
    curl -fsS --max-time 2 "$(health_url)" >/dev/null 2>&1 && return 0
    sleep 1
  done
  return 1
}

valid_port() { [[ "$1" =~ ^[0-9]+$ ]] && [ "$1" -ge 1 ] && [ "$1" -le 65535 ]; }

# port_in_use N: something already listens on TCP port N.
port_in_use() {
  if command -v ss >/dev/null 2>&1; then
    ss -Hltn 2>/dev/null | awk '{print $4}' | grep -Eq "[:.]$1\$"
  elif command -v lsof >/dev/null 2>&1; then
    lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
  else
    (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
  fi
}

# pick_port sets PORT: the one given (which must be free), else the
# default or the next free port after it.
pick_port() {
  local p
  if [ -n "$PORT" ]; then
    valid_port "$PORT" || die "端口不正确：$PORT"
    port_in_use "$PORT" && die "端口 $PORT 已被占用（ss -ltnp 查看占用的程序），换一个：--port N"
    return 0
  fi
  for p in $(seq "$DEFAULT_PORT" $((DEFAULT_PORT + 50))); do
    if ! port_in_use "$p"; then
      PORT="$p"
      [ "$p" = "$DEFAULT_PORT" ] || warn "默认端口 $DEFAULT_PORT 已被占用，改用 $p"
      return 0
    fi
  done
  die "$DEFAULT_PORT–$((DEFAULT_PORT + 50)) 都被占用，请用 --port 指定一个空闲端口"
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
  # replace by rename: a running kaku-ctl keeps reading its old copy
  if download install.sh "$t" >/dev/null 2>&1 && head -1 "$t" | grep -q bash; then
    install -m 0755 "$t" "$CTL.new" && mv -f "$CTL.new" "$CTL"
  elif [ -f "$0" ] && [ "$0" != "$CTL" ]; then
    install -m 0755 "$0" "$CTL.new" && mv -f "$CTL.new" "$CTL"
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
  pick_port

  fetch_release
  step "安装"
  id "$SVC_USER" >/dev/null 2>&1 || useradd --system --home-dir "$DATA" --shell /usr/sbin/nologin "$SVC_USER"
  install -d -m 0700 -o "$SVC_USER" -g "$SVC_USER" "$DATA"
  install -d -m 0700 "$ETC"
  install -m 0755 "$TMP/kaku" "$BIN"

  local pass="" key listen proxy=0 admin_path
  if [ -f "$ENV_FILE" ]; then
    ok "沿用已有配置 $ENV_FILE（主密钥和管理员不变）"
    PORT="$(listen_addr | sed 's/.*://')"; PORT="${PORT:-$DEFAULT_PORT}"
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
  remember_proxy
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
  write_updater
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
  printf '管理：kaku-ctl status | log | update | port | backup | info | reset-password\n'
}

do_update() {
  need_root; need_installed
  local cur; cur="$("$BIN" version | awk '{print $2}')"
  remember_proxy
  [ -f "$UPD_PATH" ] || { write_updater; ok "已启用后台在线升级"; }
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

write_status() { # STATE FROM TO
  local t="$UPD_DIR/status.tmp"
  printf 'state=%s\nfrom=%s\nto=%s\ntime=%s\n' "$1" "$2" "$3" "$(date +%s)" > "$t"
  chmod 0644 "$t"
  mv -f "$t" "$UPD_DIR/status"
}

# do_apply_request runs an upgrade requested from the admin backend
# (started as root by kaku-update.path). The request only names a version;
# where to download from and how to install come from this script.
do_apply_request() {
  need_root; need_installed
  local req="$DATA/update-request" tag="" cur now log="$UPD_DIR/last.log"
  install -d -m 0755 "$UPD_DIR"
  if [ -f "$req" ] && [ ! -L "$req" ]; then
    tag="$(timeout 5 head -c 64 -- "$req" 2>/dev/null | tr -d '[:space:]')" || true
  fi
  rm -f -- "$req"
  cur="$("$BIN" version | awk '{print $2}')"
  if ! [[ "$tag" =~ ^v[0-9][0-9A-Za-z.+-]{0,39}$ ]]; then
    echo "无效的升级请求" > "$log"; chmod 0644 "$log"
    write_status failed "$cur" ""
    die "无效的升级请求"
  fi
  write_status running "$cur" "$tag"
  VERSION="$tag"
  ( do_update ) > "$log" 2>&1 || true
  chmod 0644 "$log"
  now="$("$BIN" version | awk '{print $2}')"
  if [ "$cur" = "$tag" ]; then
    write_status uptodate "$cur" "$tag"
  elif [ "$now" = "$tag" ]; then
    write_status ok "$cur" "$tag"
  else
    write_status failed "$cur" "$tag"
    exit 1
  fi
}

# do_port moves the service to another port: the configuration, the
# Caddy site (when installed with --domain) and the firewall follow.
do_port() {
  need_root; need_installed
  local old host listen domain f=/etc/caddy/Caddyfile
  valid_port "$NEW_PORT" || die "用法：kaku-ctl port 端口号（1–65535）"
  listen="$(listen_addr)"; old="${listen##*:}"; host="${listen%:*}"
  [ "$old" = "$NEW_PORT" ] && { ok "已经是端口 $NEW_PORT"; return 0; }
  port_in_use "$NEW_PORT" && die "端口 $NEW_PORT 已被占用（ss -ltnp 查看占用的程序）"
  step "端口 $old → $NEW_PORT"
  sed -i "s/^LISTEN=.*/LISTEN=$host:$NEW_PORT/" "$ENV_FILE"
  systemctl restart kaku
  wait_healthy || {
    sed -i "s/^LISTEN=.*/LISTEN=$listen/" "$ENV_FILE"; systemctl restart kaku
    die "新端口上未能启动，已改回 $old：journalctl -u kaku -n 50"
  }
  domain="$(env_get KAKU_DOMAIN)"
  if [ -n "$domain" ] && [ -f "$f" ] && grep -q "reverse_proxy 127.0.0.1:$old\b" "$f"; then
    sed -i "s/reverse_proxy 127\.0\.0\.1:$old\b/reverse_proxy 127.0.0.1:$NEW_PORT/" "$f"
    systemctl reload caddy 2>/dev/null || systemctl restart caddy
    ok "Caddy 已改为转发到 127.0.0.1:$NEW_PORT"
  elif [ -n "$domain" ]; then
    warn "请把反向代理（$domain）改为转发到 127.0.0.1:$NEW_PORT"
  fi
  [ -z "$host" ] && open_firewall "$NEW_PORT"
  ok "已改为端口 $NEW_PORT"
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
  [ -d "$DATA/uploads" ] && cp -rp "$DATA/uploads" "$dir/"
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
  systemctl disable --now kaku kaku-update.path >/dev/null 2>&1 || true
  rm -f "$UNIT" "$UPD_PATH" "$UPD_UNIT" "$BIN" "$BIN.bak"
  rm -rf "$UPD_DIR"
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

[ -z "$GH_PROXY" ] && [ -f "$ENV_FILE" ] && GH_PROXY="$(env_get GH_PROXY)"

RUN_ARGS=()
while [ $# -gt 0 ]; do
  case "$1" in
    run) ACTION=run; shift; RUN_ARGS=("$@"); break ;;
    apply-request | install | update | status | log | restart | stop | start | info | backup | reset-password | uninstall) ACTION="$1"; shift ;;
    port) ACTION=port; NEW_PORT="${2:-}"; shift; [ $# -gt 0 ] && shift ;;
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
  apply-request) do_apply_request ;;
  port) do_port ;;
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
