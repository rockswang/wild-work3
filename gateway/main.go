package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"wild-work3/main/appconfig"
	"wild-work3/main/gateway/internal/gw"
	"wild-work3/main/gateway/internal/ipc"
	"wild-work3/main/gateway/internal/proxy"
	"wild-work3/main/gateway/internal/registry"
	"wild-work3/main/gateway/internal/store"
	"wild-work3/main/gateway/internal/tray"
	"wild-work3/main/gateway/internal/v1"
	"wild-work3/main/protocol/chat"
	"wild-work3/main/protocol/cli"
)

const gatewayVersion = "1.0.0"

// protocolVersion 当前 IPC 协议版本（版本门比较基准，与 main/protocol 保持一致）。
const protocolVersion = 1

// 缺省分发位置（W3-48 v2）：注册表 = 平铺注册文件的独立仓，一次 tarball 全拿。
//
// 包体不在注册表仓：注册文件只给 `repo` + `version`，由发布页（tags.atom / expanded_assets）定位。
const (
	defaultRegistryRepo = "wild-work3/ww3-ext-registry"
	defaultRegistryRef  = "main"
)

// defaultProxies 国内可达的 release 资产代理（实测同一条 URL 直连/gh-proxy/ghfast 均通 206）。
var defaultProxies = []string{"https://gh-proxy.com/", "https://ghfast.top/"}

