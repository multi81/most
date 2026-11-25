#!/usr/bin/env bash
# server-setup.sh — подготовка нового сервера для «Моста» (запускает мастер программы), 2026-11-15, Вера.
# Только Ubuntu 24.04 / 26.04, x86_64. Ставит: защиту и тюнинг, AmneziaWG (UDP), свой DNS, Xray (Reality, выход в интернет),
# службу управления устройствами most-admin, по желанию — рабочий стол KDE + xrdp с автовходом.
# Запуск (от root):  MOST_OWNER_PUB=<откр. ключ AWG устройства> MOST_AWG_PORT=443 MOST_DESKTOP=1 bash server-setup.sh < пароль_RDP
# Ход работы: /var/log/most-setup.log (строки «##STEP n/N …», «##ERR …», «##DONE»); итог (секреты) — /root/most-setup-result.json (600).
# Повторный запуск безопасен: уже сделанное не ломается (ключи и конфиги, если есть, сохраняются).
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive LC_ALL=C.UTF-8
# под systemd-run нет HOME — Go без него не знает, куда класть библиотеки и кэш сборки
export HOME=/root GOPATH=/root/go GOCACHE=/root/.cache/go-build GOTOOLCHAIN=go1.27.0 GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=advice.detachedHead GIT_CONFIG_VALUE_0=false
LOG=/var/log/most-setup.log
SRC=${MOST_SRC:-/root/most-setup}   # рядом лежат most-admin.go
OWNER_PUB=${MOST_OWNER_PUB:?нужен открытый ключ AWG устройства}
OWNER_SSH=${MOST_OWNER_SSH:-}   # открытый ключ SSH первого устройства (канал SSH до рабочего стола)
AWG_PORT=${MOST_AWG_PORT:-443}
DESKTOP=${MOST_DESKTOP:-1}
SNI=${MOST_SNI:-www.samsung.com}
DOMAIN=${MOST_DOMAIN:-}   # по желанию: домен в Cloudflare → канал «Xray через CDN» (VLESS+WS+TLS на 8443)
RDOM=${MOST_REALITY_DOMAIN:-} # по желанию (рекомендуется): свой домен (A → этот сервер, без облака) — Reality маскируется под свой сайт
[ -n "$RDOM" ] && SNI=$RDOM
XRAY_REF=a4a3dff5b20c2faae567b742c172c7b95e75c083   # Xray-core v26.10.31
AWG_GO_TAG=v3.1.20260828; AWG_TOOLS_TAG=v3.1.20260812
B=/root/claude-changes/most-setup-$(date +%Y%m%d-%H%M%S); install -d -m700 $B /root/build
PW=""
if [ "$DESKTOP" = 1 ]; then # пароль пользователя рабочего стола: из файла (мастер «Моста», файл сразу стирается) или со stdin
  if [ -n "${MOST_PWFILE:-}" ]; then PW=$(head -1 "$MOST_PWFILE"); shred -u "$MOST_PWFILE"; else read -r PW; fi
fi
if [ -s /etc/most-admin/config.json ]; then
  OLDPW=$(python3 -c 'import json;print(json.load(open("/etc/most-admin/config.json")).get("rdp_password",""))' 2>/dev/null || true)
  if [ -n "$OLDPW" ]; then PW=$OLDPW; fi
fi
TOTAL=$((8 + DESKTOP + $([ -n "$DOMAIN" ] && echo 1 || echo 0) + $([ -n "$RDOM" ] && echo 1 || echo 0))); N=0
step() { N=$((N+1)); echo "##STEP $N/$TOTAL $*"; }
trap 'echo "##ERR строка $LINENO: команда завершилась с ошибкой (подробности выше)"' ERR
exec >>"$LOG" 2>&1
echo "=== $(date -Is) начало подготовки"
# --- устойчивость (17.11): на свежем сервере apt занят автообновлениями — ждать до 10 мин; сеть — повторы;
# службы перезапускать только при изменениях (повторный запуск не должен рвать рабочий стол и каналы)
apt-get() { command apt-get -o DPkg::Lock::Timeout=600 "$@"; }
export GIT_HTTP_LOW_SPEED_LIMIT=1000 GIT_HTTP_LOW_SPEED_TIME=60   # зависшая загрузка git прерывается и повторяется
retry() { local n=0; until "$@"; do n=$((n+1)); [ $n -ge 3 ] && return 1; echo "сбой, повтор $n: $*"; sleep 15; done; }
apt_up() { retry apt-get update -q; }
apt_i() { retry apt-get install -y -q "$@"; }
installed() { dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -qx 'install ok installed'; }   # dpkg -s «видит» и удалённый с настройками
sum_of() { { cat "$@" 2>/dev/null || true; } | sha256sum | cut -d' ' -f1; }   # файлов может ещё не быть (pipefail)
# restart_if <служба> <сумма до> <файлы…>: перезапуск, если файлы изменились или служба не работает
restart_if() { local svc=$1 pre=$2; shift 2
  if [ "$pre" != "$(sum_of "$@")" ] || ! systemctl is-active -q "$svc"; then systemctl restart "$svc"; else echo "$svc: без изменений — не перезапускаю"; fi; }

# ---------------------------------------------------------------- 1. проверка
step "Проверка системы"
. /etc/os-release
case "$VERSION_ID" in 24.04|26.04) ;; *) echo "##ERR поддерживаются только Ubuntu 24.04 и 26.04 (здесь $PRETTY_NAME)"; exit 2;; esac
[ "$(uname -m)" = x86_64 ] || { echo "##ERR нужна архитектура x86_64"; exit 2; }
IFACE=$(ip -o route show default | awk '{print $5; exit}')
SSH_PORT=$(ss -tlnp | awk '/sshd/ {split($4,a,":"); print a[length(a)]; exit}'); SSH_PORT=${SSH_PORT:-22}
echo "##INFO os=$PRETTY_NAME iface=$IFACE ssh_port=$SSH_PORT"
for p in $AWG_PORT/udp 443/tcp 8443/tcp; do
  n=${p%/*}; proto=${p#*/}
  if [ "$proto" = udp ] && ss -ulnp | grep -q ":$n " && ! ss -ulnp | grep ":$n " | grep -q amneziawg; then echo "##ERR порт $p уже занят другой программой"; exit 3; fi
  if [ "$proto" = tcp ] && ss -tlnp | grep -q ":$n " && ! ss -tlnp | grep ":$n " | grep -q xray; then echo "##ERR порт $p уже занят другой программой"; exit 3; fi
