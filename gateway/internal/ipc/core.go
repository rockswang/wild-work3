// Package ipc 管理与 core 的 stdio 会话（gateway ↔ core 的 chat.generate 通道）。
package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"wild-work3/main/protocol/chat"
	"wild-work3/main/protocol/jsonl"
	"wild-work3/main/protocol/rpc"
)

// 重启退避参数：异常退出后 1s 起、每次翻倍、封顶 30s；存活超过 60s 则重新从 1s 起算。
const (
	restartMin   = 1 * time.Second
	restartMax   = 30 * time.Second
	restartReset = 60 * time.Second
)

// CoreIPC 持有 core 子进程与其 RPC 客户端，并负责异常退出的重启（W3-37 附带修复 1，P0）。
type CoreIPC struct {
	mu        sync.Mutex
	exe       string
	args      []string
	cli       *rpc.Client
	cmd       *exec.Cmd
	dead      chan struct{} // 当前进程退出后关闭；nil = 未运行
	stopped   bool          // 主动 Stop 后不再自动重启
	backoff   time.Duration
	alive     time.Time
	notBefore time.Time                     // 退避窗口内拒绝重新拉起
	lastExit  error                         // 最近一次异常退出原因
	onState   func(format string, a ...any) // 状态回调（日志），可为 nil
}

// New 创建（不启动）。
func New(exe string, args ...string) *CoreIPC {
	return &CoreIPC{exe: exe, args: args, backoff: restartMin}
}

// OnState 设置状态回调（重启/退出事件写日志用）。
func (c *CoreIPC) OnState(fn func(format string, a ...any)) {
	c.mu.Lock()
	c.onState = fn
	c.mu.Unlock()
}

func (c *CoreIPC) statef(format string, a ...any) {
	c.mu.Lock()
	fn := c.onState
	c.mu.Unlock()
	if fn != nil {
		fn(format, a...)
	}
}

// Start 启动 core 并建立 IPC 连接（已存活则幂等）。
func (c *CoreIPC) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil && c.dead != nil {
		select {
		case <-c.dead: // 已退出，继续往下重新拉起
		default:
			return nil // 仍在运行
		}
	}
	if c.stopped {
		return fmt.Errorf("core 已停止（主动 Stop 后不自动重启）")
	}
	if time.Now().Before(c.notBefore) {
		return fmt.Errorf("core 重启退避中（%.0fs 后可重试）: %w", time.Until(c.notBefore).Seconds(), c.lastExit)
	}
	cmd := exec.Command(c.exe, c.args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 core 失败: %w", err)
	}
	dead := make(chan struct{})
	c.cmd, c.dead, c.alive = cmd, dead, time.Now()
	c.cli = rpc.NewClient(stdin, stdout)
	go func() {
		werr := cmd.Wait()
		c.mu.Lock()
		stopped, cur := c.stopped, c.cmd == cmd
		if cur {
			c.lastExit, c.cmd, c.cli = werr, nil, nil
		}
		close(dead)
		c.mu.Unlock()
		if stopped || !cur {
			return
		}
		c.statef("core 异常退出: %v（将按退避重启）", werr)
	}()
	return nil
}

// Supervise 后台守护：core 异常退出即退避重启，直到 ctx 取消或主动 Stop。
//
// /core/* 走 HTTP 反代、不碰 IPC，所以只靠 Client() 的惰性重启不足以自愈（P0 根因）。
// 循环里每轮都必须真的调一次 Start：dead 是已关闭的旧通道，不能拿它当「还活着」的判据，
// 否则退避分支会无限重入（实测现象是日志每秒刷「core 将在 1s 后重启」而 core 永不复活）。
func (c *CoreIPC) Supervise(ctx context.Context) {
	for {
		c.mu.Lock()
		dead, stopped := c.dead, c.stopped
		c.mu.Unlock()
		if stopped {
			return
		}
		if dead == nil { // 尚未启动（含 Restart 后 c.cmd/c.dead 已清空的窗口）
			if err := c.Start(ctx); err != nil {
				c.statef("core 拉起失败: %v", err)
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-dead:
		}
		// 走到这里 = core 进程已退出（异常退出或被 Restart 强杀）
		c.mu.Lock()
		if c.stopped {
			c.mu.Unlock()
			return
		}
		if time.Since(c.alive) > restartReset {
			c.backoff = restartMin // 上一代活得够久，说明不是「起来就崩」，退避归零
		}
		wait := c.backoff
		if c.backoff *= 2; c.backoff > restartMax {
			c.backoff = restartMax
		}
		c.notBefore = time.Now().Add(wait) // 这段窗口内 Client() 直接得到可读错误
		c.mu.Unlock()
		c.statef("core 将在 %v 后重启", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait + 10*time.Millisecond): // 多让 10ms，越过 notBefore 闸门
		}
		c.mu.Lock()
		c.notBefore = time.Time{}
		c.mu.Unlock()
		if err := c.Start(ctx); err != nil {
			c.statef("core 重启失败: %v", err)
		}
	}
}

