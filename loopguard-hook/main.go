// loopguard-hook：思考链死循环（发癫）拦截器插件。
//
// 用法（W3-21：命令式 + 双短横线）：
//
//	loopguard-hook [call|stdio|hash|guard|ping|manifest] [--switch value]
//
// 缺省命令是 call（core 调用插件的方式）：stdin 读一帧 invoke，写事件与 result，退出。
//
// 拦截器与渠道插件不同：它不产生内容，只**处理**上游已经产生的事件流（双向流 hook.stream，
// 见 W3-26）。本插件的判定算法在 internal/guard，协议胶水在 internal/hook。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"wild-work3/main/loopguard-hook/internal/guard"
	"wild-work3/main/loopguard-hook/internal/hook"
	"wild-work3/main/protocol/cli"
	"wild-work3/main/protocol/jsonl"
)

func main() {
	app := cli.NewApp("loopguard-hook", "思考链死循环（发癫）拦截器插件", "call")

	// ---- 协议契约命令（protocol/docs/PLUGIN_CLI.md §2）----
	pCallData := app.Cmd("call", "一次性：读一帧 invoke，写事件与 result 后退出（core 的调用方式）").Str("data", "", "插件数据目录（缺省 $WW3_DATA，否则可执行文件所在目录）")
	pStdioData := app.Cmd("stdio", "常连：在 stdin/stdout 上跑 JSONL RPC 循环直到 EOF").Str("data", "", "插件数据目录")
	pManifestOut := app.Cmd("manifest", "输出 manifest.json（打包插件目录用）").Str("out", "", "写入文件（缺省打印到 stdout）")

	// ---- 插件自定义便利命令 ----
	pPingData := app.Cmd("ping", "控制面自检（每个方法起一个一次性子进程，与 core 调用方式一致）").Str("data", "", "插件数据目录")

	// guard：离线跑一遍判定，便于回归与调参（stdin 读文本或 --file）。
	cGuard := app.Cmd("guard", "离线检测一段文本是否触发守卫（调试/回归用）")
	pGuardFile := cGuard.Str("file", "", "从文件读取文本（缺省读 stdin）")
	pGuardRaw := cGuard.Bool("raw", false, "输出完整命中详情 JSON")
	pGuardStreak := cGuard.Int("streak", 0, "覆盖同行连续重复阈值")
	pGuardMinChars := cGuard.Int("min-chars", 0, "覆盖计入复读的最短行长")

	cmd, err := app.Parse(os.Args[1:])
	if err != nil {
		if err == cli.ErrHelp {
			return
		}
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	dataDir := cli.ResolveData(*pCallData)
	switch cmd.CmdName() {
	case "call":
		dataDir = cli.ResolveData(*pCallData)
	case "stdio":
		dataDir = cli.ResolveData(*pStdioData)
	case "ping":
		dataDir = cli.ResolveData(*pPingData)
	}
	_ = dataDir

	switch cmd.CmdName() {
	case "call", "stdio":
		// call 形态也要初始化日志（core 联调常常强杀进程，日志必须落盘取证）。
		d := hook.New()
		if err := serve(d, cmd.CmdName(), dataDir); err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
	case "manifest":
		b, _ := json.MarshalIndent(hook.Manifest(), "", "  ")
		if *pManifestOut != "" {
			if err := os.WriteFile(*pManifestOut, b, 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "写 manifest 失败: %v\n", err)
				os.Exit(1)
			}
			return
		}
		fmt.Println(string(b))
	case "ping":
		runPing(dataDir)
	case "guard":
		runGuard(*pGuardFile, *pGuardRaw, *pGuardStreak, *pGuardMinChars)
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n", cmd.CmdName())
		os.Exit(2)
	}
}

// serve 按运行形态跑协议本体。
func serve(d *hook.Daemon, mode, dataDir string) error {
	_ = dataDir
	log := newLogger(dataDir)
	defer log.Close()
	r := d.Registry()
	if mode == "call" {
		return r.ServeOnce(context.Background(), os.Stdin, os.Stdout)
	}
	return r.ServeStdio(context.Background(), os.Stdin, os.Stdout)
}

// newLogger 日志落 <data>/logs/loopguard.log 并同步 stderr（stdout 永远只走协议）。
func newLogger(dataDir string) *cli.Logger {
	lg, err := cli.NewLogger("loopguard", cli.LogPath(dataDir, "loopguard"))
	if err != nil {
		return &cli.Logger{}
	}
	return lg
}

// runPing 控制面自检：逐方法起一次性子进程（与 core 调用方式一致，无 CLI 捷径）。
func runPing(dataDir string) {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "取可执行路径失败: %v\n", err)
		os.Exit(1)
	}
	log := newLogger(dataDir)
	defer log.Close()
	p, err := cli.Spawn("loopguard", self, []string{"call", "--data", dataDir}, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "启动子进程失败: %v\n", err)
		os.Exit(1)
	}
	defer p.Close()
	ctx := context.Background()
	if err := p.Handshake(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "握手失败: %v\n", err)
		os.Exit(1)
	}
	steps := []cli.Step{
		{Method: "ping"},
		{Method: "config.schema"},
		{Method: "config.get"},
		{Method: "health.check"},
	}
	fail := 0
	for _, s := range steps {
		r, err := p.InvokeSimple(ctx, s.Method, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[✗] %-16s %v\n", s.Method, err)
			fail++
			continue
		}
		fmt.Fprintf(os.Stderr, "[✓] %-16s %s\n", s.Method, clip(string(r.Raw), 90))
	}
	if fail > 0 {
		os.Exit(1)
	}
}

// runGuard 离线判定（stdin 或 --file），用于回归与阈值调参。
func runGuard(file string, raw bool, streak, minChars int) {
	var b []byte
	var err error
	if file != "" {
		b, err = os.ReadFile(file)
	} else {
		b, err = readAll(os.Stdin)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取文本失败: %v\n", err)
		os.Exit(1)
	}
	cfg := guard.DefaultConfig
	if streak > 0 {
		cfg.StreakLimit = streak
	}
	if minChars > 0 {
		cfg.MinLineChars = minChars
	}
	g := guard.New(cfg)
	// 按小块喂入，模拟真实流式分片（单次整段喂会掩盖跨分片的状态 bug）。
	text := string(b)
	const chunk = 7
	var hit *guard.Hit
	for i := 0; i < len(text) && hit == nil; i += chunk {
		end := i + chunk
		if end > len(text) {
			end = len(text)
		}
		hit = g.Push(text[i:end])
	}
	if hit == nil {
		hit = g.Mark()
	}
	if hit == nil {
		fmt.Fprintln(os.Stdout, "[结果] 未命中")
		return
	}
	if raw {
		js, _ := json.MarshalIndent(hit, "", "  ")
		fmt.Fprintln(os.Stdout, string(js))
		return
	}
	fmt.Fprintf(os.Stdout, "[结果] 命中 kind=%s reason=%s\n", hit.Kind, hit.Reason)
}

func readAll(f *os.File) ([]byte, error) {
	var sb strings.Builder
	buf := make([]byte, 32<<10)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			if err.Error() == "EOF" {
				return []byte(sb.String()), nil
			}
			return []byte(sb.String()), nil
		}
	}
}

func clip(s string, n int) string {
	rs := []rune(strings.TrimSpace(s))
	if len(rs) <= n {
		return string(rs)
	}
	return string(rs[:n]) + "…"
}

// 保留 jsonl 依赖（manifest 与事件都用同一套帧类型）。
var _ = jsonl.OpEvent
