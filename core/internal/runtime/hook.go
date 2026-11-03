// 拦截器（hook）链执行（W3-25 / W3-26 双向流）。
//
// 拓扑与顺序（AGENTS §2-E）：
//
//	下游 ← H0 ← H1 ← … ← Hn-1 ← 上游
//
// 请求正向穿过链（H0 最先看到请求），响应事件**反向**回传（Hn-1 最先看到上游事件）。
// 每层 hook 是一个一次性进程，走双向流方法 `hook.stream`：core 用 `push` 帧把事件喂
// 进去，hook 用 `event` 帧把处理后的结果吐回来。
//
// 为什么必须双向流：逐事件做一次 invoke 等于每帧一次跨进程往返，首字延迟被帧数放大；
// 而且 hook 需要跨事件状态（行级判定、缓冲聚合），一帧一进程无从保持状态。
//
// 为什么是**流水线**而不是「先整段收集、再逐层过滤」：AGENTS §2-E 与 §9 的红线是
// 「SSE 逐事件透传，不得为逆序处理而缓存整段响应」。流水线语义正好等价于逐事件透传
// ——每个 hook 拿到一帧就立刻决定「转发 / 改写 / 不再转发」，无需知道全貌。于是
// 「主动终止」也自然成立：任意一层停止转发并返回错误，core 就把该错误作为终止事件
// 发给下游（不补 [DONE]，R36）。已发出的部分内容不可撤回，与 W3-10 不可逆提交边界一致。
//
// 失败策略（AGENTS §2-E）：
//   - 启动失败：FailOpen 为真则该层退化成直通（入口直接接到它的下游），否则中断请求。
//   - 流中途失败：一律中断请求。hook 可以合并/丢弃/改写事件，core 无法知道它已转发到
//     哪一帧，所以「跳过它继续」既不能保证不重复也不能保证不丢内容（见 bypassNote）。
//     宁可明确报错，也不伪造一个看起来正常的收尾。
//   - 收尾 result 失败：同启动失败——FailOpen 则记警告继续，否则中断。
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"

	"wild-work3/main/protocol/chat"
	"wild-work3/main/protocol/jsonl"
	"wild-work3/main/protocol/plugin"
	"wild-work3/main/protocol/rpc"
)

// bypassNote 说明为什么流中途的 fail-open 只能退化成「中断」：
//
//	hook 是流水线里的一层，它可能把 3 帧合成 1 帧、丢弃若干帧、或改成完全不同的内容。
//	一旦它在第 k 帧之后崩掉，core 只知道「上游已经推到第 k+t 帧、下游收到了未定数量的帧」，
//	既无法续接也无法回滚。唯一诚实的做法是中断并报错，让下游看到明确的 error 终止帧。
var bypassNote = "see package comment"

// HookSpec 一个参与本请求的拦截器实例。
type HookSpec struct {
	ID       string         // 实例 ID（如 loopguard-1），用于日志与插件侧区分
	PluginID string         // 插件 ID（如 loopguard）
	Order    int            // 链上顺序（小→大，旧版 hooks[].order 语义）
	Config   map[string]any // 该实例的插件专属配置（阈值等）
	// FailOpen 决定启动失败/收尾失败时是跳过该层（true）还是中断请求（false）。
	// 观测类 hook 用 true；会改写内容或决定能否放行的用 false。
	FailOpen bool
}

// HookEvent 一个归一化事件（名字 + 原始 data 字节，core 不解释其内容）。
type HookEvent struct {
	Name string
	Data json.RawMessage
}

// HookUpstream 上游执行结果摘要，随 hook.stream 的 params 下发。
//
// why：上游本身失败时 hook 需要知道「这不是我该管的事」——例如守卫不该在空流上误报，
// 也不该把自己的命中覆盖成上游错误。
type HookUpstream struct {
	OK      bool   `json:"ok"`
	Kind    string `json:"kind,omitempty"`
	Message string `json:"message,omitempty"`
}

// hookParams 是 hook.stream 的 params。
type hookParams struct {
	Request  *chat.GenerateRequest `json:"request,omitempty"` // 本次对话请求（hook 可能据 model/messages 决策）
	Config   map[string]any        `json:"config,omitempty"`  // 该实例配置
	HookID   string                `json:"hook_id,omitempty"` // 实例 ID
	Chain    hookChain             `json:"chain"`             // 链位置
	Upstream *HookUpstream         `json:"upstream,omitempty"`
}

// hookChain 链位置（Index 从 0 开始，0 = 最靠近下游、最先看到请求的层）。
type hookChain struct {
	Index int `json:"index"`
	Total int `json:"total"`
}

// hop 是一层事件落点的间接层。
//
// 需要间接层的原因：FailOpen 的层启动失败后要退化成直通，而调用方（上游 hook 的
// emit 回调）已经持有这个 hop 指针，只能替换 fn 不能换指针。
type hop struct {
	mu sync.Mutex
	fn func(name string, payload any) error
}

func (h *hop) push(name string, data json.RawMessage) error {
	h.mu.Lock()
	f := h.fn
	h.mu.Unlock()
	if f == nil {
		return nil
	}
	return f(name, data)
}

// HookChain 一条已配置并排好序的拦截器链（每次请求构造一次）。
type HookChain struct {
	Hooks []HookSpec
}