// Client 返回 RPC 客户端（未启动则启动）。
func (c *CoreIPC) Client(ctx context.Context) (*rpc.Client, error) {
	c.mu.Lock()
	cli := c.cli
	c.mu.Unlock()
	if cli != nil && !cli.IsClosed() {
		return cli, nil
	}
	if err := c.Start(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cli, nil
}

// Restart 让监督循环立即重拉 core（插件目录变更后热加载用）。
//
// 与 Stop 的区别：不置 stopped，因此 Supervise 会正常重启；也不发 shutdown（避免走优雅退出路径后
// 还等 2s RPC 往返），直接关管道 + 杀进程。重启间隔由 notBefore 清零保证立即。
func (c *CoreIPC) Restart() {
	c.mu.Lock()
	c.notBefore = time.Time{}
	c.backoff = restartMin
	rc, cmd := c.cli, c.cmd
	c.cli = nil
	c.mu.Unlock()
	if rc != nil {
		rc.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// Stop 关闭 core（主动停止，之后不再自动重启）。
func (c *CoreIPC) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	if c.cli != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _, _ = c.cli.Invoke(ctx, "shutdown", nil, nil)
		c.cli.Close()
		c.cli = nil
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		c.cmd = nil
	}
}

// Halt 停 core 并**等它真退出**（更新替换用）。
//
// Windows 硬约束：进程还活着时 exe 被文件锁占用，连 os.Rename 都会失败（陷阱：替换前必须等句柄释放），
// 所以更新流程不能用 Stop()（它只 Kill 不等），必须拿到退出信号再改名。
// 同时置 stopped=true，让监督循环不要在改名窗口里把旧 core 又拉起来。
func (c *CoreIPC) Halt(timeout time.Duration) error {
	c.mu.Lock()
	c.stopped = true
	dead, cli, cmd := c.dead, c.cli, c.cmd
	c.cli = nil
	c.mu.Unlock()

	if cli != nil {
		// shutdown 尽力而为（core 收到会自己刷盘退出），紧接着还是 Kill 兜底，不指望它准时退
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _, _ = cli.Invoke(ctx, "shutdown", nil, nil)
		cancel()
		cli.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if dead == nil {
		return nil
	}
	select {
	case <-dead:
		c.mu.Lock()
		c.cmd = nil
		c.mu.Unlock()
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("等待 core 退出超时（%.0fs）", timeout.Seconds())
	}
}

// Resume 解除 Halt/Stop 的停止态并立即拉起（更新应用或回滚之后用）。
func (c *CoreIPC) Resume(ctx context.Context) error {
	c.mu.Lock()
	c.stopped = false
	c.notBefore = time.Time{}
	c.backoff = restartMin
	c.mu.Unlock()
	if err := c.Start(ctx); err != nil {
		return err
	}
	go c.Supervise(ctx) // Halt 期间监督循环已 return，这里重新挂上
	return nil
}

// ChatStream 调用 core 的 chat.generate，逐事件回调。
//
// 返回值 fallbackFrom 非空时表示 core 发生了模型降级（原模型名），
// 调用方据此向下游发 `X-Model-Fallback` 头（降级必留痕，旧版规则 3）。
func (c *CoreIPC) ChatStream(ctx context.Context, req *chat.GenerateRequest, onEvent func(name string, data json.RawMessage) error) (usage *chat.Usage, fallbackFrom string, err error) {
	cli, err := c.Client(ctx)
	if err != nil {
		return nil, "", err
	}
	cb := &rpc.Callback{OnEvent: func(m *jsonl.Msg) error {
		if m.Event == "usage" {
			var u chat.Usage
			if json.Unmarshal(m.Data, &u) == nil {
				usage = &u
			}
		}
		if onEvent != nil {
			return onEvent(m.Event, m.Data)
		}
		return nil
	}}
	res, _, err := cli.Invoke(ctx, "chat.generate", req, cb)
	if err != nil {
		return usage, "", err
	}
	// result 形如 {"ok":true,"channel":"...","fallback_from":"原模型"}
	if len(res.Result) > 0 {
		var out struct {
			FallbackFrom string `json:"fallback_from"`
		}
		if json.Unmarshal(res.Result, &out) == nil {
			fallbackFrom = out.FallbackFrom
		}
	}
	return usage, fallbackFrom, nil
}

var _ = io.Discard
