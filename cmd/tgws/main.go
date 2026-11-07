// Команда tgws — прозрачный прокси Telegram: принимает TCP-соединения,
// перенаправленные nftables, и ведёт их через WebSocket.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tgws/internal/dcmap"
	"tgws/internal/origdst"
	"tgws/internal/probe"
	"tgws/internal/proxy"
	"tgws/internal/sockmark"
	"tgws/internal/worker"
	"tgws/internal/ws"
)

// version подставляется при сборке: -ldflags "-X main.version=...".
var version = "dev"

func main() {
	listen := flag.String("listen", ":5454", "адрес, на котором принимать перенаправленные соединения")
	dcIP := flag.String("dc-ip", "2:149.154.167.220,4:149.154.167.220", "DC:IP, на которые открывать WebSocket (пусто — только прямой TCP)")
	ipDC := flag.String("ip-dc", "", "ручная привязка адресов к DC: IP:DC через запятую (поверх зашитой таблицы)")
	fallbackMark := flag.Int("fallback-mark", 0, "SO_MARK соединений, которые не удалось провести через WebSocket (0 — как -mark); например метка туннеля TrustTunnel 0x9527")
	mark := flag.Int("mark", 0x7467, "SO_MARK исходящих сокетов (0 — без метки)")
	statsPath := flag.String("stats", "", "файл со счётчиками в JSON (пусто — не писать)")
	verbose := flag.Bool("v", false, "подробный журнал по каждому соединению")
	showVersion := flag.Bool("version", false, "напечатать версию и выйти")
	workerDomain := flag.String("worker-domain", "", "домен Cloudflare Worker; пусто — прежний путь Telegram WebSocket")
	workerMode := flag.String("worker-mode", "fallback", "fallback — Worker перед прямым TCP; prefer — Worker первым для всех TCP-соединений")
	workerTokenFile := flag.String("worker-token-file", "", "файл с секретом Worker (секрет не передаётся в аргументах процесса)")
	doProbe := flag.Bool("probe", false, "проверить путь через WebSocket до Telegram (JSON в stdout) и выйти; используется диагностикой в LuCI")
	probeDC := flag.Int("dc", 2, "для -probe: номер дата-центра")
	probeMedia := flag.Bool("media", false, "для -probe: media-соединение")
	probeTarget := flag.String("target", "149.154.167.220", "для -probe: IP, на который открывать WebSocket")
	check := flag.Bool("check", false, "только проверить -dc-ip и выйти (код 0 — значение допустимо)")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	domain, err := worker.Domain(*workerDomain)
	if err != nil {
		log.Fatal(err)
	}
	if *workerMode != "fallback" && *workerMode != "prefer" {
		log.Fatal("worker-mode: expected fallback or prefer")
	}
	token, err := worker.ReadToken(*workerTokenFile)
	if err != nil {
		log.Fatal(err)
	}
	if token != "" && domain == "" {
		log.Fatal("worker-token-file requires worker-domain")
	}

	if *doProbe {
		// Настоящий запрос к Telegram тем же путём, что и у демона. Код 0,
		// если хотя бы один домен ответил: для работы достаточно одного.
		var res []probe.Result
		if domain == "" {
			res = probe.Run(context.Background(), *probeTarget, *probeDC, *probeMedia, 5*time.Second)
		} else {
			// Unlike Telegram's WebSocket gateway, Worker dials the actual DC.
			target := "149.154.167.51"
			flag.Visit(func(f *flag.Flag) {
				if f.Name == "target" {
					target = *probeTarget
				}
			})
			ip, e := netip.ParseAddr(target)
			if e != nil || !ip.Is4() {
				log.Fatal("Worker probe target must be IPv4")
			}
			res = probe.RunWith(context.Background(), []string{domain}, func(string) ws.Dialer {
				return worker.Dialer(domain, token, netip.AddrPortFrom(ip, 443), *mark)
			}, *probeDC, *probeMedia, 5*time.Second)
		}
		b, err := probe.Marshal(res)
		if err != nil {
			log.Fatalf("tgws: %v", err)
		}
		fmt.Println(string(b))
		for _, r := range res {
			if r.OK {
				return
			}
		}
		os.Exit(1)
	}

	targets, err := dcmap.ParseTargets(*dcIP)
	if err != nil {
		log.Fatalf("tgws: %v", err)
	}
	extraDC, err := dcmap.ParseIPMap(*ipDC)
	if err != nil {
		log.Fatalf("tgws: %v", err)
	}
	// init-скрипт проверяет цели до запуска службы: недопустимое значение
	// иначе завершало бы демон при старте, а procd перезапускал бы его
	// каждые 5 секунд. Правило допустимости одно — здесь, а не в shell.
	if *check {
		return
	}

	// Метки времени не нужны: procd пишет вывод в журнал сам.
	log.SetFlags(0)
	logf := func(format string, args ...any) { log.Printf(format, args...) }
	debugf := func(string, ...any) {}
	if *verbose {
		debugf = logf
	}

	cfg := proxy.Config{
		Targets: targets,
		ExtraDC: extraDC,
		OrigDst: origdst.Get,
		DialWS: func(ctx context.Context, target, domain, path string) (proxy.WSConn, error) {
			d := ws.Dialer{
				Addr:       net.JoinHostPort(target, "443"),
				ServerName: domain,
				Path:       path,
				Mark:       *mark,
				// Короче, чем кажется нужным: на живом роутере первое
				// соединение к IP дата-центра иногда пропадало (троттлинг), а
				// повтор на соседний домен проходил. Чем раньше сдались, тем
				// раньше попробовали следующий.
				Timeout: 4 * time.Second,
			}
			c, err := d.Dial(ctx)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		DialTCP: func(ctx context.Context, addr string) (net.Conn, error) {
			// Прямое соединение несёт свою метку: по умолчанию ту же, что и
			// WebSocket (мимо туннеля), а с -fallback-mark — метку туннеля,
			// и тогда всё, что нельзя провести через WebSocket (веб, DC без
			// цели), уходит через него, как это было до tgws.
			fb := *fallbackMark
			if fb == 0 {
				fb = *mark
			}
			d := net.Dialer{Timeout: 5 * time.Second, Control: sockmark.Control(fb)}
			return d.DialContext(ctx, "tcp", addr)
		},
		Logf:   logf,
		Debugf: debugf,
	}
	if domain != "" {
		cfg.WorkerFirst = *workerMode == "prefer"
		cfg.DialWorker = func(ctx context.Context, dst netip.AddrPort) (proxy.WSConn, error) {
			return worker.Dialer(domain, token, dst, *mark).Dial(ctx)
		}
	}
	srv := proxy.New(cfg)

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("tgws: %v", err)
	}
	logf("tgws %s listening on %s, WebSocket for DC targets %v", version, ln.Addr(), targets)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Файл счётчиков служит init-скрипту признаком «демон слушает»: он пишется
	// сразу после bind, поэтому его появление означает, что порт занят именно
	// этим процессом, а не чужим. При остановке файл удаляется ДО закрытия
	// порта: новый экземпляр не может занять порт раньше, и устаревший файл
	// не сойдёт за его готовность.
	go func() {
		if *statsPath != "" {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
		loop:
			for {
				if err := srv.Stats().WriteFile(*statsPath); err != nil {
					debugf("stats: %v", err)
				}
				select {
				case <-ctx.Done():
					break loop
				case <-t.C:
				}
			}
			_ = os.Remove(*statsPath)
		} else {
			<-ctx.Done()
		}
		ln.Close()
	}()
	if err := srv.Serve(ln); err != nil {
		fmt.Fprintln(os.Stderr, "tgws:", err)
		os.Exit(1)
	}
}