// RunHooks 让事件流反向穿过拦截器链，在流水线中直接转发给下游。
//
// src 由上游生产（core 在 channel 事件回调里塞入），emit 写往下游（IPC 事件写入器）。
// 返回错误表示链上某层失败，调用方应把该错误作为终止事件发给下游，而不要补正常收尾。
func (m *Manager) RunHooks(
	ctx context.Context,
	hooks []HookSpec,
	req *chat.GenerateRequest,
	upstream *HookUpstream,
	src <-chan HookEvent,
	emit func(name string, data json.RawMessage) error,
) error {
	// 无 hook：直通，零额外开销（不为「统一路径」而多绕一层 channel）。
	if len(hooks) == 0 {
		for e := range src {
			if err := emit(e.Name, e.Data); err != nil {
				return err
			}
		}
		return nil
	}

	n := len(hooks)
	down := &hop{fn: func(name string, payload any) error {
		data, _ := payload.(json.RawMessage)
		return emit(name, data)
	}} // 链头 hook 的输出 → 下游
	in := make([]*hop, n) // in[i] = 投给 hook i 的入口
	calls := make([]*plugin.Call, n)
	defer func() {
		for _, c := range calls {
			if c != nil {
				c.Close()
			}
		}
	}()

	// 自链头向链尾启动：启动 hook i 时它的下游落点（i==0→down，否则 in[i-1]）已就绪，
	// 因此不存在时序空档。
	for i := 0; i < n; i++ {
		h := hooks[i]
		target := down
		if i > 0 {
			target = in[i-1]
		}
		params := hookParams{Request: req, Config: h.Config, HookID: h.ID, Upstream: upstream}
		params.Chain = hookChain{Index: i, Total: n}
		cb := &rpc.Callback{OnEvent: func(msg *jsonl.Msg) error {
			return target.push(msg.Event, msg.Data)
		}}
		c, err := m.startHook(ctx, h.PluginID, params, cb)
		if err != nil {
			if !h.FailOpen {
				return fmt.Errorf("拦截器 %s 启动失败: %w", h.ID, err)
			}
			// fail-open：该层退化成直通——把它的入口直接接到它的下游。
			m.logf("拦截器 %s 启动失败，已跳过我层: %v", h.ID, err)
			in[i] = target
			continue
		}
		calls[i] = c
		in[i] = &hop{fn: c.Send}
	}

	// 注入：把上游事件逐条推进链尾。任一层拒绝即中止。
	inj := in[n-1]
	for e := range src {
		if err := inj.push(e.Name, e.Data); err != nil {
			return fmt.Errorf("拦截器链拒绝事件 %s: %w", e.Name, err)
		}
	}
	end := map[string]any{"ok": upstream == nil || upstream.OK}
	if upstream != nil && upstream.Kind != "" {
		end["kind"], end["message"] = upstream.Kind, upstream.Message
	}
	if err := inj.push("upstream.end", mustRawJSON(end)); err != nil {
		return fmt.Errorf("拦截器链拒绝结束帧: %w", err)
	}

	// 收尾：从链尾往链头等结果（数据流方向的反向），错误优先归因到最靠近上游那层。
	for i := n - 1; i >= 0; i-- {
		c := calls[i]
		if c == nil {
			continue
		}
		if _, err := c.Wait(); err != nil {
			if !hooks[i].FailOpen {
				return fmt.Errorf("拦截器 %s 处理失败: %w", hooks[i].ID, err)
			}
			m.logf("拦截器 %s 收尾报错，已忽略: %v", hooks[i].ID, err)
		}
	}
	return nil
}

// startHook 启动 hook 进程并发起 hook.stream 双向流调用。
func (m *Manager) startHook(ctx context.Context, pluginID string, params hookParams, cb *rpc.Callback) (*plugin.Call, error) {
	m.mu.Lock()
	p, ok := m.plugins[pluginID]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("插件未安装: %s", pluginID)
	}
	if err := m.acquire(ctx, pluginID, p.Met.Execution.MaxWorkers); err != nil {
		return nil, err
	}
	c, err := plugin.Start(ctx, m.spec(p), "hook.stream", params, cb)
	if err != nil {
		m.release(pluginID)
		return nil, err
	}
	// 进程退出时释放并发配额。Start 的 Wait 只等一次，这里的 goroutine 挂在 done 上。
	go func() {
		_, _ = c.Wait()
		m.release(pluginID)
	}()
	return c, nil
}

// SortHooks 按 Order 升序、同 Order 按 ID 字典序排序（AGENTS §2-Y 链顺序）。
//
// 只按 ID 排会忽略配置里的 order——面板上调过顺序后链路依旧照旧，
// 表现为「改了配置没效果」。同 Order 必须再比 ID：两次跑结果要可复现。
func SortHooks(hs []HookSpec) []HookSpec {
	out := make([]HookSpec, len(hs))
	copy(out, hs)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// mustRawJSON 序列化（内部固定结构，失败给空对象而非 panic）。
func mustRawJSON(v any) jsonl.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return jsonl.RawMessage(`{}`)
	}
	return b
}

// logf 统一日志出口（Manager 无 logger 时退化到 stderr）。
func (m *Manager) logf(format string, args ...any) {
	if m.log != nil {
		m.log.Errorf(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "[runtime] "+format+"\n", args...)
}

// infof 记常规流程信息（区别于 logf：迁移/装载这类正常事件不该在日志里冒红成 ERROR）。
func (m *Manager) infof(format string, args ...any) {
	if m.log != nil {
		m.log.Infof(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, "[runtime] "+format+"\n", args...)
}

// spec 构造一次性进程的启动描述（与 call 里的参数保持一致，勿各自拼一份）。
func (m *Manager) spec(p *Plugin) plugin.Spec {
	dir := m.PluginDataDir(p.Met.ID)
	return plugin.Spec{
		Exe:  p.Exe,
		Args: []string{plugin.ModeCall, "--data", dir},
		Env:  []string{"WW3_DATA=" + dir},
	}
}
