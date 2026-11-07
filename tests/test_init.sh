#!/bin/sh
# Init-скрипт tgws на заглушках: procd, uci, nft и демона на машине
# разработчика нет, судить приходится по журналу вызовов.
. "$(dirname "$0")/lib.sh"

INIT="packages/luci-app-tgws/root/etc/init.d/tgws"
GEN_DIR="packages/luci-app-tgws/root/usr/libexec/tgws"

sandbox="$TT_TEST_TMP/sandbox"
bin="$sandbox/bin"
run="$sandbox/run"
mkdir -p "$bin" "$run"

CALLS="$sandbox/calls"
export CALLS
: > "$CALLS"

# Заглушка nft: журналирует вызов и сохраняет файл правил.
cat > "$bin/nft" <<'EOF'
#!/bin/sh
echo "nft $*" >> "$CALLS"
if [ "$1" = "-f" ]; then cp "$2" "$CALLS.ruleset"; fi
exit "${NFT_RC:-0}"
EOF
chmod +x "$bin/nft"
# Заглушка демона. Проверка по -x, а на Windows (Git Bash) пустой файл её не
# проходит, поэтому нужен shebang. Код возврата задаёт TGWS_CHECK_RC: так
# имитируется отказ `tgws -check` на недопустимых целях WebSocket.
printf '#!/bin/sh\nexit "${TGWS_CHECK_RC:-0}"\n' > "$bin/tgws"; chmod +x "$bin/tgws"

PATH="$bin:$PATH"; export PATH

# shellcheck disable=SC1090
. "$INIT"

BIN="$bin/tgws"
NFT="$bin/nft"
RUN="$run"
LIBDIR="$GEN_DIR"
STATS="$run/tgws/stats.json"
mkdir -p "$run/tgws"
RULESET="$run/tgws.nft"

# Заглушки библиотек OpenWrt. Значения UCI задаются переменными U_*.
config_load() { :; }
config_get_bool() { eval "$1=\${U_ENABLED:-0}"; }
config_get() {
	case "$3" in
		port) eval "$1=\${U_PORT:-$4}" ;;
		dc_ip) eval "$1=\${U_DC_IP:-$4}" ;;
		lan_devices) eval "$1=\${U_LAN:-$4}" ;;
		fallback_mark) eval "$1=\${U_FB:-$4}" ;;
		ip_dc) eval "$1=\${U_IPDC:-$4}" ;;
		worker_domain) eval "$1=\${U_WORKER:-$4}" ;;
		worker_mode) eval "$1=\${U_WORKER_MODE:-$4}" ;;
		worker_token) eval "$1=\${U_TOKEN:-$4}" ;;
		*) eval "$1=\"$4\"" ;;
	esac
}
# Account and ownership operations belong to OpenWrt, not the test host.
group_add_next() { echo 32768; }
user_exists() { return 0; }
id() { echo 32768; }
chown() { :; }
JAIL="$bin/tgws"
CAPS="packages/luci-app-tgws/root/etc/capabilities/tgws.json"
procd_add_jail() { echo "jail $*" >> "$CALLS"; }
procd_add_jail_mount() { echo "mount-ro $*" >> "$CALLS"; }
procd_add_jail_mount_rw() { echo "mount-rw $*" >> "$CALLS"; }
procd_open_instance() { echo "procd_open_instance $*" >> "$CALLS"; }
procd_set_param() { echo "procd_set_param $*" >> "$CALLS"; }
procd_close_instance() { echo "procd_close_instance" >> "$CALLS"; }
procd_add_reload_trigger() { echo "procd_add_reload_trigger $*" >> "$CALLS"; }
logger() { echo "logger $*" >> "$CALLS"; }
uci() { return 1; }
# Ожидание готовности не должно тормозить тесты.
sleep() { :; }

# Демон «слушает», когда есть его файл счётчиков: он пишет его сразу после
# bind. Сам wait_listening не подменяется — проверяется настоящий.
daemon_up() { echo '{}' > "$STATS"; }

# reset возвращает заглушки в исходное состояние. Настройки задаются обычными
# присваиваниями, а не префиксом перед вызовом функции: у функций поведение
# такого префикса зависит от оболочки, и значения протекали бы в следующий случай.
reset() {
	: > "$CALLS"
	rm -f "$CALLS.ruleset" "$STATS"
	U_ENABLED=0; U_PORT=; U_DC_IP=; U_LAN=; U_FB=; U_IPDC=; U_WORKER=; U_TOKEN=; U_WORKER_MODE=
	NFT_RC=0; export NFT_RC
	TGWS_CHECK_RC=0; export TGWS_CHECK_RC
}
calls() { cat "$CALLS"; }