// wild-work3（gateway）：本工具唯一对外 origin。
//
//	/v1/*   对外三协议（Chat / Responses / Anthropic）
//	/gw/*   gateway 管理面（版本/插件商店/安装/升级/回滚/卸载/更新）
//	/core/* → core 管理面（剥离前缀反代到 core 的 /api/*）
//	/ext/<id>/* → 插件自定义 UI
//	/        反代到 core 的 Web UI（静态资产在 core，W3-37 定案 1）
//
// 边界：gateway 不内嵌默认规则表、不读 core 的配置段（W3-37），依赖由 deps_test.go 守住。
//
// 模式（都能直接命令行使用，便于分层联调）：
//
//	serve   默认。启动 core 子进程 + 对外 HTTP
//	chat    命令行直接对话（走 gateway→core IPC，与 /v1 同一条链路）
//	status  探活：core IPC ping + core 管理面 health + gateway 自身版本
//	models  经 core 管理面列出模型
func main() {
	app := cli.NewApp("wild-work3", "wild-work3 网关（对外三协议 + 管理面 + 插件商店）", "serve")

	cServe := app.Cmd("serve", "启动网关：拉起 core 子进程 + 对外 HTTP（默认）")
	listen := cServe.Str("listen", "", "对外监听地址（默认读配置，否则 127.0.0.1:7863）")
	coreExe := cServe.Str("core", defaultCoreExe(), "core 可执行文件路径")
	dataDir := cServe.Str("data", defaultDataDir(), "数据目录")
	pluginsDir := cServe.Str("plugins", defaultPluginsDir(), "插件目录")
	registryRepo := cServe.Str("registry", "", "插件注册表仓（owner/repo，取 codeload tarball 枚举根目录注册文件；缺省读配置，否则 "+defaultRegistryRepo+"）")
	registryRef := cServe.Str("registry-ref", "", "注册表仓的分支/tag/commit（缺省读配置，否则 "+defaultRegistryRef+"）")
	proxies := cServe.StrList("proxy", "下载代理前缀（可重复传，与直连并发竞速；缺省读配置，否则内置两个）")
	noTray := cServe.Bool("no-tray", false, "无头模式：不创建托盘图标（服务/无桌面环境）")

	// W3-38：gateway 不参与自动升级，所手改路径必须能在命令行上直接拿到。
	app.SetVersion(func() string {
		self := selfExe()
		return fmt.Sprintf("wild-work3 gateway %s｜IPC 协议 %d｜core 可执行=%s\n手动升级：先停进程，把 %s 改名（+%s.old）再放入新文件；\n期望包位：https://github.com/%s/releases",
			gatewayVersion, protocolVersion, absPath(defaultCoreExe()), filepath.Base(self), filepath.Base(self), defaultRegistryRepo)
	})

	cChat := app.Cmd("chat", "命令行直接对话（走 gateway→core IPC，与 /v1 同一条链路）")
	chatModel := cChat.Str("model", os.Getenv("WW3_MODEL"), "模型名")
	chatPrompt := cChat.Str("prompt", "你好，请用一句话自我介绍", "提示词")
	cChatData := cChat.Str("data", defaultDataDir(), "数据目录")
	cChatCore := cChat.Str("core", defaultCoreExe(), "core 可执行文件路径")
	cChatPlugins := cChat.Str("plugins", defaultPluginsDir(), "插件目录")

	cStatus := app.Cmd("status", "分层探活：core IPC → core 管理面 → gateway 版本")
	statusData := cStatus.Str("data", defaultDataDir(), "数据目录")
	statusCore := cStatus.Str("core", defaultCoreExe(), "core 可执行文件路径")
	statusPlugins := cStatus.Str("plugins", defaultPluginsDir(), "插件目录")

	cModels := app.Cmd("models", "经 core 管理面列出各渠道模型")
	modelsData := cModels.Str("data", defaultDataDir(), "数据目录")
	modelsCore := cModels.Str("core", defaultCoreExe(), "core 可执行文件路径")
	modelsPlugins := cModels.Str("plugins", defaultPluginsDir(), "插件目录")

	cmd, err := app.Parse(os.Args[1:])
	if err != nil {
		if err == cli.ErrHelp {
			return
		}
		fatal("%v", err)
	}

	data := *dataDir
	if data == "" {
		data = cli.ResolveData("")
	}
	plugins := *pluginsDir
	if plugins == "" {
		plugins = filepath.Join(data, "plugins")
	}
	if err := os.MkdirAll(data, 0o755); err != nil {
		fatal("创建数据目录失败: %v", err)
	}
	if err := os.MkdirAll(plugins, 0o755); err != nil {
		fatal("创建插件目录失败: %v", err)
	}

	log, err := cli.NewLogger("gateway", cli.LogPath(data, "gateway"))
	if err != nil {
		log = nil
	}
	if log != nil {
		defer log.Close()
	}
	l = log
	l.Infof("启动 gateway v%s command=%s", gatewayVersion, cmd.CmdName())

	// 先建配置存储，再算监听与 core 地址：W3-37 定案 2 把「core 听哪里」的决定权交给 gateway，
	// gateway 不再读 core 的配置段（原 readCoreBase/listenOf 已删，顺带消掉「gateway 必须抢先迁移 core 段」的时序 hack）。
	cfgStore := newGWConfig(data) // 可热更新的 gateway 配置（管理口令/API Key/监听/core_listen）

	// 监听地址：命令行 > 配置文件 > 默认
	listenAddr := *listen
	if listenAddr == "" {
		listenAddr = cfgStore.Get().Listen
	}
	if listenAddr == "" {
		listenAddr = defaultGWListen
	}
	// 首次启动把生效监听写回配置，便于面板区分「已保存」与「实际生效」。
	if cfgStore.Get().Listen == "" {
		_ = cfgStore.Update(func(c *gatewayConfig) { c.Listen = listenAddr })
	}
	coreAddr := coreAddrFor(cfgStore.Get(), listenAddr)
	coreIPC := newCoreIPC(*coreExe, data, plugins, coreAddr, listenAddr)
	coreBase := "http://" + coreAddr

	switch cmd.CmdName() {
	case "chat":
		addr := coreAddrFor(loadGWSection(*cChatData), "")
		runChat(newCoreIPC(*cChatCore, *cChatData, *cChatPlugins, addr, ""), "http://"+addr, *chatModel, *chatPrompt)
		return
	case "status":
		addr := coreAddrFor(loadGWSection(*statusData), "")
		runStatus(newCoreIPC(*statusCore, *statusData, *statusPlugins, addr, ""), "http://"+addr)
		return
	case "models":
		_, _ = modelsCore, modelsPlugins // models 只走 core 的 HTTP 管理面，不 spawn
		runModels("http://" + coreAddrFor(loadGWSection(*modelsData), ""))
		return
	}

	// ---- serve ----
	// core 生命周期（W3-37 附带修复 1，P0）：不再一次性 Start，而是后台监督 + 退避重启。
	// 原行为：core 异常退出后 /core/* 恒 502、/v1/* 漏内部串，永不自愈。
	coreIPC.OnState(func(format string, a ...any) { l.Infof(format, a...) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go coreIPC.Supervise(ctx)

	// 规则引擎 Pass A（W3-37 定案 3）：默认表内嵌在 core，gateway 向 core 拉「生效规则表」。
	// 拉不到就 Pass A 跳过（沿用既有 nil 语义：入站归一化仍生效，不阻断对话），不阻塞启动。
	go rulesLoop(ctx, coreBase)

	h := &v1.Handler{IPC: coreIPC, KeyFn: func() string { return cfgStore.Get().APIKey }, CoreBase: coreBase,
		Version: map[string]string{"gateway": gatewayVersion}}

	st := store.New(plugins)
	gws := &gw.Server{Store: st, Version: map[string]string{"gateway": gatewayVersion, "protocol": strconv.Itoa(protocolVersion)},
		Dist:   distFor(cfgStore.Get(), *registryRepo, *registryRef, *proxies),
		OnQuit: func() { shutdown(coreIPC) }}
	gws.CoreVersionFn = func() map[string]string { return map[string]string{"core": coreVersionHTTP(coreBase)} }
	// core 更新应用要用：exe 路径、停 core 并等它真退出、替换后重新拉起（W3-38）
	gws.CoreExe = absPath(*coreExe)
	gws.GatewayExe = selfExe()
	gws.UpdateDir = filepath.Join(data, "updates")
	gws.HaltCore = func() error { return coreIPC.Halt(10 * time.Second) }
	gws.StartCore = func() {
		if err := coreIPC.Resume(context.Background()); err != nil {
			l.Errorf("重新拉起 core 失败: %v", err)
		}
		notifyRules() // core 重启期间 Pass A 会短暂为 nil
	}
	gws.ReloadFn = func() {
		// 插件目录变更后重扫：杀掉 core 让监督循环立即重拉（Restart 不置 stopped），
		// 重拉后顺手重拉一次生效规则表（core 重启期间 Pass A 会短暂为 nil）。
		coreIPC.Restart()
		notifyRules()
	}

	// 监听地址已在 spawn core 之前算好（W3-37：core 端口 = gateway 端口 +1，必须由 gateway 先定）。

	mux := http.NewServeMux()
	h.Routes(mux)
	gws.Routes(mux)
	// gateway 安全/生命周期端点（登录、会话、状态、配置、自启）
	mux.HandleFunc("/gw/session", gwSession(cfgStore))
	mux.HandleFunc("/gw/login", gwLogin(cfgStore))
	mux.HandleFunc("/gw/logout", gwLogout())
	mux.HandleFunc("/gw/state", gwStateHandler(cfgStore, listenAddr))
	mux.HandleFunc("/gw/config", gwConfigHandler(cfgStore, notifyRules))
	// 规则重载入口（W3-37 定案 3）：面板改完 rules 段（写盘走 /core/config/rules 反代）后
	// 通知 gateway 重新向 core 拉一次生效规则表，不必重启 gateway。
	mux.HandleFunc("/gw/rules/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			jsonErr(w, http.StatusMethodNotAllowed, "方法不允许")
			return
		}
		n, err := reloadRules(coreBase)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		jsonOut(w, map[string]any{"ok": true, "rules": n})
	})

	px := &proxy.Proxy{
		CoreURL: proxy.ParseURL(coreBase),
		ExtURL:  func(id string) *url.URL { return nil }, // 插件自定义 UI 暂未启用
	}
	// Web UI 静态资产已改由 core 提供（W3-37 定案 1）：未命中更具体前缀的路径一律透传 core。
	// 注意：/v1、/gw、/core、/ext、/healthz、/status 在 mux 上是更具体的模式，不会被这里吞掉。
	mux.Handle("/core/", px.Handler())
	mux.Handle("/ext/", px.Handler())
	mux.Handle("/", px.Handler())

	// 健康探测端点（旧版 `GET /healthz` 无鉴权、`GET /status` 带鉴权）。
	// gateway 是唯一对外 origin，外部探活不应该需要 admin 口令，所以两者都豁免鉴权。
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprint(w, `{"ok":true}`)
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		b, _ := json.Marshal(map[string]any{
			"ok": true, "gateway": gatewayVersion, "core": coreBase, "listen": listenAddr,
		})
		w.Write(b)
	})

	// 最外层再包 DrainBody：把 handler 没读完的请求体丢掉，否则 net/http 会在回包后直接关连接
	// （客户端 `ConnectionResetError 10054`，前端也会命中）。
	handler := hostCheck(listenAddr, gw.DrainBody(adminAuth(func() string { return cfgStore.Get().AdminPass }, mux)))
	srv := &http.Server{Addr: listenAddr, Handler: handler}
	l.Infof("监听 http://%s/ （core 管理面 %s）", listenAddr, coreBase)

	// 先确认端口能监听起来，再挂托盘：端口被占用时不应该先看到一个托盘图标再消失。
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fatal("监听 %s 失败: %v", listenAddr, err)
	}

	// HTTP 在后台跑；主线程留给托盘消息循环（与旧版 wild-work.exe 一致）。
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		l.Infof("收到信号，退出")
		tray.Quit(3 * time.Second)
		shutdown(coreIPC)
		os.Exit(0)
	}()

	if *noTray {
		// 无头模式：服务/无桌面环境（SSH、Windows 服务、CI）
		if err := <-serveErr; err != nil && err != http.ErrServerClosed {
			fatal("HTTP 服务退出: %v", err)
		}
		shutdown(coreIPC)
		return
	}

	// 托盘（阻塞）。无桌面环境下 energye/systray 可能 panic，捕获后提示改用 --no-tray。
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "托盘初始化失败: %v\n请改用 --no-tray 以无头模式运行\n", r)
				closeTray()
			}
		}()
		tray.Run(tray.Icon(), "wild-work3 — 渠道聚合网关", tray.Actions{
			OpenUI: func() { openBrowser(uiURL(listenAddr)) },
			OpenLog: func() {
				if err := openPath(cli.LogPath(data, "gateway")); err != nil {
					l.Errorf("打开日志失败: %v", err)
				}
			},
			Quit: func() {
				if askYesNo("wild-work3", "确定退出 wild-work3 吗？") {
					// 先摘托盘图标（NIM_DELETE 由托盘消息循环执行），直接 os.Exit 会留下幽灵图标。
					tray.Quit(3 * time.Second)
				}
			},
		})
	}()

	// Run 返回 = 托盘图标已回收（或被 --no-tray 以外的原因退出），停机清理。
	shutdown(coreIPC)
}

