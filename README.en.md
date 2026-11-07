> Fork packaging: the daemon is now a separate `tgws` APK dependency of
> `luci-app-tgws`. Ready APKs currently target `aarch64_cortex-a53` / OpenWrt 25.12.6.
> The installer requires an APK matching the exact OpenWrt architecture; raw
> cross-built binaries are not installed outside the package manager.
> Manual Release workflow runs produce build artifacts without publishing a release.

# tgws for OpenWrt

> Security fork of [NooBiToo/tgws-openwrt](https://github.com/NooBiToo/tgws-openwrt),
> based on `f64036d`. Version **0.1.2 is not released yet**; installation examples
> require its tag and release assets. This revision has not been deployed to a router.

The fork rejects MTProto payloads above 2 MiB and malformed lengths, caps assembly
buffer capacity at 16 MiB across connections, and admits at most 64 connections
(16 per source IP). Stalled client/direct I/O expires after 5 minutes, WebSocket
writes after 30 seconds. These are router policies, not Telegram protocol maxima.
The service requires `procd-ujail`, runs as `tgws:tgws` with `no_new_privs` and only
`CAP_NET_RAW` (SO_MARK on Linux 5.17+), and refuses an unsandboxed fallback. Use a
listening port >=1024. Its statistics directory is the only explicit writable mount.
`GOMEMLIMIT=64MiB` is a soft GC target, not a hard RSS cap; TLS, WebSocket and kernel
socket memory are outside the 16 MiB assembly budget.

The installer requires an explicit `TGWS_VERSION` and verifies every downloaded
asset against release SHA256SUMS before replacing the daemon. Hashes from the same
GitHub release provide integrity, not an independent publisher signature. Local
APK installation still uses `--allow-untrusted` after verification. Account compromise
is outside this protection. WebSocket upgrades require a valid accept header and
bounded response header lines. See the Russian README for detailed changes and tests.


[Русский](README.md) · [Releases](https://github.com/trikesh32/tgws-openwrt/releases) · [Detailed guide](GUIDE.en.md) · [Report a bug](https://github.com/trikesh32/tgws-openwrt/issues)

**Telegram on every device of the home network goes over WebSocket — with nothing to set up on the devices. Control and diagnostics in LuCI.**

`luci-app-tgws` runs a transparent Telegram proxy on a router with **OpenWrt 25.12+**.
The router intercepts the devices' connections to Telegram's data centers and carries them over WebSocket
(`kws*.web.telegram.org`) — where the direct connection is slow or blocked. No proxy, secret or
`tg://proxy` links need to be set up on phones and computers.

- **Transparent:** an `nftables` rule redirects traffic to a local daemon; clients know nothing.
- **Does not break Telegram:** if the WebSocket is unavailable, the client's bytes are replayed verbatim over a direct connection.
- **Set up in the browser:** three LuCI pages — status, settings, diagnostics.
- **Real diagnostics:** not "the port is open" but a real request to every data center, timed, with a hint on what to fix.
- **Android and Desktop:** for Android, which does not name the data center in its connection, it is worked out automatically.
- **Works with TrustTunnel:** what cannot go over WebSocket (Telegram's web pages and the like) can be sent through your tunnel.

The idea and the protocol details come from [Flowseal/tg-ws-proxy](https://github.com/Flowseal/tg-ws-proxy); this is an
independent implementation in Go that works transparently on the router.

**Diagnostics in LuCI**

![tgws diagnostics in LuCI: checks with hints and a connection test with timings per address](Screenshot_2.png)

## How it works

```
a LAN device ─TCP→ a Telegram data center address
                      │  nftables: redirect to a local port
                      ▼
                    tgws (on the router)
                      ├─ WebSocket (TLS) → kws{N}.web.telegram.org
                      └─ if the WebSocket is unavailable — a direct connection (or through the tunnel)
```

The router removes and applies only the transport obfuscation of the connection. Telegram's messages stay
encrypted by its own protocol from the client to the server, and the router does not read them.

## Quick start

### What you need

- A router with **OpenWrt 25.12 or newer**, LuCI and the `apk` package manager.
- SSH access and about 5 MB of free space for the binary.
- A supported architecture: `x86_64`, `aarch64`, `arm`, `mips` or `mipsel`.
  Check it with `. /etc/openwrt_release; echo $DISTRIB_ARCH`.

OpenWrt 24.10 and earlier versions with `opkg` are not supported.

### 1. Install the package

Run over SSH **on the router**:

```sh
TGWS_VERSION=v0.1.2 sh -c "$(wget -O - https://raw.githubusercontent.com/trikesh32/tgws-openwrt/v0.1.2/install.sh)"
```

The installer checks compatibility and installs the dependencies, the daemon binary, the LuCI package and
the Russian translation. A first install turns the service on right away; run the installer with
`TGWS_ENABLE=0` to keep it off. An update leaves the current state (on or off) unchanged.

### 2. Configure the acceleration

If the service is off (installed with `TGWS_ENABLE=0`, or you turned it off), turn it on:
**Services → Telegram → Settings → Enable → Save & Apply**.

Optional:

- If you have [TrustTunnel](https://github.com/NooBiToo/TrustTunnelOpenWrt) and direct access to Telegram is closed,
   pick the tunnel (`0x9527`) on the **Other traffic** tab: what cannot go over WebSocket will go through it.

### 3. Check the result

Open **Diagnostics** and press **Run checks**, then **Test the connection**.
On the **Status** page the "Through WebSocket" counter grows while you use Telegram.
Restart the Telegram app on the device: connections that are already open are not redirected.

## Interface

**Status:** service control, interception and port, connection counters, package and daemon versions, the log.

![tgws status in LuCI: a running service, interception on, connection counters](Screenshot_1.png)

**Settings:** four tabs with hints and value validation.

![tgws settings in LuCI: tabs, the switch, the port, LAN interfaces](Screenshot_3.png)

**Diagnostics:** checks from the settings to a real request to Telegram, and a connection test per address
(the picture at the top).

## Good to know

- Only **IPv4** is intercepted. If IPv6 is on at the router, a client may bypass the acceleration over IPv6;
  diagnostics warns about it.
- By default the WebSocket is opened for **DC2 and DC4**. Other data centers and Telegram's web pages go
  directly or, if a mark is set, through the TrustTunnel tunnel.
- Clients with a **proxy or a VPN** do not enter the interception: turn the proxy off in Telegram's settings.
- The firewall zone of every network you intercept must accept input to the router (`input ACCEPT`), or
  Telegram cannot connect from it. For the ordinary `lan` network it does.
- If Telegram is selected in TrustTunnel's lists, that traffic does not reach it: the interception comes first.
- `/etc/init.d/firewall stop` removes all `nftables` tables, ours included; bring the interception back with
  `/etc/init.d/tgws restart`.
- Tested on Android and Windows Desktop; **iOS has not been tested**.

The full list of particulars is in the [limitations](GUIDE.en.md#limitations).

## Documentation

- [Detailed guide](GUIDE.en.md): installation, the interface, the internals, configuration without LuCI, updating and removal.
- [Settings reference](SETTINGS.ru.md) (in Russian): every option, defaults, examples.
- [How it works inside](GUIDE.en.md#how-it-works-inside): interception, finding the data center, fallback, protection against failures.
- [Diagnostics and common problems](GUIDE.en.md#diagnostics-and-common-problems).
- [Working with TrustTunnel](GUIDE.en.md#working-with-trusttunnel).
- [Which routers are supported](GUIDE.en.md#which-routers-are-supported).

To update, run the install command again: it updates the package and the daemon and keeps your settings.

## Support the project

**If the package is useful, give it a ⭐ [on GitHub](https://github.com/trikesh32/tgws-openwrt).**
It helps other users find the project.

Found a bug or tried the package on your router (especially on iOS)? [Open an issue](https://github.com/trikesh32/tgws-openwrt/issues)
with the model, the OpenWrt version, the package version and the diagnostics result.
Remove anything private from logs before posting them.

The package is free. You can support development with a donation at the [addresses in the TrustTunnelOpenWrt guide](https://github.com/NooBiToo/TrustTunnelOpenWrt/blob/main/GUIDE.en.md#donate): both projects have the same developer.

## License and credits

[MIT](LICENSE). The idea and the protocol details come from [Flowseal/tg-ws-proxy](https://github.com/Flowseal/tg-ws-proxy) (MIT).
The approach to the interface and to packaging was checked against [TrustTunnelOpenWrt](https://github.com/NooBiToo/TrustTunnelOpenWrt).
This is a community integration, not an official Telegram product.