# --- выключено ------------------------------------------------------------
reset
start_service; rc=$?
assert_eq "0" "$rc" "disabled: start_service succeeds"
assert_eq "0" "$(calls | grep -c procd_open_instance)" "disabled: no instance"
# procd убивает работающий экземпляр, когда start не открыл ни одного, а
# загруженная таблица осталась бы и перенаправляла в закрытый порт.
assert_contains "$(calls)" "nft delete table inet tgws" "disabled: a loaded table is removed"

# --- демона нет -------------------------------------------------------------
reset
BIN="$sandbox/missing"
U_ENABLED=1
start_service; rc=$?
assert_eq "1" "$rc" "missing binary: start_service fails"
assert_contains "$(calls)" "run install.sh" "missing binary: says what to do"
assert_contains "$(calls)" "nft delete table inet tgws" "missing binary: a loaded table is removed"
BIN="$bin/tgws"

# --- обычный запуск ---------------------------------------------------------
reset
U_ENABLED=1
start_service; rc=$?
assert_eq "0" "$rc" "enabled: start_service succeeds"
assert_contains "$(calls)" "procd_open_instance tgws" "enabled: opens the instance"
assert_contains "$(calls)" "-listen :5454" "enabled: default port"
assert_contains "$(calls)" "-dc-ip 2:149.154.167.220,4:149.154.167.220" "enabled: default DC targets"
assert_contains "$(calls)" "-mark 0x7467" "enabled: socket mark"
assert_contains "$(calls)" "procd_set_param user tgws" "runs as dedicated user"
assert_contains "$(calls)" "procd_set_param no_new_privs 1" "no privilege escalation"
assert_contains "$(calls)" "jail tgws log requirejail" "jail required"
assert_contains "$(calls)" "-stats $STATS" "enabled: stats file"
assert_eq "0" "$(calls | grep -c '^nft ')" "enabled: nft is NOT loaded before the daemon listens"
assert_eq "0" "$(calls | grep -c 'fallback-mark')" "enabled: no fallback mark unless configured"

# Соединения, которые нельзя провести через WebSocket, можно направить через
# туннель: метка туннеля TrustTunnel (0x9527) отправляет их по его таблице.
# По умолчанию опция выключена, и без TrustTunnel ничего не меняется.
reset
U_ENABLED=1; U_FB=0x9527
start_service; rc=$?
assert_eq "0" "$rc" "fallback mark: start_service succeeds"
assert_contains "$(calls)" "-fallback-mark 0x9527" "fallback mark: passed to the daemon"

reset
U_ENABLED=1; U_FB=0
start_service
assert_eq "0" "$(calls | grep -c 'fallback-mark')" "fallback mark 0 means off"

reset
U_ENABLED=1; U_FB='0x9527;rm'
start_service; rc=$?
assert_eq "1" "$rc" "fallback mark with unsupported characters: start_service fails"
assert_eq "0" "$(calls | grep -c procd_open_instance)" "fallback mark with unsupported characters: no instance"
assert_contains "$(calls)" "nft delete table inet tgws" "fallback mark with unsupported characters: a loaded table is removed"

# Значение, которое демон не разберёт, иначе завершало бы его при старте, а
# procd перезапускал бы его каждые 5 секунд.
reset
U_ENABLED=1; U_FB=0xZZ; TGWS_CHECK_RC=2; export TGWS_CHECK_RC
start_service; rc=$?
assert_eq "1" "$rc" "fallback mark the daemon rejects: start_service fails"
assert_eq "0" "$(calls | grep -c procd_open_instance)" "fallback mark the daemon rejects: no respawn loop"

# --- свой порт и недопустимые значения -------------------------------------
reset
U_ENABLED=1; U_WORKER=relay.example.workers.dev; U_TOKEN=0123456789abcdef0123456789abcdef
start_service; rc=$?
assert_eq "0" "$rc" "worker: starts"
assert_contains "$(calls)" "-worker-domain relay.example.workers.dev" "worker: domain passed"
assert_contains "$(calls)" "-worker-mode fallback" "worker: fallback by default"
assert_contains "$(calls)" "-worker-token-file $RUN/tgws-worker.token" "worker: token file passed"
assert_contains "$(calls)" "mount-ro $RUN/tgws-worker.token" "worker: token mounted read-only"
assert_contains "$(calls)" "/etc/resolv.conf" "worker: jail can resolve domain"
assert_eq "$U_TOKEN" "$(cat "$RUN/tgws-worker.token")" "worker: token file contents"
assert_eq "0" "$(calls | grep -c "$U_TOKEN")" "worker: secret absent from procd arguments and logs"
stop_service
assert_eq "0" "$(test -f "$RUN/tgws-worker.token" && echo 1 || echo 0)" "worker: stop removes token"