// 进程级日志（nil 时静默）。
var l *cli.Logger

func shutdown(ci *ipc.CoreIPC) {
	ci.Stop()
}

// runChat 命令行直接对话：走 gateway→core 的 IPC（与 /v1/chat/completions 完全同源）。
func runChat(coreIPC *ipc.CoreIPC, coreBase, model, prompt string) {
	if model == "" {
		// 从 core 管理面取第一个启用渠道的第一个模型
		for _, ch := range fetchChannels(coreBase) {
			if !ch.Enabled || ch.Models == 0 {
				continue
			}
			if ms := channelModels(coreBase, ch.ChannelID); len(ms) > 0 {
				model = ms[0]
				break
			}
		}
		if model == "" {
			fatal("没有可用模型：请先配置渠道，或用 --model 指定")
		}
	}
	req := chat.GenerateRequest{
		Model:          model,
		Stream:         true,
		Messages:       []chat.ChatMessage{{Role: "user", Content: prompt}},
		ConversationID: "gw-cli",
	}
	l.Infof("chat 通过 IPC 调用 core: model=%s", model)
	var text strings.Builder
	usage, fallbackFrom, err := coreIPC.ChatStream(context.Background(), &req, func(name string, data json.RawMessage) error {
		var f map[string]any
		_ = json.Unmarshal(data, &f)
		switch name {
		case "text.delta", "reasoning.delta":
			s, _ := f["content"].(string)
			text.WriteString(s)
			os.Stdout.WriteString(s)
		case "tool_call.delta":
			l.Infof("  ← tool_call.delta %s", compactJSON(data))
		case "usage":
			l.Infof("  ← usage %s", compactJSON(data))
		case "error":
			l.Errorf("  ← error %s", compactJSON(data))
		}
		return nil
	})
	if err != nil {
		l.Errorf("chat 失败: %v", err)
		fail("chat 失败: %v", err)
	}
	if fallbackFrom != "" {
		l.Infof("[降级] %s -> %s", fallbackFrom, model)
	}
	if usage != nil {
		fmt.Fprintf(os.Stderr, "\n[结果] tokens=%d/%d 耗时=%dms 首字=%dms\n",
			usage.PromptTokens, usage.CompletionTokens, usage.TimeMs, usage.FirstTokenMs)
	} else {
		fmt.Fprintln(os.Stderr)
	}
}

