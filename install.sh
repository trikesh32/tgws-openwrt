#!/bin/sh
# Установщик tgws для OpenWrt 25.12+.
#   TGWS_VERSION=v0.1.2 sh -c "$(wget -O - https://raw.githubusercontent.com/trikesh32/tgws-openwrt/v0.1.2/install.sh)"
set -e

REPO="${TGWS_REPO:-trikesh32/tgws-openwrt}"
VERSION="${TGWS_VERSION:-}"
case "$REPO" in *[!A-Za-z0-9_./-]*|''|*..*) echo 'invalid TGWS_REPO' >&2; exit 1;; esac
case "$VERSION" in v[0-9]*) ;; *) echo 'Set TGWS_VERSION to an explicit release tag, e.g. v0.1.2' >&2; exit 1;; esac
case "$VERSION" in *[!A-Za-z0-9._-]*) echo 'invalid release tag' >&2; exit 1;; esac

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# Manifest is fetched over verified HTTPS from the selected GitHub release.
# This checks integrity, not an independent publisher signature.
verify_asset() {
	expected=$(awk -v name="$2" 'NF == 2 && $2 == name { print $1 }' "$tmp/SHA256SUMS")
	[ "${#expected}" = 64 ] || die "missing or duplicate checksum for $2"
	case "$expected" in *[!0-9a-f]*) die "invalid checksum for $2";; esac
	actual=$(sha256sum "$1") || die "cannot hash $2"
	[ "${actual%% *}" = "$expected" ] || die "checksum mismatch for $2"
}

[ -f /etc/openwrt_release ] || die "this script is for OpenWrt only"
# Файл существует только на роутере, линтеру его не видно.
# shellcheck disable=SC1091
. /etc/openwrt_release

major=$(printf '%s' "$DISTRIB_RELEASE" | cut -d. -f1)
case "$major" in
	[0-9]*) [ "$major" -ge 25 ] || die "OpenWrt 25.12 or newer is required (found $DISTRIB_RELEASE)" ;;
	*)      say "warning: cannot parse release '$DISTRIB_RELEASE', continuing" ;;
esac
command -v apk >/dev/null 2>&1 || die "apk not found; this package targets OpenWrt 25.12+"

# Архитектура выводится из DISTRIB_ARCH, а НЕ из `uname -m`: на mips ядро
# отвечает "mips" и для big-endian, и для little-endian, а бинарнику порядок
# байт важен. DISTRIB_ARCH различает их (mips_24kc и mipsel_24kc).
say "== Checking architecture"
case "$DISTRIB_ARCH" in
	x86_64)           suffix=x86_64 ;;
	aarch64_*)        suffix=aarch64 ;;
	arm_*)            suffix=arm ;;
	mipsel_*)         suffix=mipsel ;;
	mips_*)           suffix=mips ;;
	*) die "unsupported architecture '$DISTRIB_ARCH'; supported: x86_64, aarch64, arm, mips, mipsel" ;;
esac
say "   package architecture: $DISTRIB_ARCH (binary family: $suffix)"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "== Installing dependencies"
apk update
apk add curl ca-bundle firewall4

say "== Fetching release $VERSION"
curl -fsSL --max-time 30 "https://api.github.com/repos/$REPO/releases/tags/$VERSION" > "$tmp/release.json" \
	|| die "cannot reach the GitHub API for $REPO"

# Аргумент подставляется в sed без экранирования метасимволов: вызывать только
# с буквальными строками, как ниже.
pick_asset() {
	sed -n 's/.*"browser_download_url": *"\([^"]*'"$1"'\)".*/\1/p' "$tmp/release.json" | head -n1
}

sums_url=$(pick_asset '\/SHA256SUMS')
[ -n "$sums_url" ] || die "release has no SHA256SUMS; refusing unchecked installation"
curl -fsSL --proto '=https' --proto-redir '=https' --max-time 60 -o "$tmp/SHA256SUMS" "$sums_url" || die "cannot download checksums"

core_url=$(pick_asset "\/tgws-[^\"\/]*-$DISTRIB_ARCH\\.apk")
pkg_url=$(pick_asset '\/luci-app-tgws[^"\/]*\.apk')
i18n_url=$(pick_asset '\/luci-i18n-tgws-ru[^"\/]*\.apk')
[ -n "$core_url" ] || die "no tgws .apk for $DISTRIB_ARCH in release $VERSION of $REPO"
[ -n "$pkg_url" ] || die "no luci-app-tgws .apk in release $VERSION of $REPO"

say "   $core_url"
curl -fsSL --proto '=https' --proto-redir '=https' --max-time 120 -o "$tmp/core.apk" "$core_url" || die "failed to download $core_url"
verify_asset "$tmp/core.apk" "${core_url##*/}"

say "   $pkg_url"
curl -fsSL --proto '=https' --proto-redir '=https' --max-time 120 -o "$tmp/pkg.apk" "$pkg_url" || die "failed to download $pkg_url"
verify_asset "$tmp/pkg.apk" "${pkg_url##*/}"
if [ -n "$i18n_url" ]; then
	curl -fsSL --proto '=https' --proto-redir '=https' --max-time 120 -o "$tmp/i18n.apk" "$i18n_url" \
	 || die "failed to download translation package"
	verify_asset "$tmp/i18n.apk" "${i18n_url##*/}"
fi

# Resolve dependencies and check space/conflicts before interrupting Telegram.
set -- "$tmp/core.apk" "$tmp/pkg.apk"
[ ! -f "$tmp/i18n.apk" ] || set -- "$@" "$tmp/i18n.apk"
apk add --simulate --allow-untrusted "$@"

fresh=0
[ -e /etc/config/tgws ] || fresh=1
was_running=0
if [ -x /etc/init.d/tgws ]; then
	/etc/init.d/tgws running >/dev/null 2>&1 && was_running=1
	say "== Stopping the running service"
	/etc/init.d/tgws stop
fi

say "== Installing daemon and LuCI packages"
if ! apk add --allow-untrusted "$@"; then
	[ "$was_running" != 1 ] || /etc/init.d/tgws start || true
	die "package installation failed; inspect apk output before retrying"
fi

say "== Restarting LuCI backend"
/etc/init.d/rpcd restart >/dev/null 2>&1 || true

if [ "$was_running" = "1" ]; then
	say "== Starting the service back up"
	# restart, а не start: у procd одинаковая командная строка означает
	# «уже запущено», и демон, поднятый postinst-ом, не был бы перезапущен.
	/etc/init.d/tgws restart || say "warning: the service did not start; see 'logread -e tgws'"
fi

# Первая установка сразу включает службу: пакет без работающего перехвата
# бесполезен, а запускать установщик — уже осознанное решение. TGWS_ENABLE=0
# оставляет службу выключенной.
if [ "$fresh" = "1" ] && [ "${TGWS_ENABLE:-1}" != "0" ]; then
	say "== Enabling the service"
	uci set tgws.main.enabled='1' && uci commit tgws
	/etc/init.d/tgws restart || say "warning: the service did not start; see 'logread -e tgws'"
	enabled_now=1
fi

say ""
say "== Done"
if [ "${enabled_now:-0}" = "1" ]; then
	say "The service is on. Settings: LuCI -> Services -> Telegram."
elif [ "$(uci -q get tgws.main.enabled)" = "1" ]; then
	say "Settings: LuCI -> Services -> Telegram."
else
	say "The service is OFF. Turn it on in LuCI -> Services -> Telegram (Save & Apply), or:"
	say "  uci set tgws.main.enabled='1' && uci commit tgws && /etc/init.d/tgws restart"
fi