done

# ---------------------------------------------------------------- 2. база
step "Обновление и защита (ufw, fail2ban, автообновления, тюнинг сети)"
apt_up
apt_i ufw fail2ban unattended-upgrades curl git golang-go build-essential pkg-config unbound python3 iptables >/dev/null
cat > /etc/sysctl.d/90-most.conf <<'EOF'
# «Мост»: сеть как на узлах VPN
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr
net.core.rmem_max = 67108864
net.core.wmem_max = 67108864
net.ipv4.tcp_rmem = 4096 87380 67108864
net.ipv4.tcp_wmem = 4096 65536 67108864
net.ipv4.tcp_keepalive_time = 600
net.netfilter.nf_conntrack_max = 262144
net.ipv4.ip_forward = 1
EOF
modprobe nf_conntrack 2>/dev/null || true
sysctl -q -p /etc/sysctl.d/90-most.conf || true
if [ "$(swapon --show | wc -l)" = 0 ] && [ "$(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo)" -lt 8000 ]; then
  fallocate -l 4G /swapfile && chmod 600 /swapfile && mkswap -q /swapfile && swapon /swapfile && echo '/swapfile none swap sw 0 0' >> /etc/fstab
fi
printf '[sshd]\nenabled = true\nport = %s\nmaxretry = 6\nbantime = 1h\nignoreip = 127.0.0.1/8 10.77.0.0/24\n' "$SSH_PORT" > /etc/fail2ban/jail.d/most.local
systemctl enable --now fail2ban unattended-upgrades >/dev/null 2>&1 || true
systemctl restart fail2ban || true
ufw allow "$SSH_PORT"/tcp comment 'SSH' >/dev/null
ufw default deny incoming >/dev/null; ufw default allow outgoing >/dev/null
ufw --force enable >/dev/null

# ---------------------------------------------------------------- 3. AmneziaWG
step "AmneziaWG: сборка и туннель (UDP $AWG_PORT)"
if ! command -v amneziawg-go >/dev/null || ! command -v awg >/dev/null; then
  cd /root/build && rm -rf amneziawg-go amneziawg-tools
  retry bash -c "rm -rf amneziawg-go; git clone -q --depth 1 --branch $AWG_GO_TAG https://github.com/amnezia-vpn/amneziawg-go.git"
  retry bash -c "rm -rf amneziawg-tools; git clone -q --depth 1 --branch $AWG_TOOLS_TAG https://github.com/amnezia-vpn/amneziawg-tools.git"
  (cd amneziawg-go && retry make >/dev/null && install -m755 amneziawg-go /usr/bin/amneziawg-go)
  (cd amneziawg-tools/src && make >/dev/null && make install WITH_WGQUICK=yes WITH_SYSTEMDUNITS=yes >/dev/null)
fi
D=/etc/amnezia/amneziawg; install -d -m700 $D
DROPIN=/etc/systemd/system/awg-quick@awg0.service.d/userspace.conf
AWG_PRE=$(sum_of $D/awg0.conf $DROPIN); DROP_PRE=$(sum_of $DROPIN); NEWPEER=""
if [ ! -s $D/awg0.conf ]; then
  AWG_PRE=new
  r() { shuf -i "$1-$2" -n1; }
  H=$(shuf -i 100000-2147483000 -n4 | tr '\n' ' '); set -- $H
  SK=$(awg genkey)
  {
    printf '[Interface]\nAddress = 10.77.0.1/24\nMTU = 1280\nListenPort = %s\nPrivateKey = %s\n' "$AWG_PORT" "$SK"
    printf 'Jc = %s\nJmin = 40\nJmax = 70\nS1 = %s\nS2 = %s\nH1 = %s\nH2 = %s\nH3 = %s\nH4 = %s\n' "$(r 4 8)" "$(r 15 150)" "$(r 15 150)" "$1" "$2" "$3" "$4"
  } > $D/awg0.conf
  chmod 600 $D/awg0.conf
fi
if ! grep -q "PublicKey = $OWNER_PUB" $D/awg0.conf; then
  PSK=$(awg genpsk); OWNER_PSK_NEW=$PSK; used=$( (grep -o 'AllowedIPs = 10.77.0.[0-9]*' $D/awg0.conf || true) | awk -F. '{print $4}')  # на чистом сервере устройств ещё нет
  for i in $(seq 5 250); do echo "$used" | grep -qx "$i" || { OWNER_IP=10.77.0.$i; break; }; done
  printf '\n# владелец\n[Peer]\nPublicKey = %s\nPresharedKey = %s\nAllowedIPs = %s/32\n' "$OWNER_PUB" "$PSK" "$OWNER_IP" >> $D/awg0.conf
  NEWPEER=1
fi
OWNER_IP=$(awk -v k="$OWNER_PUB" '$0=="PublicKey = "k{f=1} f&&/AllowedIPs/{split($3,a,"/"); print a[1]; exit}' $D/awg0.conf)
OWNER_PSK=$(awk -v k="$OWNER_PUB" '$0=="PublicKey = "k{f=1} f&&/PresharedKey/{print $3; exit}' $D/awg0.conf)
install -d /etc/systemd/system/awg-quick@awg0.service.d
printf '[Service]\nEnvironment=WG_QUICK_USERSPACE_IMPLEMENTATION=amneziawg-go\nRestart=on-failure\nRestartSec=15\n' > $DROPIN
systemctl daemon-reload; systemctl enable awg-quick@awg0 >/dev/null 2>&1
# туннель работает, настройки службы прежние, добавился только новый владелец — добавить его на ходу
# (перезапуск порвал бы подключённые устройства); иначе — перезапуск, если что-то изменилось или туннель не работает
if [ "$AWG_PRE" != new ] && [ -n "$NEWPEER" ] && [ "$DROP_PRE" = "$(sum_of $DROPIN)" ] && systemctl is-active -q awg-quick@awg0; then
  awg set awg0 peer "$OWNER_PUB" preshared-key <(printf '%s\n' "$OWNER_PSK_NEW") allowed-ips "$OWNER_IP/32"
  echo "AWG: новый владелец добавлен без перезапуска"
