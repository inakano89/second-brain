#!/usr/bin/env bash
# Second Brain — instalador para Linux com systemd (VPS, Raspberry/Orange Pi).
#   curl -fsSL https://raw.githubusercontent.com/inakano89/second-brain/main/deploy/install.sh | sudo bash
# Rodar de novo atualiza o binário mantendo .env e dados.
set -euo pipefail

REPO="${SB_REPO:-inakano89/second-brain}"
DIR="${SB_DIR:-/opt/second-brain}"
RUN_USER="${SB_USER:-brain}"

say() { printf '\033[1;35m→\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "Execute como root: curl ... | sudo bash"
command -v systemctl >/dev/null || die "systemd não encontrado (use Docker nesta máquina)."
command -v curl >/dev/null || die "curl não encontrado (apt install curl)."
command -v sha256sum >/dev/null || die "sha256sum não encontrado (apt install coreutils)."

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  armv7l|armv7|armhf) ARCH=armv7 ;;
  *) die "Arquitetura $(uname -m) não suportada." ;;
esac

ASSET="second-brain-linux-$ARCH"
BASE="https://github.com/$REPO/releases/latest/download"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

say "Baixando $ASSET ($REPO)…"
curl -fsSL -o "$TMP/$ASSET" "$BASE/$ASSET"
curl -fsSL -o "$TMP/SHA256SUMS" "$BASE/SHA256SUMS"
say "Verificando SHA-256…"
(cd "$TMP" && grep " \*\?$ASSET\$" SHA256SUMS | sha256sum -c --quiet -) || die "Checksum não confere — download corrompido."

if ! id "$RUN_USER" >/dev/null 2>&1; then
  say "Criando usuário de sistema '$RUN_USER'…"
  useradd --system --home-dir "$DIR" --shell /usr/sbin/nologin "$RUN_USER"
fi
mkdir -p "$DIR"
systemctl is-active --quiet second-brain && systemctl stop second-brain || true
install -m 0755 "$TMP/$ASSET" "$DIR/second-brain"
chown -R "$RUN_USER:$RUN_USER" "$DIR"
VERSION="$("$DIR/second-brain" -version | awk '{print $2}')"

say "Configurando serviço systemd…"
cat > /etc/systemd/system/second-brain.service <<EOF
[Unit]
Description=Second Brain
After=network-online.target
Wants=network-online.target

[Service]
User=$RUN_USER
Group=$RUN_USER
WorkingDirectory=$DIR
ExecStart=$DIR/second-brain -env $DIR/.env
Restart=always
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=$DIR

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now second-brain

PORT="$(grep -E '^HTTP_PORT=' "$DIR/.env" 2>/dev/null | cut -d= -f2 || true)"
PORT="${PORT:-8080}"
IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
printf '\n\033[1;32m✅ Second Brain %s instalado em %s\033[0m\n' "$VERSION" "$DIR"
echo "   Abra http://${IP:-SEU_IP}:$PORT no navegador para concluir o setup."
echo "   Logs:      journalctl -u second-brain -f"
echo "   Reiniciar: systemctl restart second-brain"
echo "   Firewall:  libere a porta $PORT (ex.: ufw allow $PORT/tcp) ou use HTTPS com Caddy (veja /help#https)."