// runStatus 分层探活：core IPC → core 管理面 → gateway 版本。
func runStatus(coreIPC *ipc.CoreIPC, coreBase string) {
	out := map[string]any{"gateway": gatewayVersion, "protocol": 1, "core_base": coreBase}
	if cli, err := coreIPC.Client(context.Background()); err != nil {
		out["ipc_error"] = err.Error()
	} else if res, _, err := cli.Invoke(context.Background(), "ping", nil, nil); err != nil {
		out["ipc_error"] = err.Error()
	} else {
		out["ipc"] = compactJSON(res.Result)
	}
	c := &http.Client{Timeout: 3 * time.Second}
	// core 刚随 IPC 启动时 HTTP 监听可能稍滞后，短重试避免假阴性
	var lastErr error
	for i := 0; i < 10; i++ {
		resp, err := c.Get(coreBase + "/api/health")
		if err != nil {
			lastErr = err
			time.Sleep(300 * time.Millisecond)
			continue
		}
		var v any
		_ = json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		out["core_health"] = v
		lastErr = nil
		break
	}
	if lastErr != nil {
		out["core_health_error"] = lastErr.Error()
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	coreIPC.Stop()
}

// runModels 经 core 管理面列出模型。
func runModels(coreBase string) {
	chans := fetchChannels(coreBase)
	out := map[string]any{}
	for _, ch := range chans {
		if !ch.Enabled || ch.Models == 0 {
			continue
		}
		out[ch.ChannelID] = channelModels(coreBase, ch.ChannelID)
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

// channelBrief core 管理面裸渠道数组（只取本 CLI 需要的字段）。
type channelBrief struct {
	ChannelID string `json:"channel_id"`
	Enabled   bool   `json:"enabled"`
	Models    int    `json:"models"`
}

// channelModels 取某渠道的模型名列表（core 管理面 /api/channels/{id}）。
func channelModels(coreBase, id string) []string {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(coreBase + "/api/channels/" + url.PathEscape(id))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var detail struct {
		Models []string `json:"models"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&detail)
	return detail.Models
}

func fetchChannels(coreBase string) []channelBrief {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(coreBase + "/api/channels")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var list []channelBrief
	_ = json.NewDecoder(resp.Body).Decode(&list)
	return list
}

func compactJSON(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return string(raw)
	}
	if s, ok := v.(string); ok {
		return s // 字符串结果（如 ping → "pong"）直接返回，不要多一层引号
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// reloadRules 从 core 拉「生效规则表」（默认表内嵌 core + 用户 config.json rules 段覆盖后的结果），
// 热替换 Pass A 引擎（W3-37 定案 3：gateway 不内嵌默认表，只消费 core 下发数据）。
//
// 失败时保留旧引擎（不阻断请求，也不让已生效的脱敏静默消失）。
func reloadRules(coreBase string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, coreBase+"/api/config/rules", nil)
	if err != nil {
		return 0, err
	}
	resp, err := rulesHTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("core 回 %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, err
	}
	n, err := v1.SetRulesFromEffective(body)
	if err != nil {
		return 0, err
	}
	if l != nil {
		l.Infof("Pass A 规则已从 core 重载：%d 条", n)
	}
	return n, nil
}

// rulesHTTP 拉规则表用的客户端（core 冷启动/重启时会给反馈，靠 rulesLoop 退避重试而不是单次长超时）。
var rulesHTTP = &http.Client{Timeout: 8 * time.Second}

// rulesPoll 通知规则循环立即重拉（core 重启后面板保存后）；容量 1，满了说明已排隊。
var rulesPoll = make(chan struct{}, 1)

// notifyRules 非阻塞投递重拉信号。
func notifyRules() {
	select {
	case rulesPoll <- struct{}{}:
	default:
	}
}

// rulesLoop 后台维住 Pass A 规则表：启动后重试到首次成功，之后 TTL 兑底 + 事件驱动重拉。
//
// TTL 的意义：面板只改 rules 不叫 reload 时（或 gateway 比 core 先起、core 后来才升级），
// 靠定时重拉保证最终一致；拉不到就 Pass A 保持 nil（入站归一化仍生效）。
func rulesLoop(ctx context.Context, coreBase string) {
	const ttl = 5 * time.Minute
	wait := 300 * time.Millisecond
	ready := false
	for {
		n, err := reloadRules(coreBase)
		if err == nil {
			if !ready {
				ready = true
				l.Infof("Pass A 规则就绪：%d 条（默认表由 core 下发，TTL %v）", n, ttl)
			} else {
				l.Infof("Pass A 规则已刷新：%d 条", n)
			}
			wait = 300 * time.Millisecond
		} else {
			if ready {
				l.Errorf("重拉 Pass A 规则失败（保留旧引擎，%v 后重试）: %v", wait, err)
			} else {
				l.Debugf("core 规则表未就绪（%v 后重试）: %v", wait, err)
			}
			ready = false
			if wait *= 2; wait > 5*time.Second {
				wait = 5 * time.Second
			}
		}
		sel := time.After(ttl)
		if !ready {
			sel = time.After(wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-sel:
		case <-rulesPoll:
			ready = false // 事件驱动：允许下一轮重新计成功日志
		}
	}
}

// hostCheck 防 DNS rebinding（W3-16）：只接受本机 Host。
func hostCheck(listen string, next http.Handler) http.Handler {
	host, _, _ := net.SplitHostPort(listen)
	loopback := host == "127.0.0.1" || host == "localhost" || host == "::1"
	if !loopback {
		return next // 显式监听非本机时不校验（用户自担）
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Host
		if hp, _, err := net.SplitHostPort(h); err == nil {
			h = hp
		}
		switch h {
		case "127.0.0.1", "localhost", "::1", "[::1]":
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "invalid host", http.StatusForbidden)
		}
	})
}

// gatewayConfig gateway 自身配置（安全与生命周期；业务策略在 config.json 的 core 段）。
type gatewayConfig struct {
	Listen     string `json:"listen,omitempty"`
	APIKey     string `json:"api_key"`
	AdminPass  string `json:"admin_password"`
	CoreListen string `json:"core_listen,omitempty"` // core 管理面监听（缺省 = 对外端口 +1）

	// 分发与更新（W3-48 v2）：注册表仓 / 分支 / 代理 / 缓存时长。
	// 一律「CLI > env > 本段 > 内置默认」，且 CLI/env 不落盘。
	RegistryRepo    string   `json:"registry_repo,omitempty"`
	RegistryRef     string   `json:"registry_ref,omitempty"`
	DownloadProxies []string `json:"download_proxies,omitempty"`
	IndexTTLMin     int      `json:"index_ttl_min,omitempty"`

	// GitHubBase/CodeloadBase 发布页与 tarball 的基址（生产留空 = https://github.com / codeload）。
	// 只给离线夹具与 mock 上游用：e2e 要覆盖「按版本号定位发布包」就必须能把发布页指到本机。
	GitHubBase   string `json:"github_base,omitempty"`
	CodeloadBase string `json:"codeload_base,omitempty"`
}

// defaultGWListen gateway 缺省对外监听（core 缺省听 7864 = 此端口 +1）。
const defaultGWListen = "127.0.0.1:7863"

// coreAddrFor 算 core 的管理面监听地址（W3-37 定案 2）。
//
// 优先级：gateway 段 core_listen > gateway 生效端口 +1。地址一律由 gateway 算好后 --listen 下发，
// gateway 不再读 core 的配置段；core 侧另有 forceLoopback 傅底（只听回环）。
func coreAddrFor(c gatewayConfig, gwListen string) string {
	if c.CoreListen != "" {
		return c.CoreListen
	}
	if gwListen == "" {
		gwListen = c.Listen
	}
	if gwListen == "" {
		gwListen = defaultGWListen
	}
	host, port, err := net.SplitHostPort(gwListen)
	if err != nil {
		return "127.0.0.1:7864"
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return "127.0.0.1:7864"
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		host = "127.0.0.1" // core 只听回环（对外只经 gateway 反代）
	}
	return net.JoinHostPort(host, strconv.Itoa(n+1))
}

// newCoreIPC 按统一形状构造 core 子进程（serve + --data/--plugins/--listen/--gw-listen）。
//
// gwListen 只是让 core 在浏览器直连它自己的端口时能提示「请访问 gateway 端口」；
// core 拿不到它也能正常工作（退化为不做提示）。
func newCoreIPC(exe, data, plugins, coreAddr, gwListen string) *ipc.CoreIPC {
	args := []string{"serve", "--data", filepath.Join(data, "core"), "--plugins", plugins, "--listen", coreAddr}
	if gwListen != "" {
		args = append(args, "--gw-listen", gwListen)
	}
	return ipc.New(exe, args...)
}

// loadGWSection 给 CLI 分支用：读指定数据目录的 gateway 段（不建热更新存储）。
func loadGWSection(dataDir string) gatewayConfig {
	return loadGatewaySection(appconfig.OpenData(dataDir))
}

// distFor 组装分发配置（W3-48 v2）：优先级 CLI > 配置段 > 内置默认。
//
// CLI/env 不落盘（W3-35 同一口径），所以这里只算本次生效值。
func distFor(c gatewayConfig, registryRepo, registryRef string, proxies []string) gw.Dist {
	prx := append([]string{}, firstNonEmptyList(proxies, c.DownloadProxies, defaultProxies)...)
	return gw.Dist{
		Registry:    sourceOf(firstNonEmpty(registryRepo, c.RegistryRepo, defaultRegistryRepo), firstNonEmpty(registryRef, c.RegistryRef, defaultRegistryRef)),
		Proxies:     prx,
		ProtocolVer: protocolVersion,
		TTL:         time.Duration(c.IndexTTLMin) * time.Minute,
		Fetch: registry.Options{
			GitHubBase:   c.GitHubBase,
			CodeloadBase: c.CodeloadBase,
		},
		// 基址被改写时（离线夹具/mock 上游）它的主机也要进下载允许列表，否则定位出的 URL 会被 allow 过滤掉。
		ExtraHosts: hostOfList(c.GitHubBase, c.CodeloadBase),
	}
}

// sourceOf 把配置值解释成注册表来源：含 `://` 当绝对 tarball URL（离线夹具/mock 上游），
// 否则当 owner/repo（走 codeload tarball）。
func sourceOf(val, ref string) registry.Source {
	if strings.Contains(val, "://") {
		return registry.Source{URL: val, Name: "official"}
	}
	return registry.Source{Repo: val, Ref: ref, Name: "official"}
}

// hostOfList 取一组 URL 的主机名（给 Dist.ExtraHosts 用；空串与非法值跳过）。
func hostOfList(urls ...string) []string {
	var out []string
	for _, s := range urls {
		if u, err := url.Parse(strings.TrimSpace(s)); err == nil && u.Hostname() != "" {
			out = append(out, u.Hostname())
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func firstNonEmptyList(vals ...[]string) []string {
	for _, v := range vals {
		if len(v) > 0 {
			return v
		}
	}
	return nil
}

// absPath 转绝对路径（更新要拿它做 rename 目标，相对路径在子进程 cwd 下会指错地方）。
func absPath(p string) string {
	if p == "" {
		return ""
	}
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// selfExe 当前可执行文件绝对路径（仅用于「gateway 需手动升级」提示里写清要换哪个文件）。
func selfExe() string {
	if p, err := os.Executable(); err == nil {
		return absPath(p)
	}
	return absPath(os.Args[0])
}

func coreVersionHTTP(base string) string {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(base + "/api/health")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var v struct {
		Version map[string]string `json:"version"`
	}
	if json.NewDecoder(resp.Body).Decode(&v) != nil {
		return ""
	}
	return v.Version["core"]
}

func defaultDataDir() string {
	if d := os.Getenv("WW3_DATA"); d != "" {
		return d
	}
	exe, _ := os.Executable()
	return filepath.Join(filepath.Dir(exe), "data")
}

func defaultPluginsDir() string {
	if d := os.Getenv("WW3_PLUGINS"); d != "" {
		return d
	}
	exe, _ := os.Executable()
	return filepath.Join(filepath.Dir(exe), "plugins")
}

func defaultCoreExe() string {
	exe := "ww3-core"
	if strings.Contains(os.Getenv("OS"), "Windows_NT") {
		exe = "ww3-core.exe"
	}
	self, _ := os.Executable()
	cand := filepath.Join(filepath.Dir(self), exe)
	if _, err := os.Stat(cand); err == nil {
		return cand
	}
	if p, err := exec.LookPath(exe); err == nil {
		return p
	}
	return cand
}

func fail(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	l.Errorf("%s", msg)
	fmt.Fprintf(os.Stderr, "[gateway] %s\n", msg)
	os.Exit(1)
}

func fatal(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	l.Errorf("%s", msg)
	fmt.Fprintf(os.Stderr, "[gateway] %s\n", msg)
	os.Exit(1)
}

// ---- 托盘/平台小工具（避免在 main 里散落平台代码）----

// closeTray 是托盘 panic 后的收尾占位（目前只需确保图标不残留）。
func closeTray() { tray.Quit(2 * time.Second) }

// uiURL 把监听地址换成浏览器可访问的地址（0.0.0.0/* 时回退 127.0.0.1）。
func uiURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen + "/"
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

// openBrowser 用系统默认浏览器打开地址。
func openBrowser(u string) {
	cmd := exec.Command("cmd", "/c", "start", "", u)
	_ = cmd.Start()
}

// openPath 用系统默认程序打开文件/目录。
func openPath(p string) error {
	if p == "" {
		return fmt.Errorf("路径为空")
	}
	if _, err := os.Stat(p); err != nil {
		return err
	}
	return exec.Command("cmd", "/c", "start", "", p).Start()
}

// askYesNo 弹是/否框（Windows 走 MessageBox；其他平台退化为直接确认）。
func askYesNo(title, msg string) bool { return askYesNoImpl(title, msg) }
