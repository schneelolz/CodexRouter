// CodexRouter 提供双击托盘启动和保留的前台服务入口。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"codexrouter/internal/clients"
	"codexrouter/internal/daily"
	"codexrouter/internal/diaglog"
	"codexrouter/internal/instance"
	"codexrouter/internal/platform"
	"codexrouter/internal/router"
	"codexrouter/internal/store"
	"log"
)

var version = "0.4.0-test-history1"

func init() {
	// AppKit must run on the process main thread; other platforms keep their lifecycle.
	if runtime.GOOS == "darwin" {
		runtime.LockOSThread()
	}
}

func main() {
	if err := run(); err != nil {
		if len(os.Args) == 1 || (len(os.Args) > 1 && os.Args[1] == "launch") {
			platform.Message("CodexRouter 启动失败", err.Error(), true)
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}

type launchOptions struct {
	gui, apiOnly, open bool
	ui, data, origin   string
	port               int
}
type instanceRecord struct {
	Version int `json:"version"`
	Port    int `json:"port"`
}

func parseOptions(args []string, exe string) (launchOptions, error) {
	var out launchOptions
	out.gui = len(args) == 0 || (len(args) > 0 && args[0] == "launch")
	if len(args) > 0 && args[0] != "serve" && args[0] != "launch" {
		return out, errors.New("用法: 双击运行，或 codexrouter serve [--ui-dir 路径] [--data-dir 路径] [--port 18788]")
	}
	defaults := ""
	if out.gui {
		defaults = defaultUIDir(exe)
	}
	flags := flag.NewFlagSet("CodexRouter", flag.ContinueOnError)
	flags.BoolVar(&out.apiOnly, "api-only", false, "仅提供API")
	flags.StringVar(&out.ui, "ui-dir", defaults, "前端静态目录")
	flags.StringVar(&out.data, "data-dir", "", "独立运行数据目录")
	flags.IntVar(&out.port, "port", 18788, "本机监听端口")
	flags.BoolVar(&out.open, "open", out.gui, "打开管理页面")
	flags.StringVar(&out.origin, "dev-origin", "", "显式Vite开发源")
	tail := []string{}
	if len(args) > 0 {
		tail = args[1:]
	}
	if err := flags.Parse(tail); err != nil {
		return out, err
	}
	if flags.NArg() != 0 || out.port < 1 || out.port > 65535 {
		return out, errors.New("参数或端口无效")
	}
	if out.apiOnly && out.ui != "" {
		return out, errors.New("--api-only不能同时指定前端目录")
	}
	if out.open && out.ui == "" && out.origin == "" {
		return out, errors.New("打开页面需要前端目录")
	}
	if out.data == "" {
		var err error
		out.data, err = platform.DefaultDataDir()
		if err != nil {
			return out, err
		}
	}
	abs, err := filepath.Abs(out.data)
	if err != nil {
		return out, err
	}
	out.data = abs
	return out, nil
}

var platformCommand = func([]string) (bool, error) { return false, nil }

func run() (runErr error) {
	if handled, err := platformCommand(os.Args[1:]); handled {
		return err
	}
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return nil
	}
	if len(os.Args) > 1 && os.Args[1] == "client-console" {
		f := flag.NewFlagSet("专用客户端", flag.ContinueOnError)
		profile := f.String("profile", "", "专用目录")
		action := f.String("action", "", "固定动作")
		if err := f.Parse(os.Args[2:]); err != nil {
			return err
		}
		return clients.RunConsole(*profile, *action)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	o, err := parseOptions(os.Args[1:], exe)
	if err != nil {
		return err
	}
	db, err := store.Open(o.data)
	if err != nil {
		if o.gui && errors.Is(err, store.ErrLocked) {
			return openExisting(o.data)
		}
		return fmt.Errorf("状态目录不可用或已有实例持锁: %w", err)
	}
	defer db.Close()
	diagnosticLog, logErr := diaglog.Open(filepath.Join(o.data, "logs"), version)
	if diagnosticLog == nil {
		return errors.New("无法初始化诊断日志标识")
	}
	diagnosticLog.Emit("process_start", diaglog.Fields{"port": o.port})
	defer func() {
		reason := "normal"
		if runErr != nil {
			reason = "startup_or_serve_failed"
		}
		diagnosticLog.Emit("process_stop", diaglog.Fields{"reason": reason})
		diagnosticLog.Close()
	}()
	if logErr != nil {
		fmt.Fprintln(os.Stderr, "诊断日志暂不可写；服务继续运行，请查看近期诊断。")
	}
	creds, err := platform.OpenCredentials(o.data)
	if err != nil {
		return err
	}
	address := fmt.Sprintf("127.0.0.1:%d", o.port)
	ln, err := net.Listen("tcp4", address)
	if err != nil {
		return fmt.Errorf("监听 %s 失败，未更换端口或停止其他程序: %w", address, err)
	}
	defer ln.Close()
	var manager *clients.Manager
	if !o.apiOnly {
		manager, err = clients.New(filepath.Join(o.data, "clients"), "http://"+address)
		if err != nil {
			return err
		}
		defer manager.Close()
	}
	var dailyManager *daily.Manager
	if !o.apiOnly {
		// 日常路径不可安全确定时仅禁用此功能，保留路由和独立测试。
		dailyManager, _ = daily.Discover("http://"+address, o.data)
	}
	svc, err := router.New(router.Options{Log: diagnosticLog, HistoryPath: filepath.Join(o.data, "history.sqlite"), Store: db, Credentials: creds, Authority: address, DevOrigin: o.origin, UIDir: o.ui, Clients: manager, Daily: dailyManager})
	if err != nil {
		return err
	}
	defer svc.Close()
	openPage := func() error {
		diagnosticLog.Emit("management_open", nil)
		base := "http://" + address
		if o.origin != "" {
			base = o.origin
		}
		if o.ui == "" && o.origin == "" {
			return errors.New("无管理页面")
		}
		err := platform.OpenBrowser(base + "/#token=" + svc.Bootstrap())
		reason := "ok"
		if err != nil {
			reason = "browser_launch_failed"
		}
		diagnosticLog.Emit("management_open_result", diaglog.Fields{"reason": reason})
		return err
	}
	key, err := creds.Get("control")
	if err != nil {
		return err
	}
	reopen := instance.Handler(key, openPage)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == instance.Path {
			if r.Host != address {
				http.Error(w, "拒绝非本机Host", 403)
				return
			}
			reopen.ServeHTTP(w, r)
			return
		}
		svc.ServeHTTP(w, r)
	})
	host := router.HTTPServer(address, handler)
	host.ErrorLog = log.New(router.DiagnosticHTTPWriter(diagnosticLog), "", 0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	record := instanceRecord{1, o.port}
	if err = db.Write("instance.json", record); err != nil {
		return err
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if host.Shutdown(deadline) != nil {
			svc.Close()
			_ = host.Close()
		}
	}()
	if !o.gui {
		go func() {
			reader := bufio.NewScanner(os.Stdin)
			for reader.Scan() {
				switch strings.TrimSpace(reader.Text()) {
				case "open":
					if err := openPage(); err != nil {
						fmt.Fprintln(os.Stderr, "浏览器启动失败")
					}
				case "quit", "exit":
					stop()
					return
				}
			}
			if reader.Err() != nil {
				fmt.Fprintln(os.Stderr, "前台输入结束")
			}
		}()
	}
	diagnosticLog.Emit("process_ready", diaglog.Fields{"port": o.port})
	fmt.Printf("CodexRouter %s 运行成功，仅本机 %s；输入open打开设置，quit退出。\n", version, address)
	serveErr := make(chan error, 1)
	go func() { serveErr <- host.Serve(ln); stop() }()
	if o.open {
		if err = openPage(); err != nil {
			stop()
			<-serveErr
			<-shutdownDone
			return errors.New("浏览器打开失败，请使用托盘重新打开或重启")
		}
	}
	if o.gui {
		if err = platform.Tray(ctx, openPage, stop); err != nil {
			stop()
			<-serveErr
			<-shutdownDone
			return err
		}
		stop()
	}
	err = <-serveErr
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return nil
	}
	return err
}
func defaultUIDir(exe string) string {
	dir := filepath.Dir(exe)
	if runtime.GOOS == "darwin" && filepath.Base(dir) == "MacOS" && filepath.Base(filepath.Dir(dir)) == "Contents" {
		return filepath.Join(filepath.Dir(dir), "Resources", "ui")
	}
	return filepath.Join(dir, "ui")
}

func openExisting(dir string) error {
	deadline := time.Now().Add(18 * time.Second)
	var last error
	for {
		last = openExistingOnce(dir)
		if last == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return last
		}
		time.Sleep(200 * time.Millisecond)
	}
}
func openExistingOnce(dir string) error {
	raw, err := os.ReadFile(filepath.Join(dir, "instance.json"))
	if err != nil {
		return errors.New("已有实例占用数据目录，但无法确认其控制入口")
	}
	var record instanceRecord
	if json.Unmarshal(raw, &record) != nil || record.Version != 1 || record.Port < 1 || record.Port > 65535 {
		return errors.New("已有实例记录无效，未连接其他端口")
	}
	creds, err := platform.OpenCredentials(dir)
	if err != nil {
		return err
	}
	key, err := creds.Get("control")
	if err != nil {
		return err
	}
	return instance.Open(fmt.Sprintf("http://127.0.0.1:%d", record.Port), key)
}
