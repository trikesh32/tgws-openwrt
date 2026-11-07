#!/bin/sh
# install.sh нельзя запустить на машине разработчика, поэтому проверяются два
# класса дефектов, которые находит только живой роутер: вызов программы, которой
# нет в busybox OpenWrt, и неверный порядок действий. Выбор архитектуры
# проверяется на настоящем фрагменте скрипта, а не по подстрокам.
. "$(dirname "$0")/lib.sh"

F=install.sh

# `install` не апплет busybox в стандартной сборке OpenWrt (BUSYBOX_DEFAULT_INSTALL
# выключен). В Alpine он есть, поэтому прогон установщика в Alpine этого не ловит.
assert_eq "0" "$(grep -cE '(^|[;&|(]|[[:space:]])install[[:space:]]+-' "$F")" \
	"install.sh does not call install(1), which stock OpenWrt lacks"

# The daemon and UI belong to one apk transaction. No unmanaged binary copy.
assert_eq "0" "$(grep -c 'BIN.new' "$F")" "no unmanaged daemon replacement"
assert_contains "$(cat "$F")" 'set -- "$tmp/core.apk" "$tmp/pkg.apk"' "daemon and UI installed together"
apk_line=$(grep -n '^if ! apk add --allow-untrusted' "$F" | cut -d: -f1)
sim_line=$(grep -n '^apk add --simulate' "$F" | cut -d: -f1)
stop_line=$(grep -n '^[[:space:]]*/etc/init.d/tgws stop' "$F" | cut -d: -f1)
assert_eq "yes" "$([ "$sim_line" -lt "$stop_line" ] && [ "$stop_line" -lt "$apk_line" ] && echo yes || echo no)" "dependency dry run precedes service stop and installation"

# После обновления служба перезапускается, а не стартует: у procd одинаковая
# командная строка означает «уже запущено», и старый код продолжил бы работать.
assert_contains "$(cat "$F")" "/etc/init.d/tgws restart" "an update restarts the service"

# Первая установка включает службу, обновление — нет: признак берётся до apk add.
fresh_line=$(grep -n "^\[ -e /etc/config/tgws \] || fresh=1" "$F" | head -n1 | cut -d: -f1)
assert_eq "yes" "$([ -n "$fresh_line" ] && [ -n "$apk_line" ] && [ "$fresh_line" -lt "$apk_line" ] && echo yes || echo no)" 	"first install is detected before the package is installed"
assert_contains "$(cat "$F")" "uci set tgws.main.enabled='1'" "a first install enables the service"
assert_contains "$(cat "$F")" 'TGWS_ENABLE:-1' "TGWS_ENABLE=0 opts out"

# Выбор архитектуры: настоящий фрагмент скрипта на реальных значениях DISTRIB_ARCH.
sed -n '/^case "\$DISTRIB_ARCH" in/,/^esac/p' "$F" > "$TT_TEST_TMP/arch.sh"
suffix_for() {
	( DISTRIB_ARCH="$1"; die() { echo DIE; exit 1; }; . "$TT_TEST_TMP/arch.sh"; echo "$suffix" ) 2>/dev/null
}
assert_eq "x86_64"  "$(suffix_for x86_64)" "x86_64"
assert_eq "aarch64" "$(suffix_for aarch64_cortex-a53)" "aarch64"
assert_eq "mipsel"  "$(suffix_for mipsel_24kc)" "little-endian mips"
assert_eq "mips"    "$(suffix_for mips_24kc)" "big-endian mips"
# На ARM без FPU (bcm53xx, arm_cortex-a9) бинарник под VFP не запускается, а
# procd перезапускал бы его каждые 5 секунд. Один программный (GOARM=5) на все.
assert_eq "arm" "$(suffix_for arm_cortex-a7_neon-vfpv4)" "arm with fpu"
assert_eq "arm" "$(suffix_for arm_cortex-a9)" "arm without fpu"
assert_eq "arm" "$(suffix_for arm_arm1176jzf-s_vfp)" "armv6"
assert_eq "DIE" "$(suffix_for mips64_octeonplus)" "mips64 is refused"
assert_eq "DIE" "$(suffix_for riscv64_riscv64)" "riscv64 is refused"

assert_eq "1" "$(grep -c '^build arm  *GOARCH=arm GOARM=5' scripts/build.sh)" "the arm build is soft-float GOARM=5"
assert_eq "0" "$(cat scripts/build.sh install.sh | grep -c 'tgws-linux-armv7\|build armv7')" "no stale armv7 name"

# Exercise the actual verifier on valid, modified, missing and duplicate assets.
sed -n '/^verify_asset()/,/^}/p' "$F" > "$TT_TEST_TMP/verify.sh"
printf 'reviewed executable\n' > "$TT_TEST_TMP/asset"
hash=$(sha256sum "$TT_TEST_TMP/asset" | cut -d' ' -f1)
verify() (
 tmp="$TT_TEST_TMP"
 die() { exit 1; }
 . "$TT_TEST_TMP/verify.sh"
 verify_asset "$TT_TEST_TMP/asset" tgws-linux-aarch64
)
printf '%s  tgws-linux-aarch64\n' "$hash" > "$TT_TEST_TMP/SHA256SUMS"
assert_exit 0 "valid release checksum accepted" verify
printf 'tampering\n' >> "$TT_TEST_TMP/asset"
assert_exit 1 "modified asset refused" verify
: > "$TT_TEST_TMP/SHA256SUMS"
assert_exit 1 "missing digest refused" verify
printf '%s  tgws-linux-aarch64\n%s  tgws-linux-aarch64\n' "$hash" "$hash" > "$TT_TEST_TMP/SHA256SUMS"
assert_exit 1 "duplicate digest refused" verify

tt_test_summary