else
  restart_if awg-quick@awg0 "$AWG_PRE" $D/awg0.conf $DROPIN
fi
ufw allow "$AWG_PORT"/udp comment 'Мост: AWG' >/dev/null
ufw allow in on awg0 to any port "$SSH_PORT" proto tcp comment 'Мост: SSH из туннеля' >/dev/null
ufw route allow in on awg0 out on awg0 comment 'Мост: устройства между собой' >/dev/null
ufw route allow in on awg0 out on "$IFACE" comment 'Мост: выход в интернет' >/dev/null
if ! grep -q 'Мост: NAT awg' /etc/ufw/before.rules; then
  cp -p /etc/ufw/before.rules $B/before.rules
  { printf '# Мост: NAT awg — выход устройств в интернет\n*nat\n:POSTROUTING ACCEPT [0:0]\n-A POSTROUTING -s 10.77.0.0/24 -o %s -j MASQUERADE\nCOMMIT\n\n' "$IFACE"; cat $B/before.rules; } > /etc/ufw/before.rules
fi
ufw reload >/dev/null
SRV_PUB=$(awg show awg0 public-key)
OBF=$(grep -E '^(Jc|Jmin|Jmax|S[1-4]|H[1-4]|I[1-5]) ' $D/awg0.conf)

# ---------------------------------------------------------------- 4. DNS
step "Свой DNS для туннеля (unbound)"
UNB_PRE=$(sum_of /etc/unbound/unbound.conf.d/most.conf /etc/systemd/system/unbound.service.d/after-awg.conf)
cat > /etc/unbound/unbound.conf.d/most.conf <<'EOF'
server:
  interface: 10.77.0.1
  access-control: 0.0.0.0/0 refuse
  access-control: 10.77.0.0/24 allow
  hide-identity: yes
  hide-version: yes
  qname-minimisation: yes
  prefetch: yes
EOF
install -d /etc/systemd/system/unbound.service.d
printf '[Unit]\nAfter=awg-quick@awg0.service\nWants=awg-quick@awg0.service\n[Service]\nRestart=on-failure\nRestartSec=5\n' > /etc/systemd/system/unbound.service.d/after-awg.conf
systemctl daemon-reload; unbound-checkconf >/dev/null; systemctl enable unbound >/dev/null 2>&1
restart_if unbound "$UNB_PRE" /etc/unbound/unbound.conf.d/most.conf /etc/systemd/system/unbound.service.d/after-awg.conf
ufw allow in on awg0 to any port 53 comment 'Мост: DNS' >/dev/null