reset
U_ENABLED=1; U_WORKER=relay.example.workers.dev; TGWS_CHECK_RC=2; export TGWS_CHECK_RC
start_service; rc=$?
assert_eq "1" "$rc" "worker: rejected config does not start"
assert_eq "0" "$(calls | grep -c procd_open_instance)" "worker: no respawn for bad config"

reset
U_ENABLED=1; U_PORT=6000
start_service
assert_contains "$(calls)" "-listen :6000" "custom port"

reset
U_ENABLED=1; U_PORT=abc
start_service
assert_contains "$(calls)" "-listen :5454" "bad port falls back to the default"

reset
U_ENABLED=1; U_PORT=70000
start_service
assert_contains "$(calls)" "-listen :5454" "out-of-range port falls back to the default"

# Ведущие нули: 06000 — это порт 6000, а не восьмеричное число и не отказ.
reset
U_ENABLED=1; U_PORT=06000
start_service
assert_contains "$(calls)" "-listen :6000" "leading zeros in the port are stripped"

reset
U_ENABLED=1; U_PORT=000
start_service
assert_contains "$(calls)" "-listen :5454" "an all-zero port falls back to the default"

reset
U_ENABLED=1; U_DC_IP='2:1.2.3.4;rm'
start_service; rc=$?
assert_eq "1" "$rc" "bad dc_ip: start_service fails"
assert_eq "0" "$(calls | grep -c procd_open_instance)" "bad dc_ip: no instance"
assert_contains "$(calls)" "nft delete table inet tgws" "bad dc_ip: a loaded table is removed"

# Допустимые символы, но не допустимый смысл (DC 7 не существует): без проверки
# демон завершался бы при запуске, а procd перезапускал бы его каждые 5 секунд.
# Разбор целей делает сам демон (tgws -check), чтобы правило было одно.
reset
U_ENABLED=1; U_DC_IP='7:1.2.3.4'; TGWS_CHECK_RC=2; export TGWS_CHECK_RC
start_service; rc=$?
assert_eq "1" "$rc" "dc_ip the daemon rejects: start_service fails"
assert_eq "0" "$(calls | grep -c procd_open_instance)" "dc_ip the daemon rejects: no instance, no respawn loop"
assert_contains "$(calls)" "nft delete table inet tgws" "dc_ip the daemon rejects: a loaded table is removed"

# LuCI удаляет опцию, когда поле очищено, а init подставляет значение по
# умолчанию, поэтому «только прямой TCP» выражается явным словом none.
reset
U_ENABLED=1; U_DC_IP=none
start_service; rc=$?
assert_eq "0" "$rc" "dc_ip none: start_service succeeds"
assert_contains "$(calls)" "-dc-ip  -mark 0x7467" "dc_ip none: the daemon gets an empty target list"

# Ручная привязка адресов к DC (ip_dc): Android в прямом соединении не пишет
# настоящий индекс DC, и для нового адреса его можно привязать вручную.
reset
U_ENABLED=1
start_service
assert_eq "0" "$(calls | grep -c 'ip-dc')" "no manual address map unless configured"

reset
U_ENABLED=1; U_IPDC='149.154.167.35:2, 149.154.167.255:4'
start_service; rc=$?
assert_eq "0" "$rc" "ip_dc: start_service succeeds"
assert_contains "$(calls)" "-ip-dc 149.154.167.35:2, 149.154.167.255:4" "ip_dc: passed to the daemon"

reset
U_ENABLED=1; U_IPDC='1.2.3.4:2;rm'
start_service; rc=$?
assert_eq "1" "$rc" "ip_dc with unsupported characters: start_service fails"
assert_eq "0" "$(calls | grep -c procd_open_instance)" "ip_dc with unsupported characters: no instance"
assert_contains "$(calls)" "nft delete table inet tgws" "ip_dc with unsupported characters: a loaded table is removed"

reset
U_ENABLED=1; U_IPDC='1.2.3.4:9'; TGWS_CHECK_RC=2; export TGWS_CHECK_RC
start_service; rc=$?
assert_eq "1" "$rc" "ip_dc the daemon rejects: start_service fails"
assert_eq "0" "$(calls | grep -c procd_open_instance)" "ip_dc the daemon rejects: no respawn loop"

# --- устаревшие счётчики -----------------------------------------------------
# Файл прошлого экземпляра иначе сошёл бы за признак готовности нового и
# показывал бы в LuCI чужие цифры.
reset
U_ENABLED=1
daemon_up
start_service
assert_eq "no" "$([ -e "$STATS" ] && echo yes || echo no)" "start removes the previous instance's stats file"

