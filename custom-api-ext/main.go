// custom-api-ext：自定义 OpenAI 兼容 API 渠道插件（渠道插件样板，W3-20/21）。
//
// 命令行形态（契约见 docs/PLUGIN_CLI.md）：
//
//	custom-api-ext.exe <command> --data <dir> [--switch value ...]
//
//	call      协议本体：stdin 读一帧 invoke → 事件 + result → 退出（core 的调用方式）
//	stdio     协议本体：JSONL RPC 循环直到 EOF（自测/调试）
//	manifest  输出 manifest.json
//	ping      控制面逐项自检
//	models    拉模型列表
//	chat      直接对话，正文打到 stdout
//
// 便利命令一律 spawn 自身 call 子进程走真实 IPC，不存在「CLI 专用捷径」（AGENTS §S）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"wild-work3/main/custom-api-ext/internal/channel"
	"wild-work3/main/protocol/chat"
	"wild-work3/main/protocol/cli"
	"wild-work3/main/protocol/plugin"
)

func main() {
	app := cli.NewApp("custom-api-ext", "自定义 OpenAI 兼容 API 渠道插件", "call")

	// ---- 协议本体 ----
	cCall := app.Cmd("call", "协议本体：一次性执行一条 invoke（core 的调用方式）")
	pCall := cCall.Str("data", "", "插件数据目录（缺省 $WW3_DATA，再缺省 exe 目录）")
	cStdio := app.Cmd("stdio", "协议本体：JSONL RPC 循环直到 stdin EOF（自测/调试）")
	pStdio := cStdio.Str("data", "", "插件数据目录")
	cManifest := app.Cmd("manifest", "输出 manifest.json")
	pManOut := cManifest.Str("out", "", "写到文件（缺省打到 stdout）")

	// ---- 便利命令 ----
	cPing := app.Cmd("ping", "控制面逐项自检")
	nPing, _ := addNetFlags(cPing)
	cModels := app.Cmd("models", "拉取模型列表")
	nModels, _ := addNetFlags(cModels)
	cChat := app.Cmd("chat", "发起一次对话，正文打到 stdout")
	nChat, _ := addNetFlags(cChat)
	chatModel := cChat.Str("model", os.Getenv("WW3_MODEL"), "模型名")
	chatPrompt := cChat.Str("prompt", "你好，请用一句话自我介绍", "提示词")
	chatRaw := cChat.Str("raw", "", "直接给出 chat.generate 的 JSON 请求体（调试用，覆盖 --prompt）")

	cmd, err := app.Parse(os.Args[1:])
	if err != nil {
		if err == cli.ErrHelp {
			return
		}
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	var dataFlag string
	switch cmd.CmdName() {
	case "call":
		dataFlag = *pCall
	case "stdio":
		dataFlag = *pStdio
	case "manifest":
		dataFlag = ""
	case "ping":
		dataFlag = *nPing.data
	case "models":
		dataFlag = *nModels.data
	case "chat":
		dataFlag = *nChat.data
	}
	data := cli.ResolveData(dataFlag)
	channel.Log, _ = cli.NewLogger("custom-api", cli.LogPath(data, "custom-api"))
	if channel.Log != nil {
		defer channel.Log.Close()
	}

	switch cmd.CmdName() {
	case plugin.ModeCall, plugin.ModeStdio:
		// 协议本体：call 由 core 用；stdio 供自测/调试反复调用同一进程。
		if err := channel.Serve(context.Background(), cmd.CmdName(), data, os.Stdin, os.Stdout); err != nil {
			fail("%s 退出: %v", cmd.CmdName(), err)
		}
	case "manifest":
		b, _ := json.MarshalIndent(channel.Manifest(), "", "  ")
		if *pManOut != "" {
			if err := os.WriteFile(*pManOut, append(b, '\n'), 0o644); err != nil {
				fail("写 %s 失败: %v", *pManOut, err)
			}
			fmt.Fprintf(os.Stderr, "已写出 %s\n", *pManOut)
			return
		}
		fmt.Println(string(b))
	case "ping":
		runPing(data, nPing.cfg())
	case "models":
		runModels(data, nModels.cfg())
	case "chat":
		runChat(data, nChat.cfg(), *chatModel, *chatPrompt, *chatRaw)
	default:
		fail("未知命令: %s", cmd.CmdName())
	}
}

// netFlags 是 §3 契约网络开关（--config/--base-url/--api-key/--proxy/--timeout）。
type netFlags struct {
	data    *string
	config  *string
	baseURL *string
	apiKey  *string
	proxy   *string
	timeout *int
}

func addNetFlags(c *cli.Command) (*netFlags, *netFlags) {
	n := &netFlags{
		data:    c.Str("data", "", "插件数据目录（缺省 $WW3_DATA，再缺省 exe 目录）"),
		config:  c.Str("config", "", "渠道配置 JSON，或 @文件路径"),
		baseURL: c.Str("base-url", os.Getenv("WW3_BASE_URL"), "端点基址，如 https://api.openai.com/v1"),
		apiKey:  c.Str("api-key", os.Getenv("WW3_API_KEY"), "API Key"),
		proxy:   c.Str("proxy", os.Getenv("WW3_PROXY"), "出站代理，如 http://127.0.0.1:10809"),
	}
	n.timeout = c.Int("timeout", 300, "上游超时（秒）")
	return n, n
}

// cfg 组装渠道配置：--config 为底，其余显式开关覆盖。
func (n *netFlags) cfg() map[string]any {
	cfg := map[string]any{}
	if s := *n.config; s != "" {
		if err := json.Unmarshal([]byte(cli.ConfigArg(s)), &cfg); err != nil {
			fail("--config 解析失败: %v", err)
		}
	}
	if *n.baseURL != "" {
		cfg["base_url"] = *n.baseURL
	}
	if *n.apiKey != "" {
		cfg["api_key"] = *n.apiKey
	}
	if *n.proxy != "" {
		cfg["proxy"] = *n.proxy
	}
	if *n.timeout > 0 {
		cfg["timeout"] = *n.timeout
	}
	return cfg
}

// runChat 命令行直接对话：spawn 自身 call 子进程，走与 core 完全一致的 IPC 路径。
func runChat(data string, cfg map[string]any, model, prompt, rawBody string) {
	req := chat.GenerateRequest{Model: model, Stream: true}
	if rawBody != "" {
		if err := json.Unmarshal([]byte(rawBody), &req); err != nil {
			fail("--raw 解析失败: %v", err)
		}
	} else {
		req.Messages = []chat.ChatMessage{{Role: "user", Content: prompt}}
	}
	req.ChannelConfig, _ = json.Marshal(cfg)
	req.ConversationID = fmt.Sprintf("cli-%d", os.Getpid())

	p := spawnSelf(data, plugin.ModeCall)
	defer p.Close()
	p.Out = os.Stdout // 正文实时输出到 stdout
	res, err := p.Invoke(context.Background(), cli.Step{Method: "chat.generate", Params: req, Stream: true})
	if err != nil {
		fail("chat.generate 失败: %v", err)
	}
	// 汇总行打到 stderr（stdout 保留纯正文，便于管道处理）
	fmt.Fprintf(os.Stderr, "\n[结果] ok=%v finish=%s tokens=%d/%d 耗时=%dms 首字=%dms\n",
		res.OK, res.Finish, res.UsageInt("prompt_tokens"), res.UsageInt("completion_tokens"),
		res.UsageInt("time_ms"), res.UsageInt("first_token_ms"))
	if !res.OK {
		os.Exit(2)
	}
}

// runModels 命令行拉取模型列表（走 call 形态的 models.list）。
func runModels(data string, cfg map[string]any) {
	p := spawnSelf(data, plugin.ModeCall)
	defer p.Close()
	res, err := p.InvokeSimple(context.Background(), "models.list", map[string]any{"config": cfg})
	if err != nil {
		fail("拉取模型失败: %v", err)
	}
	fmt.Println(res.Raw)
}

// runPing 命令行自检：控制面逐项调用（每项都是一次性进程，与生产一致）。
func runPing(data string, cfg map[string]any) {
	probes := []string{"initialize", "capabilities.list", "config.schema", "models.list", "accounts.list", "credits.get", "stats.get"}
	for _, m := range probes {
		p := spawnSelf(data, plugin.ModeCall)
		var params any
		if m == "models.list" || m == "stats.get" {
			params = map[string]any{"config": cfg}
		}
		r, err := p.InvokeSimple(context.Background(), m, params)
		p.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %s: %v\n", m, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "✓ %s → %s\n", m, truncateStr(r.Raw, 300))
	}
}

// spawnSelf 启动自身 call 子进程并接成 JSONL RPC。
// --data 必须显式传递：否则子进程会按 $WW3_DATA / exe 目录解析，与父进程不一致（日志/凭据分裂）。
func spawnSelf(data, mode string) *cli.Proc {
	exe, err := os.Executable()
	if err != nil {
		fail("定位自身可执行文件失败: %v", err)
	}
	args := []string{mode}
	if data != "" {
		args = append(args, "--data", data)
	}
	p, err := cli.Spawn("custom-api-"+mode, exe, args, channel.Log)
	if err != nil {
		fail("启动子进程失败: %v", err)
	}
	return p
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