# ---------------------------------------------------------------- 4б. свой сайт для маскировки Reality (по желанию)
# как на VPN-нодах: свой домен и настоящий сертификат; порт 80 → 301 на https (и проверка Let's Encrypt);
# заглушка только изнутри (127.0.0.1:8444): TLS 1.2 + 1.3, HSTS, без заголовка Server, своя страница.
if [ -n "$RDOM" ]; then
  step "Свой сайт $RDOM для маскировки Reality"
  apt_i --no-install-recommends nginx libnginx-mod-http-headers-more-filter certbot dnsutils >/dev/null
  MYIP=${MOST_PUBLIC_IP:-$(curl -s --max-time 10 https://api.ipify.org)}
  GOT=$(dig +short A "$RDOM" @1.1.1.1 | tr '\n' ' ')
  echo "##INFO домен $RDOM → $GOT (сервер $MYIP)"
  if ! echo " $GOT " | grep -q " $MYIP "; then
    echo "##ERR домен $RDOM указывает не на этот сервер ($GOT вместо $MYIP). Нужна запись A на $MYIP, в Cloudflare — серое облачко (только DNS): за оранжевым облаком маскировка не работает"
    exit 6
  fi
  rm -f /etc/nginx/sites-enabled/default /etc/nginx/conf.d/10-reality-site.conf  # заглушка пишется заново ниже (старая могла быть неисправной)
  sed -i 's/^\(\s*\)server_tokens build;.*/\1server_tokens off;/' /etc/nginx/nginx.conf
  install -d -m755 /var/www/acme "/var/www/$RDOM"
  NAME=$(echo "$RDOM" | cut -d. -f1); [ ${#NAME} -lt 4 ] && NAME=$(echo "$RDOM" | cut -d. -f2)  # «r.example.com» → Example
  NAME=$(echo "$NAME" | sed 's/.*/\u&/')
  if [ ! -s "/var/www/$RDOM/index.html" ]; then # своя страница у каждого сервера (имя из домена)
    cat > "/var/www/$RDOM/index.html" <<HTML
<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>$NAME — engineering services</title><meta name="description" content="$NAME: infrastructure and software engineering for small teams.">
<style>body{margin:0;font:16px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;color:#15171b;background:#fafafb}
header,main,footer{max-width:860px;margin:0 auto;padding:0 24px}header{height:68px;display:flex;align-items:center;border-bottom:1px solid #e7e9ed;font-weight:700}
h1{font-size:38px;line-height:1.15;letter-spacing:-.02em;margin:60px 0 14px}p{color:#5b6270;max-width:62ch}
.g{display:grid;grid-template-columns:repeat(auto-fit,minmax(230px,1fr));gap:18px;margin:44px 0}.c{background:#fff;border:1px solid #e7e9ed;border-radius:12px;padding:20px}
.c b{display:block;margin-bottom:4px}footer{border-top:1px solid #e7e9ed;padding:26px 24px;color:#7a808c;font-size:14px;margin-top:40px}</style></head>
<body><header>$NAME</header><main><h1>Reliable systems, quietly maintained.</h1>
<p>We design, run and document infrastructure for small product teams: hosting, monitoring, backups and release pipelines.</p>
<div class="g"><div class="c"><b>Infrastructure</b>Servers, networks and storage that stay boring.</div>
<div class="c"><b>Monitoring</b>Alerts that mean something, dashboards people read.</div>
<div class="c"><b>Release pipelines</b>Repeatable builds and safe rollouts.</div></div></main>
<footer>© $(date +%Y) $NAME</footer></body></html>
HTML
    printf 'User-agent: *\nAllow: /\n' > "/var/www/$RDOM/robots.txt"
  fi
  cat > /etc/nginx/conf.d/00-http.conf <<'EOF'
more_clear_headers Server;
server {
  listen 80 default_server;
  listen [::]:80 default_server;
  location /.well-known/acme-challenge/ { root /var/www/acme; }
  location / { return 301 https://$host$request_uri; }
}
EOF
  nginx -t -q
  systemctl enable --now nginx >/dev/null 2>&1
  systemctl reload nginx
  ufw allow 80/tcp comment 'Мост: 301 на https и проверка сертификатов' >/dev/null
  if [ ! -s "/etc/letsencrypt/live/$RDOM/fullchain.pem" ]; then
    if ! certbot certonly --webroot -w /var/www/acme -d "$RDOM" --non-interactive --agree-tos --register-unsafely-without-email; then
      echo "##ERR сертификат для $RDOM не выпущен: проверьте запись A ($MYIP, серое облачко) и что порт 80 не закрыт у хостера"
      exit 6
    fi
  fi
  cat > /etc/nginx/conf.d/10-reality-site.conf <<EOF
server {
  listen 127.0.0.1:8444 ssl http2;   # так понимают и nginx 1.24 (Ubuntu 24.04), и новые
  server_name $RDOM;
  ssl_certificate     /etc/letsencrypt/live/$RDOM/fullchain.pem;
  ssl_certificate_key /etc/letsencrypt/live/$RDOM/privkey.pem;
  ssl_protocols TLSv1.2 TLSv1.3;
  ssl_session_cache shared:most:10m;
  add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;
  add_header X-Content-Type-Options nosniff always;
  root /var/www/$RDOM;
  index index.html;
  location / { try_files \$uri \$uri/ =404; }
}
EOF
  nginx -t -q
  systemctl reload nginx
  sleep 1
  if [ "$(curl -sk -o /dev/null -w '%{http_code}' --resolve "$RDOM:8444:127.0.0.1" "https://$RDOM:8444/")" != 200 ]; then
    echo "##ERR сайт для маскировки не отвечает (nginx на 127.0.0.1:8444) — подробности выше"
    exit 6
  fi
  install -d /etc/letsencrypt/renewal-hooks/deploy
  printf '#!/bin/sh\nsystemctl reload nginx 2>/dev/null; systemctl restart most-xray 2>/dev/null; exit 0\n' > /etc/letsencrypt/renewal-hooks/deploy/most-reload
  chmod 755 /etc/letsencrypt/renewal-hooks/deploy/most-reload
fi

# ---------------------------------------------------------------- 4а. домен и сертификат (по желанию)
if [ -n "$DOMAIN" ]; then
  step "Домен $DOMAIN: проверка и сертификат Let's Encrypt"
  apt_i certbot dnsutils >/dev/null
  MYIP=${MOST_PUBLIC_IP:-$(curl -s --max-time 10 https://api.ipify.org)}
  GOT=$(dig +short A "$DOMAIN" @1.1.1.1 | tr '\n' ' ')
  echo "##INFO домен $DOMAIN → $GOT (сервер $MYIP)"
  if ! echo " $GOT " | grep -q " $MYIP "; then
    # за оранжевым облаком Cloudflare домен показывает адреса Cloudflare — тогда проверить, что HTTP доходит до нас
    echo "домен не указывает напрямую на сервер — проверяю, не за Cloudflare ли он"
  fi
  if [ ! -s "/etc/letsencrypt/live/$DOMAIN/fullchain.pem" ] && [ -n "$RDOM" ]; then
    # порт 80 уже у nginx (свой сайт) — проверка через webroot, без временного открытия порта
    if ! certbot certonly --webroot -w /var/www/acme -d "$DOMAIN" --non-interactive --agree-tos --register-unsafely-without-email; then
      echo "##ERR сертификат для $DOMAIN не выпущен: проверьте запись A и что в Cloudflare нет «Always Use HTTPS»"
      exit 5
    fi
  fi
  if [ ! -s "/etc/letsencrypt/live/$DOMAIN/fullchain.pem" ]; then
    ufw allow 80/tcp comment 'certbot (временно)' >/dev/null
    if ! certbot certonly --standalone -d "$DOMAIN" --non-interactive --agree-tos --register-unsafely-without-email --preferred-challenges http; then
      ufw delete allow 80/tcp >/dev/null 2>&1 || true
      echo "##ERR сертификат для $DOMAIN не выпущен: проверьте, что запись A домена указывает на $MYIP (в Cloudflare на время подготовки — серое облачко) и что порт 80 не закрыт у хостера"
      exit 5
    fi
    ufw delete allow 80/tcp >/dev/null 2>&1 || true
  fi
  install -d /etc/letsencrypt/renewal-hooks/pre /etc/letsencrypt/renewal-hooks/post /etc/letsencrypt/renewal-hooks/deploy
  if [ -z "$RDOM" ]; then # без своего сайта порт 80 открывается только на время продления
    printf '#!/bin/sh\nufw allow 80/tcp comment certbot-renew >/dev/null\n' > /etc/letsencrypt/renewal-hooks/pre/ufw80
    printf '#!/bin/sh\nufw delete allow 80/tcp >/dev/null 2>&1; true\n' > /etc/letsencrypt/renewal-hooks/post/ufw80
  else # порт 80 постоянно у nginx — продление через webroot
    rm -f /etc/letsencrypt/renewal-hooks/pre/ufw80 /etc/letsencrypt/renewal-hooks/post/ufw80
    RC="/etc/letsencrypt/renewal/$DOMAIN.conf"
    if [ -f "$RC" ]; then
      sed -i 's/^authenticator = standalone/authenticator = webroot/' "$RC"
      grep -q '^webroot_path' "$RC" || sed -i '/^\[renewalparams\]/a webroot_path = /var/www/acme,' "$RC"
    fi
  fi
  printf '#!/bin/sh\nsystemctl restart most-xray\n' > /etc/letsencrypt/renewal-hooks/deploy/most-xray
  chmod 755 /etc/letsencrypt/renewal-hooks/*/*
  install -d /etc/systemd/system/most-xray.service.d
  printf '[Service]\nLoadCredential=cert:/etc/letsencrypt/live/%s/fullchain.pem\nLoadCredential=key:/etc/letsencrypt/live/%s/privkey.pem\n' "$DOMAIN" "$DOMAIN" > /etc/systemd/system/most-xray.service.d/cert.conf
  ufw allow 8443/tcp comment 'Мост: Xray через CDN' >/dev/null
fi

# ---------------------------------------------------------------- 5. Xray
step "Xray: сборка и каналы (TCP 443 Reality)"
if ! /usr/local/bin/xray version 2>/dev/null | grep -q 26.10.31; then
  cd /root/build && retry bash -c "rm -rf xray-src; git clone -q --filter=blob:none https://github.com/XTLS/Xray-core.git xray-src"
  (cd xray-src && git checkout -q $XRAY_REF && retry env CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -buildid=" -o /usr/local/bin/xray ./main)
fi
X=/usr/local/etc/most-xray; install -d -m755 $X
XR_PRE=$(sum_of $X/config.json /etc/systemd/system/most-xray.service /etc/systemd/system/most-xray.service.d/cert.conf /usr/local/bin/xray)
if [ ! -s /root/most-xray-client.json ]; then
  KP=$(/usr/local/bin/xray x25519)
  PRIV=$(echo "$KP" | awk -F': *' '/Private/{print $2}'); PUB=$(echo "$KP" | awk -F': *' '/Public|Password/{print $2}' | head -1)
  U1=$(/usr/local/bin/xray uuid); U2=$(/usr/local/bin/xray uuid); SID=$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')
  printf '{"port":443,"uuid":"%s","public_key":"%s","short_id":"%s","sni":"%s","proxy_uuid":"%s","priv":"%s"}\n' "$U1" "$PUB" "$SID" "$SNI" "$U2" "$PRIV" > /root/most-xray-client.json
  chmod 600 /root/most-xray-client.json
fi
python3 - "$DOMAIN" "$RDOM" <<'PY'
import json,sys,os
domain=sys.argv[1]; rdom=sys.argv[2]
j=json.load(open("/root/most-xray-client.json"))
if rdom and j.get("sni")!=rdom:  # свой сайт для маскировки (в том числе при повторной подготовке)
    j["sni"]=rdom; open("/root/most-xray-client.json","w").write(json.dumps(j))
own=os.path.exists("/etc/nginx/conf.d/10-reality-site.conf")
if not domain and j.get("ws_host") and os.path.exists("/etc/letsencrypt/live/%s/fullchain.pem"%j["ws_host"]):
    domain=j["ws_host"]  # повторная подготовка без домена: уже настроенный канал CDN сохраняется
elif not domain:
    for k in ("ws_host","ws_port","ws_path"): j.pop(k,None)
    open("/root/most-xray-client.json","w").write(json.dumps(j))
if domain and j.get("ws_host")!=domain:  # канал через CDN: свой путь WebSocket
    j.update({"ws_host":domain,"ws_port":8443,"ws_path":"/"+os.urandom(9).hex()})
    open("/root/most-xray-client.json","w").write(json.dumps(j))
c={"log":{"loglevel":"warning"},
 "inbounds":[{"tag":"most-in","listen":"0.0.0.0","port":443,"protocol":"vless",
   "settings":{"clients":[{"id":j["uuid"],"flow":"xtls-rprx-vision","email":"owner"},{"id":j["proxy_uuid"],"flow":"xtls-rprx-vision","email":"proxy"}],"decryption":"none"},
   "streamSettings":{"network":"tcp","security":"reality","realitySettings":{"dest":("127.0.0.1:8444" if own else j["sni"]+":443"),"serverNames":[j["sni"]],"privateKey":j["priv"],"shortIds":[j["short_id"]]}},
   "sniffing":{"enabled":False}}],
 "outbounds":[{"tag":"direct","protocol":"freedom","settings":{"finalRules":[
     {"action":"allow","network":"tcp","ip":["127.0.0.1"],"port":"3389"},{"action":"allow","network":"tcp","ip":["127.0.0.1"],"port":"8787"}]}},
   {"tag":"egress","protocol":"freedom","settings":{"domainStrategy":"UseIPv4"}},{"tag":"block","protocol":"blackhole"}],
 "routing":{"domainStrategy":"AsIs","rules":[
   {"type":"field","ip":["127.0.0.1"],"port":"3389,8787","outboundTag":"direct"},
   {"type":"field","user":["proxy"],"outboundTag":"egress"},
   {"type":"field","network":"tcp,udp","outboundTag":"block"}]}}
if domain:
    c["inbounds"].append({"tag":"most-ws","listen":"0.0.0.0","port":8443,"protocol":"vless",
      "settings":{"clients":[{"id":j["uuid"],"email":"owner-ws"},{"id":j["proxy_uuid"],"email":"proxy"}],"decryption":"none"},
      "streamSettings":{"network":"ws","security":"tls",
        "tlsSettings":{"serverName":domain,"alpn":["http/1.1"],"certificates":[{"certificateFile":"/run/credentials/most-xray.service/cert","keyFile":"/run/credentials/most-xray.service/key"}]},
        "wsSettings":{"path":j["ws_path"]}}})
    # копия для проверки конфига вне службы (сертификат в службу передаётся через LoadCredential)
    t=json.loads(json.dumps(c))
    for ib in t["inbounds"]:
        for cert in ib.get("streamSettings",{}).get("tlsSettings",{}).get("certificates",[]):
            cert["certificateFile"]="/etc/letsencrypt/live/%s/fullchain.pem"%domain; cert["keyFile"]="/etc/letsencrypt/live/%s/privkey.pem"%domain
    open("/root/most-xray-test.json","w").write(json.dumps(t))
else:
    open("/root/most-xray-test.json","w").write(json.dumps(c))
open("/usr/local/etc/most-xray/config.json","w").write(json.dumps(c,indent=1))
PY
chmod 644 $X/config.json
/usr/local/bin/xray run -test -c /root/most-xray-test.json >/dev/null; rm -f /root/most-xray-test.json
cat > /etc/systemd/system/most-xray.service <<'EOF'
[Unit]
Description=Most: Xray (только RDP/управление для своих, интернет для пользователя proxy)
After=network-online.target
Wants=network-online.target
[Service]
DynamicUser=yes
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ExecStart=/usr/local/bin/xray run -c /usr/local/etc/most-xray/config.json
Restart=always
RestartSec=5
LimitNOFILE=65536
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload; systemctl enable most-xray >/dev/null 2>&1
restart_if most-xray "$XR_PRE" $X/config.json /etc/systemd/system/most-xray.service /etc/systemd/system/most-xray.service.d/cert.conf /usr/local/bin/xray
ufw allow 443/tcp comment 'Мост: Xray' >/dev/null

# ---------------------------------------------------------------- 6. most-admin
step "Служба управления устройствами"
install -d /root/build/most-admin; cp $SRC/most-admin.go /root/build/most-admin/main.go
(cd /root/build/most-admin && { [ -f go.mod ] || go mod init most-admin >/dev/null 2>&1; } && retry env CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /usr/local/bin/most-admin .)
[ -s /root/most-admin-token ] || { head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' > /root/most-admin-token; chmod 600 /root/most-admin-token; }
PUBIP=${MOST_PUBLIC_IP:-$(curl -s --max-time 10 https://api.ipify.org || hostname -I | awk '{print $1}')}
install -d -m700 /etc/most-admin /var/lib/most-admin
python3 - "$PW" "$PUBIP" "$AWG_PORT" "$(hostname)" "$SSH_PORT" <<'PY'
import json,sys
pw,ip,port,host,sshport=sys.argv[1:6]
x=json.load(open("/root/most-xray-client.json")); x.pop("priv",None)
c={"token":open("/root/most-admin-token").read().strip(),"name":host,"server":ip,"rdp_user":"cloud" if pw else "",
   "rdp_password":pw,"awg_host":"10.77.0.1","endpoint":ip+":"+port,"ssh_port":int(sshport or 22),"xray":x}
open("/etc/most-admin/config.json","w").write(json.dumps(c,ensure_ascii=False,indent=1))
PY
chmod 600 /etc/most-admin/config.json
cat > /etc/systemd/system/most-admin.service <<'EOF'
[Unit]
Description=Most: управление устройствами (только 127.0.0.1 и 10.77.0.1:8787)
After=network-online.target awg-quick@awg0.service
Wants=network-online.target
[Service]
ExecStart=/usr/local/bin/most-admin
Restart=always
RestartSec=5
PrivateTmp=yes
ProtectHome=read-only
# ключи SSH устройств (канал SSH): писать можно только сюда
ReadWritePaths=-/home/cloud/.ssh
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload; systemctl enable most-admin >/dev/null 2>&1; systemctl restart most-admin
ufw allow in on awg0 to any port 8787 proto tcp comment 'Мост: управление' >/dev/null

# ---------------------------------------------------------------- 7. рабочий стол (по желанию)
if [ "$DESKTOP" = 1 ]; then
  step "Рабочий стол KDE и RDP с автовходом (долго, до 30 мин)"
  id cloud >/dev/null 2>&1 || useradd -m -s /bin/bash -G sudo cloud
  echo "cloud:$PW" | chpasswd
  # имена пакетов в 24.04 (Plasma 5) и 26.04 (Plasma 6) разные — ставим те, что есть в репозитории
  WANT="kde-standard plasma-session-x11 kwin-x11 plasma-nm plasma-pa konsole dolphin kate ark gwenview okular spectacle
    breeze-gtk-theme kde-config-gtk-style xdg-desktop-portal-kde language-pack-kde-ru language-pack-ru
    xrdp xorgxrdp dbus-x11 fonts-noto-core fonts-noto-cjk fonts-noto-color-emoji libfuse2t64 libfuse2"
  HAVE=""; for p in $WANT; do apt-cache show "$p" >/dev/null 2>&1 && HAVE="$HAVE $p"; done
  echo "ставлю:$HAVE"
  apt_i $HAVE >/dev/null
  systemctl disable --now sddm >/dev/null 2>&1 || true; systemctl set-default multi-user.target >/dev/null
  # NetworkManager (его тянет plasma-nm) на сервере сетью не управляет — её настраивает systemd-networkd — и
  # сообщает «нет сети»; из-за этого установщик программ (Discover/PackageKit) отказывался работать
  # («Cannot refresh cache whilst offline»). Выключить, только если он действительно ничем не управляет.
  if systemctl is-active -q NetworkManager && systemctl is-active -q systemd-networkd \
     && networkctl list 2>/dev/null | grep -qE " $IFACE .* configured" \
     && ! nmcli -t -f DEVICE,STATE device 2>/dev/null | grep -q ':connected$'; then
    systemctl disable --now NetworkManager NetworkManager-wait-online >/dev/null 2>&1 || true
    systemctl mask NetworkManager >/dev/null 2>&1 || true
    systemctl restart packagekit 2>/dev/null || true
    echo "##INFO NetworkManager выключен: сетью сервера управляет systemd-networkd (иначе установщик программ считал сервер «без сети»)"
  fi
  XI=/etc/xrdp/xrdp.ini; SI=/etc/xrdp/sesman.ini; cp -p $XI $B/; cp -p $SI $B/
  XRDP_PRE=$(sum_of $XI $SI /etc/xrdp/xrdp_keyboard.ini /usr/sbin/xrdp-chansrv)
  python3 - "$XI" "$PW" <<'PY'
import sys,re
f,pw=sys.argv[1],sys.argv[2]; s=open(f).read()
s=re.sub(r"(?m)^autorun=.*$","autorun=Xorg",s) if re.search(r"(?m)^autorun=",s) else s.replace("[Globals]\n","[Globals]\nautorun=Xorg\n",1)
m=re.search(r"(?ms)^\[Xorg\]\n(.*?)(?=^\[)",s); blk=m.group(1)
blk=re.sub(r"(?m)^username=.*$","username=cloud",blk); blk=re.sub(r"(?m)^password=.*$","password="+pw,blk)
open(f,"w").write(s[:m.start(1)]+blk+s[m.end(1):])
PY
  chown root:xrdp $XI; chmod 640 $XI
  sed -i -E 's/^Policy=.*/Policy=U/' $SI
  python3 - /etc/xrdp/xrdp_keyboard.ini <<'PY'
import sys,re
f=sys.argv[1]; s=open(f).read()
if "options=grp:alt_shift_toggle" not in s: s=s.replace("layouts_map=default_layouts_map\n","layouts_map=default_layouts_map\noptions=grp:alt_shift_toggle\n",1)
m=re.search(r"(?ms)^\[default_layouts_map\]\n(.*?)(?=^\[)",s); b=m.group(1)
b=re.sub(r"(?m)^rdp_layout_us=us$","rdp_layout_us=us,ru",b); b=re.sub(r"(?m)^rdp_layout_ru=ru$","rdp_layout_ru=us,ru",b)
open(f,"w").write(s[:m.start(1)]+b+s[m.end(1):])
PY
  adduser xrdp ssl-cert >/dev/null 2>&1 || true
  printf 'export XDG_SESSION_TYPE=x11\nexport XDG_CURRENT_DESKTOP=KDE\nexport DESKTOP_SESSION=plasma\nexport KDE_FULL_SESSION=true\nexport LANG=ru_RU.UTF-8\nexec startplasma-x11\n' > /home/cloud/.xsession
  chown cloud:cloud /home/cloud/.xsession
  install -d -o cloud -g cloud /home/cloud/.config/plasma-workspace/env /home/cloud/thinclient_drives
  echo 'export QT_QUICK_BACKEND=software' > /home/cloud/.config/plasma-workspace/env/render.sh
  chown -R cloud:cloud /home/cloud/.config
  cat > /etc/polkit-1/rules.d/46-most.rules <<'EOF'
polkit.addRule(function(action, subject) {
  if ((action.id.indexOf("org.freedesktop.NetworkManager.") == 0 || action.id.indexOf("org.freedesktop.color-manager.") == 0) && subject.isInGroup("sudo")) { return polkit.Result.YES; }
});
EOF
  KW=$(command -v kwriteconfig6 || command -v kwriteconfig5 || echo true)   # Plasma 6 / Plasma 5
  k() { sudo -u cloud "$KW" --file "$1" --group "$2" --key "$3" "$4" 2>/dev/null || true; }
  k kscreenlockerrc Daemon Autolock false; k kscreenlockerrc Daemon LockOnResume false
  k kwinrc Compositing Enabled false; k baloofilerc "Basic Settings" "Indexing-Enabled" false
  k kxkbrc Layout Use true; k kxkbrc Layout LayoutList us,ru; k kxkbrc Layout Options grp:alt_shift_toggle
  for d in "/home/cloud/Рабочий стол" /home/cloud/Desktop; do install -d -o cloud -g cloud "$d"; ln -sfn /home/cloud/thinclient_drives "$d/Обмен"; chown -h cloud:cloud "$d/Обмен"; done
  # папка обмена: в пакете Ubuntu xrdp-chansrv собран без FUSE (ошибка debian/rules) — пересобрать только его
  if ! ldd /usr/sbin/xrdp-chansrv | grep -q libfuse; then
    sed -e 's/^Types: deb$/Types: deb-src/' /etc/apt/sources.list.d/ubuntu.sources > /etc/apt/sources.list.d/ubuntu-src.sources
    apt_up >/dev/null; retry apt-get build-dep -y -q xrdp >/dev/null; apt_i libfuse-dev devscripts >/dev/null
    install -d /root/build/xrdp-src && cd /root/build/xrdp-src && rm -rf xrdp-*/ && retry apt-get source -q xrdp >/dev/null 2>&1
    cd xrdp-*/ && sed -i '/^CONFIGURE_ARGS+=\t--disable-fuse$/d' debian/rules && dpkg-buildpackage -b -uc -us -j"$(nproc)" >/dev/null 2>&1
    cd .. && rm -rf x && dpkg-deb -x xrdp_*_amd64.deb x
    [ -e /usr/sbin/xrdp-chansrv.distrib ] || dpkg-divert --local --rename --add /usr/sbin/xrdp-chansrv >/dev/null
    install -m755 x/usr/sbin/xrdp-chansrv /usr/sbin/xrdp-chansrv
  fi
  # Google Chrome (по желанию): официальный репозиторий Google, ключ в отдельной связке; браузер по умолчанию
  if [ "${MOST_CHROME:-0}" = 1 ] && ! command -v google-chrome-stable >/dev/null; then
    install -d -m755 /etc/apt/keyrings
    # Chrome — по желанию: любой сбой (ключ, репозиторий, сеть до Google) не останавливает подготовку
    # (17.11 на проверке dl.google.com не ответил 5 минут — мастер падал на загрузке ключа)
    if retry bash -c 'curl -fsSL --max-time 60 https://dl.google.com/linux/linux_signing_key.pub | gpg --dearmor --yes -o /etc/apt/keyrings/google-chrome.gpg'; then
      echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/google-chrome.gpg] https://dl.google.com/linux/chrome/deb/ stable main" > /etc/apt/sources.list.d/google-chrome.list
      if ! { apt_up >/dev/null && apt_i google-chrome-stable >/dev/null; }; then
        rm -f /etc/apt/sources.list.d/google-chrome.list   # недоступный репозиторий не должен ломать apt update потом
        echo "##INFO Chrome не поставился (сеть или репозиторий Google) — рабочий стол работает и без него"
      fi
    else
      echo "##INFO Chrome пропущен: нет связи с dl.google.com — рабочий стол работает и без него"
    fi
    # пакет Chrome сам кладёт свой список источников — оставить один, чтобы apt не ругался на дубли
    [ -f /etc/apt/sources.list.d/google-chrome.sources ] && rm -f /etc/apt/sources.list.d/google-chrome.list || true
  fi
  # Claude Desktop (по желанию): официальная бета Anthropic для Ubuntu из их apt-репозитория
  # (code.claude.com/docs/en/desktop-linux); ключ проверяется по отпечатку из документации.
  # Без рекомендуемых пакетов: они тянут QEMU для Cowork (на VPS без KVM не работает) и gnome-keyring,
  # который на KDE конфликтует с KDE Wallet (вход в Claude не сохранялся бы).
  if [ "${MOST_CLAUDE:-0}" = 1 ] && ! installed claude-desktop; then
    CK=/usr/share/keyrings/claude-desktop-archive-keyring.asc
    if retry curl -fsSL --max-time 60 -o "$CK" https://downloads.claude.ai/claude-desktop/key.asc \
       && gpg --show-keys --with-colons "$CK" 2>/dev/null | grep -q '^fpr:*31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE:'; then
      echo "deb [arch=amd64,arm64 signed-by=$CK] https://downloads.claude.ai/claude-desktop/apt/stable stable main" > /etc/apt/sources.list.d/claude-desktop.list
      if ! { apt_up >/dev/null && apt_i --no-install-recommends claude-desktop >/dev/null; }; then
        rm -f /etc/apt/sources.list.d/claude-desktop.list
        echo "##INFO Claude не поставился (сеть или репозиторий Anthropic) — рабочий стол работает и без него"
      fi
    else
      rm -f "$CK"
      echo "##INFO Claude пропущен: нет связи с downloads.claude.ai или ключ подписи не совпал с ключом Anthropic"
    fi
  fi
  # на KDE вход в Claude хранится в KDE Wallet; gnome-keyring (его тянут пакеты рабочего стола) с ним конфликтует —
  # по документации Anthropic убрать (удаляется только он сам)
  if installed claude-desktop && installed gnome-keyring; then
    apt-get remove -y -q gnome-keyring >/dev/null || echo "##INFO gnome-keyring не удалился — вход в Claude может не сохраняться"
  fi
  if command -v google-chrome-stable >/dev/null; then
    sudo -u cloud xdg-settings set default-web-browser google-chrome.desktop 2>/dev/null || true
    k kdeglobals General BrowserApplication google-chrome.desktop
  fi
  systemctl enable xrdp xrdp-sesman >/dev/null 2>&1
  if [ "$XRDP_PRE" != "$(sum_of $XI $SI /etc/xrdp/xrdp_keyboard.ini /usr/sbin/xrdp-chansrv)" ] || ! systemctl is-active -q xrdp || ! systemctl is-active -q xrdp-sesman; then
    systemctl restart xrdp-sesman xrdp
  else
    echo "xrdp: без изменений — не перезапускаю (открытые сеансы рабочего стола не рвутся)"
  fi
  ufw allow in on awg0 to any port 3389 proto tcp comment 'Мост: RDP из туннеля' >/dev/null
fi

# ---------------------------------------------------------------- 8. проверка
step "Проверка служб"
sleep 2
FAIL=""
for s in awg-quick@awg0 unbound most-xray most-admin fail2ban; do systemctl is-active -q $s || FAIL="$FAIL $s"; done
[ "$DESKTOP" = 1 ] && { systemctl is-active -q xrdp || FAIL="$FAIL xrdp"; }
[ -z "$FAIL" ] || { echo "##ERR не запустились службы:$FAIL"; exit 4; }
curl -s -o /dev/null -w '%{http_code}' -H "X-Admin: $(cat /root/most-admin-token)" http://127.0.0.1:8787/api/devices | grep -q 200 || { echo "##ERR служба управления не отвечает"; exit 4; }

# ---------------------------------------------------------------- 9. итог
step "Готово"
# канал SSH первого устройства — как у устройств по приглашению: свой ключ, только проброс до рабочего стола
SSHOK=0
if [ "$DESKTOP" = 1 ] && id cloud >/dev/null 2>&1 && [[ "$OWNER_SSH" =~ ^ssh-ed25519\ AAAAC3NzaC1lZDI1NTE5[A-Za-z0-9+/]{48}$ ]]; then
  AK=/home/cloud/.ssh/authorized_keys
  install -d -m700 -o cloud -g cloud /home/cloud/.ssh
  touch $AK && chown cloud:cloud $AK && chmod 600 $AK
  grep -q " most-dev:$OWNER_PUB$" $AK || printf '%s %s most-dev:%s\n' 'restrict,port-forwarding,permitopen="127.0.0.1:3389",command="/usr/sbin/nologin"' "$OWNER_SSH" "$OWNER_PUB" >> $AK
  SSHOK=1
fi
python3 - "$PUBIP" "$AWG_PORT" "$SRV_PUB" "$OWNER_PSK" "$OWNER_IP" "$PW" "$(hostname)" "$SSHOK" "$SSH_PORT" <<PY
import json,sys
ip,port,spub,psk,oip,pw,host,sshok,sshport=sys.argv[1:10]
x=json.load(open("/root/most-xray-client.json")); x.pop("priv",None)
obf="""$OBF"""
awg="[Interface]\nPrivateKey = __LOCAL__\nAddress = %s/24\nMTU = 1280\n%s\n\n[Peer]\nPublicKey = %s\nPresharedKey = %s\nEndpoint = %s:%s\nAllowedIPs = 10.77.0.0/24\nPersistentKeepalive = 15\n"%(oip,obf,spub,psk,ip,port)
r={"name":host,"server":ip,"rdp_user":"cloud" if pw else "","rdp_password":pw,"xray":x,
   "awg":{"conf":awg,"host":"10.77.0.1"},"admin_token":open("/root/most-admin-token").read().strip()}
if sshok=="1":
    r["ssh"]={"user":"cloud","port":int(sshport or 22),"key":"__LOCAL__"}
open("/root/most-setup-result.json","w").write(json.dumps(r,ensure_ascii=False))
PY
chmod 600 /root/most-setup-result.json
# временные файлы мастера (скрипт и исходник службы) больше не нужны; журнал /var/log/most-setup.log остаётся
# для разбора — секретов в нём нет (пароль, ключи и токен в журнал не выводятся)
if [ "$SRC" = /root/most-setup ]; then rm -rf /root/most-setup; fi
echo "##DONE"