# --- таблица грузится только когда демон слушает ----------------------------
reset
U_ENABLED=1; daemon_up
service_started
assert_contains "$(calls)" "nft -f" "listening: nft is loaded"
assert_contains "$(cat "$CALLS.ruleset")" "redirect to :5454" "listening: ruleset redirects to the port"
assert_contains "$(cat "$CALLS.ruleset")" 'iifname != { "br-lan" } return' "listening: default interface"

# Порт занят чужим процессом: демон упал на bind и файла счётчиков нет. Прежняя
# проверка по netstat приняла бы чужой процесс за демона и загрузила таблицу.
reset
U_ENABLED=1
service_started
assert_eq "0" "$(calls | grep -c '^nft ')" "not listening: nft is NOT loaded, Telegram stays untouched"
assert_contains "$(calls)" "not listening" "not listening: logged"

reset
service_started
assert_eq "0" "$(calls | grep -c '^nft ')" "disabled: service_started does nothing"

# --- интерфейсы из UCI -------------------------------------------------------
reset
U_ENABLED=1; U_LAN="br-lan br-guest"; daemon_up
service_started
assert_contains "$(cat "$CALLS.ruleset")" '"br-lan", "br-guest"' "several LAN interfaces"

reset
U_ENABLED=1; U_LAN='br-lan" ; drop'; daemon_up
service_started
assert_eq "0" "$(calls | grep -c '^nft ')" "a hostile interface name never reaches nft"
assert_contains "$(calls)" "cannot generate the ruleset" "a hostile interface name is reported"

# --- сбой nft ----------------------------------------------------------------
reset
U_ENABLED=1; NFT_RC=1; export NFT_RC; daemon_up
service_started
assert_contains "$(calls)" "nft rejected" "nft failure is logged"

# --- остановка ---------------------------------------------------------------
reset
daemon_up
stop_service
assert_contains "$(calls)" "nft delete table inet tgws" "stop removes the table"
# После остановки LuCI не должен показывать счётчики давно умершего процесса.
assert_eq "no" "$([ -e "$STATS" ] && echo yes || echo no)" "stop removes the stats file"

# --- обновление списка подсетей -----------------------------------------------
# Команда update_subnets запускает update-subnets и перезагружает правила
# только если список изменился и демон работает.
mkdir -p "$sandbox/lib"
cat > "$sandbox/lib/update-subnets" <<'EOF'
#!/bin/sh
echo "update-subnets run" >> "$CALLS"
echo "${UPD_OUT:-unchanged}"
exit "${UPD_RC:-0}"
EOF
cp "$GEN_DIR/gen-nft" "$GEN_DIR/subnets.sh" "$sandbox/lib/"
chmod +x "$sandbox/lib/update-subnets" "$sandbox/lib/gen-nft"
LIBDIR="$sandbox/lib"

reset
U_ENABLED=1; daemon_up; UPD_OUT="updated 9"; export UPD_OUT
update_subnets; rc=$?
assert_eq "0" "$rc" "update_subnets succeeds"
assert_contains "$(calls)" "nft -f" "a changed list reloads the rules while the daemon runs"

reset
U_ENABLED=1; daemon_up; UPD_OUT="unchanged"; export UPD_OUT
update_subnets
assert_eq "0" "$(calls | grep -c '^nft ')" "an unchanged list does not touch the rules"

# Демон не работает — загружать таблицу нельзя: перенаправлять в закрытый порт.
reset
U_ENABLED=1; UPD_OUT="updated 9"; export UPD_OUT
update_subnets
assert_eq "0" "$(calls | grep -c '^nft ')" "a changed list does not load the table when the daemon is down"

reset
U_ENABLED=1; daemon_up; UPD_OUT="boom"; UPD_RC=1; export UPD_OUT UPD_RC
update_subnets; rc=$?
assert_eq "1" "$rc" "a failed update fails"
assert_contains "$(calls)" "subnet list update failed" "a failed update is logged"
assert_eq "0" "$(calls | grep -c '^nft ')" "a failed update leaves the rules alone"
unset UPD_OUT UPD_RC
LIBDIR="$GEN_DIR"

# --- триггер перезагрузки ----------------------------------------------------
reset
service_triggers
assert_contains "$(calls)" "procd_add_reload_trigger tgws" "reload on config change"

reset
U_ENABLED=1
JAIL="$sandbox/missing"
start_service; rc=$?
assert_eq "1" "$rc" "missing jail refuses startup"
assert_eq "0" "$(calls | grep -c procd_open_instance)" "never falls back to root"
tt_test_summary
