// Package runtime 统一运行时管理器：发现插件、按需一次性启动插件进程并调用（W3-20 定案）。
//
// 运行形态（AGENTS §2-T / W3-20）：
//   - 渠道插件：**无常驻**。每次调用 spawn 一个进程走 `call`，读完结果即回收。
//   - 拦截器插件：每次对话都要用，由 core 按需拉起后常驻，走 `stdio` 反复 invoke。
//
// 为什么不做常驻渠道 daemon：控制面调用是低频离散的（查模型、刷 token、看余额），
// 常驻要额外维护空闲回收、健康检查、插件升级后的一致性，收益不抵复杂度。代价是每次调用
// 都要付进程启动成本，因此这里必须做两件事（不能假定目标机器 65ms 就能起一个进程）：
//  1. **并发上限**：同一插件的在途调用数受 `max_workers`（缺省 4）限制，超出排队；
//  2. **批量串行化 + 间隔**：批量场景（刷新全部模型、批量签到、批量刷新账号）用
//     `CallByPlugin`——**按插件分组，组内串行 + `BatchGap`(200ms)、组间并行**。
//     串行的目的只是“别给同一个上游瞬间打一堆请求”，不同渠道是不同供应商，
//     把它们排成一条队列只会让总耗时线性叠加。
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wild-work3/main/protocol/chat"
	"wild-work3/main/protocol/cli"
	"wild-work3/main/protocol/jsonl"
	"wild-work3/main/protocol/meta"
	"wild-work3/main/protocol/plugin"
	"wild-work3/main/protocol/rpc"
)

// 调用节流参数（见包注释）。
const (
	// DefaultMaxWorkers 是插件未声明 max_workers 时的同插件并发上限。
	DefaultMaxWorkers = 4
	// BatchGap 是批量调用之间的最小间隔：给对端机器留出进程创建/回收的喘息时间。
	BatchGap = 200 * time.Millisecond
	// DefaultTimeout 是单次一次性调用的默认超时（控制面：models.list / accounts.* / credits.*）。
	DefaultTimeout = 30 * time.Second
	// ChatTimeout 是 chat.generate 的默认超时。对话是流式长请求，不能用控制面的 30s：
	// 真实上游首字慢时整段超过 30s 很常见（raccoon 实测 33.5s、26s，被 30s 卡成
	// context deadline exceeded）。与插件侧 `timeout` 配置默认值 300 保持一致。
	ChatTimeout = 300 * time.Second
)

// Plugin 一个已安装插件（可执行文件 + 元数据）。
type Plugin struct {
	Met    meta.Manifest
	Exe    string // 可执行文件绝对路径
	Dir    string
	Source string // official | community
}

// Manager 插件进程管理器。
type Manager struct {
	mu      sync.Mutex
	plugins map[string]*Plugin       // plugin_id -> plugin
	sem     map[string]chan struct{} // plugin_id -> 并发信号量（懒建）
	lastUse map[string]time.Time     // 分组键（渠道实例 ID）-> 上次调用时间（批量间隔用）
	cache   map[string]cachedMsg     // "plugin|method" -> 缓存结果（config.schema 等静态元数据）
	workDir string
	dataDir string
	log     *cli.Logger // 可选：core 注入后拦截器链的告警写入 core.log
}

// SetLogger 注入日志器（拦截器链的降级/失败告警需要落到 core.log）。
func (m *Manager) SetLogger(l *cli.Logger) { m.log = l }

// cachedMsg 是一条被缓存的方法结果。
type cachedMsg struct {
	msg jsonl.Msg
	at  time.Time
}

// New 创建管理器；pluginsDir 为插件安装根目录（<dir>/<plugin_id>/<version>/...），
// dataDir 为 core 数据目录（stat/pool-state/core.log 在此；插件可变量在 <dataDir>/plugins/<id>/）。
func New(pluginsDir, dataDir string) *Manager {
	return &Manager{
		plugins: map[string]*Plugin{},
		sem:     map[string]chan struct{}{},
		lastUse: map[string]time.Time{},
		cache:   map[string]cachedMsg{},
		workDir: pluginsDir,
		dataDir: dataDir,
	}
}

// Register 注册插件。
func (m *Manager) Register(p *Plugin) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.plugins[p.Met.ID] = p
}

// Plugin 取插件。
func (m *Manager) Plugin(id string) *Plugin {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.plugins[id]
}

// Plugins 列出已注册插件。
func (m *Manager) Plugins() []*Plugin {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Plugin, 0, len(m.plugins))
	for _, p := range m.plugins {
		out = append(out, p)
	}
	return out
}

// SubPlugins 是 core 数据根下存放各插件可变量的子目录名（W3-39）。
const SubPlugins = "plugins"

// PluginDataDir 返回插件的**数据根目录**＝<core data>/plugins/<plugin_id>（W3-39）。
//
// 为什么不再放在安装根 `<plugins>/<id>`（旧 W3-27）：安装树里只应装「可替换的 exe+manifest」，
// 而凭据/日志/统计是要跟版本、跟卸载存活的可变量。两者混在同一棵树下，卸载/回滚随时威胁数据
// （原先靠 store.keepOnUninstall 白名单当护栏），备份还要跨 dist/data 与 dist/plugins/*/ 两处。
// 拆开后：升级/回滚只换二进制（W3-39 第 5 条）、卸载可整目录 RemoveAll、备份＝拷一个 data。
//
// 为什么落在 **core 的数据根之内** 而不是与 core 平级的 `<data>/plugins/`：① 本方法的值
// 会作为 `--data` 交给插件，作用域就该和调用它的 core 一致——两个 core 实例各用一份凭据，
// 比共享一份更安全（不会出现 A 登录的账号被 B 拿去打真实上游、账单记到 A 头上）；
// ② 平级方案要靠「core 数据目录的父目录＝共享根」这个约定反推（`config.go:126` 为找
// config.json 才有的规则），把 `--data X` 直接指到别处时会算出 X/../plugins，跑出用户预期。
//
// 仍保持「一插件一根」：归属以插件 ID 为界（ID 由商店登记保证唯一，默认取仓库名去掉 -ext/-hook，
// W3-39 第 2 条），插件之间不得互读；core/gateway 可直接读（第 3 条）。数据文件的跨版本兼容性
// 由插件自己保证（第 5 条），所以这里不再掺版本号。
func (m *Manager) PluginDataDir(pluginID string) string {
	return filepath.Join(m.dataDir, SubPlugins, pluginID)
}

// preparePluginData 备好插件的数据根：<core data>/plugins/<id>/（0700），并把旧布局
// （<plugins>/<id>/{auths,logs,stat}，即可变量混在安装根里）搬进来。
//
// 建目录是必须的：新数据根不像安装根那样天然存在，而插件侧不保证自己 MkdirAll。
// 迁移是 W3-37 定案 4「除 auths 外无向前兼容义务」允许的唯一例外——凭据弄丢就是事故，
// 而布局旧一点不影响可用性。
//
// 保守规则：数据根里已有**同名且有内容**的子目录时不动（绝不能拿新数据盖掉旧凭据），
// 只记一条待人工合并；空壳目录（旧脚本 mkdir 出来的）让位。rename 失败（跨盘，或
// Windows 下插件进程还占着日志）退化为复制并保留源。
func (m *Manager) preparePluginData(installName, pluginID string) {
	root := m.PluginDataDir(pluginID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		m.logf("创建插件数据根 %s 失败: %v", root, err)
		return
	}
	for _, sub := range []string{cli.SubAuths, cli.SubLogs, cli.SubStat} {
		src := filepath.Join(m.workDir, installName, sub)
		if st, err := os.Stat(src); err != nil || !st.IsDir() {
			continue
		}
		dst := filepath.Join(root, sub)
		if st, err := os.Stat(dst); err == nil && st.IsDir() && !dirEmpty(dst) {
			m.logf("旧数据未迁移：%s 已有内容，需人工合并 src=%s", dst, src)
			continue
		}
		if dirEmpty(dst) {
			_ = os.Remove(dst) // 空壳让位，下面整目录 rename 过去
		}
		if err := os.Rename(src, dst); err == nil {
			m.infof("迁移插件数据 %s -> %s", src, dst)
			continue
		} else if cerr := copyTree(src, dst); cerr != nil {
			m.logf("迁移插件数据 %s 失败: rename=%v copy=%v", src, err, cerr)
			continue
		}
		// 复制成功：源留着会让「安装树只剩 exe+manifest」的假设继续成立不了，尽量清掉；
		// 清不掉（日志句柄被占）也只是多一层旧目录，不影响正确性。
		if rmErr := os.RemoveAll(src); rmErr != nil {
			m.infof("迁移插件数据（复制，源暂时删不掉）%s -> %s: %v", src, dst, rmErr)
		} else {
			m.infof("迁移插件数据（复制）%s -> %s", src, dst)
		}
	}
}

// dirEmpty 判目录是否无条目（旧布局里常有只建了壳的 auths/logs/stat，不该挡住迁移）。
func dirEmpty(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	_, err = f.ReadDir(1)
	return err == io.EOF
}

// copyTree 递归复制目录（rename 不可用时的退路；权限统一收到 0700/0600，凭据目录不能泄）。
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		tgt := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(tgt, 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(tgt, b, 0o600)
	})
}

// Discover 扫描插件目录，识别 <id>/<ver>/manifest.json + 可执行文件。
func (m *Manager) Discover() error {
	// 重新扫描意味着插件可能已升级，静态元数据缓存（config.schema）必须失效。
	m.ClearCache()
	ents, err := os.ReadDir(m.workDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		idDir := filepath.Join(m.workDir, e.Name())
		// <id>/current 指向当前版本目录名（无则取第一个版本目录）
		ver := ""
		if b, err := os.ReadFile(filepath.Join(idDir, "current")); err == nil {
			ver = strings.TrimSpace(string(b))
		} else if vers, _ := os.ReadDir(idDir); len(vers) > 0 {
			for _, v := range vers {
				if v.IsDir() {
					ver = v.Name()
					break
				}
			}
		}
		if ver == "" {
			continue
		}
		vdir := filepath.Join(idDir, ver)
		mb, err := os.ReadFile(filepath.Join(vdir, "manifest.json"))
		if err != nil {
			continue
		}
		var man meta.Manifest
		if err := json.Unmarshal(mb, &man); err != nil {
			fmt.Fprintf(os.Stderr, "[runtime] %s manifest 解析失败: %v\n", e.Name(), err)
			continue
		}
		exe := resolveExe(vdir, man.ID, man.Kind)
		// 备好数据根（含旧布局迁移），再注册（W3-39）
		m.preparePluginData(e.Name(), man.ID)
		m.Register(&Plugin{Met: man, Exe: exe, Dir: vdir, Source: "official"})
	}
	return nil
}

// resolveExe 定插件可执行文件（W3-25 命名约定）。
//
// 优先按约定：渠道插件 `<id>-ext`，拦截器插件 `<id>-hook`（项目名以 -hook 结尾）。
// 两者都试是为了兼容早期把 hook 打包成 `-ext` 的插件——目录里实际存在的那个说了算，
// 因为可执行文件名是打包事实，而 manifest.kind 是能力声明，不保证两者永远同步。
func resolveExe(vdir, id, kind string) string {
	// 后缀优先级按 manifest.kind：hook → `<id>-hook`，其余 → `<id>-ext`；另一形态作兜底。
	first, second := "-ext", "-hook"
	if kind == "hook" {
		first, second = second, first
	}
	names := []string{id + first, id + first + ".exe", id + second, id + second + ".exe"}
	for _, n := range names {
		if st, err := os.Stat(filepath.Join(vdir, n)); err == nil && !st.IsDir() {
			return filepath.Join(vdir, n)
		}
	}
	return filepath.Join(vdir, id+first+".exe")
}

// semOf 取（懒建）某插件的并发信号量，容量来自 manifest 的 max_workers。
func (m *Manager) semOf(pluginID string, maxWorkers int) chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sem[pluginID]; ok {
		return s
	}
	if maxWorkers <= 0 {
		maxWorkers = DefaultMaxWorkers
	}
	s := make(chan struct{}, maxWorkers)
	m.sem[pluginID] = s
	return s
}

// acquire 取得一次调用配额；ctx 取消时返回错误（排队中的调用可被取消）。
func (m *Manager) acquire(ctx context.Context, pluginID string, maxWorkers int) error {
	select {
	case m.semOf(pluginID, maxWorkers) <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) release(pluginID string) {
	m.mu.Lock()
	s := m.sem[pluginID]
	m.mu.Unlock()
	if s != nil {
		<-s
	}
}

// Call 调一次渠道插件方法（一次性进程，W3-20）。
//
// params 可以是任意可 JSON 序列化值，约定控制面方法至少带 {"config": ...}。
// 返回插件写回的 result 帧；err 为传输层/RPC 层错误（插件业务错误在 msg.Error 里）。
func (m *Manager) Call(ctx context.Context, pluginID, method string, params any) (jsonl.Msg, error) {
	return m.call(ctx, "", pluginID, method, params, nil)
}

// CallGroup 带并发分组键的一次调用（group 应为**渠道实例 ID**）。
//
// 与 Call 的唯一区别是「距上次调用的间隔」（gapBefore）按 group 而非 pluginID 计时：
// 同一插件的不同区域实例（q-cn / q-com、wb-cn / wb-global）连的是不同上游，
// 不该互相等 200ms。并发配额（max_workers）仍按 pluginID 算。
func (m *Manager) CallGroup(ctx context.Context, group, pluginID, method string, params any) (jsonl.Msg, error) {
	return m.call(ctx, group, pluginID, method, params, nil)
}

// call 是调用实现。group 是「同一上游」标识（渠道实例 ID），留空时退化为 pluginID。
// 两个维度不同：**并发配额按 pluginID**（受限的是进程数，同一插件的不同区域实例共用同一个 exe），
// **间隔计时按 group**（受限的对象是单个上游）。
func (m *Manager) call(ctx context.Context, group, pluginID, method string, params any, cb *rpc.Callback) (jsonl.Msg, error) {
	m.mu.Lock()
	p, ok := m.plugins[pluginID]
	m.mu.Unlock()
	if !ok {
		return jsonl.Msg{}, fmt.Errorf("插件未安装: %s", pluginID)
	}
	if group == "" {
		group = pluginID
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		// 对话与控制面的超时量级差一个数量级，不能共用同一个默认值。
		t := DefaultTimeout
		if method == "chat.generate" {
			t = ChatTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t)
		defer cancel()
	}
	if err := m.acquire(ctx, pluginID, p.Met.Execution.MaxWorkers); err != nil {
		return jsonl.Msg{}, err
	}
	defer m.release(pluginID)

	// 批量场景的间隔由 CallByPlugin/CallSerial 负责；这里只在「上一次调用刚结束」时补足间隔，
	// 避免上层密集循环（如面板一次渲染几十个渠道）瞬间拉起一串进程。
	m.gapBefore(ctx, group)
	defer m.markUsed(group)

	return plugin.RunOnce(ctx, plugin.Spec{
		Exe: p.Exe,
		// 命令是裸名（W3-21）：<exe> call --data <插件根目录>。
		// --data 必须显式传：插件据此定位 <data>/{auths,logs,stat}；
		// 不传则退回 exe 目录，而 exe 在版本目录里，升级一次凭据就“消失”。
		Args: []string{plugin.ModeCall, "--data", m.PluginDataDir(pluginID)},
		Env:  []string{"WW3_DATA=" + m.PluginDataDir(pluginID)},
	}, method, params, cb)
}

// gapBefore 若距上次调用不足 BatchGap 则等待（可被 ctx 取消）。key 是分组键（渠道实例 ID）。
func (m *Manager) gapBefore(ctx context.Context, key string) {
	m.mu.Lock()
	last, ok := m.lastUse[key]
	m.mu.Unlock()
	if !ok {
		return
	}
	if d := BatchGap - time.Since(last); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
	}
}

func (m *Manager) markUsed(key string) {
	m.mu.Lock()
	m.lastUse[key] = time.Now()
	m.mu.Unlock()
}

// CallSerial 逐条执行一批调用，两条之间留 BatchGap（批量刷新模型/签到/账号等场景专用）。
//
// 与 Call 的区别：**串行**而不是受并发上限约束——批量任务本身就是「宁可慢一点，
// 也不要把目标机器打成一堆同时创建的进程」。单条失败不中断，errors 按输入顺序返回
// （成功的为 nil），调用方自己汇总。
//
// 注意：跨插件场景应改用 CallByPlugin——不同渠道是不同上游，串行没有意义。
func (m *Manager) CallSerial(ctx context.Context, jobs []Job) ([]jsonl.Msg, []error) {
	out := make([]jsonl.Msg, len(jobs))
	errs := make([]error, len(jobs))
	for i, j := range jobs {
		if ctx.Err() != nil {
			errs[i] = ctx.Err()
			continue
		}
		if i > 0 {
			select {
			case <-time.After(BatchGap):
			case <-ctx.Done():
				errs[i] = ctx.Err()
				continue
			}
		}
		out[i], errs[i] = m.Call(ctx, j.PluginID, j.Method, j.Params)
	}
	return out, errs
}

// CallByPlugin 按「调用分组」并发执行一批调用：**组间并发、组内串行 + BatchGap**。
//
// 分组键是 Job.Group（缺省回退到 PluginID）。**调用方应该传渠道实例 ID 而不是插件 ID**：
// 同一个插件可以承载多个上游互不相干的实例（qoder 插件的 q-cn 走 openapi.qoder.com.cn、
// q-com 走 openapi.qoder.sh；workbuddy 插件的 wb-cn 与 wb-global 也是两套独立配额），
// 按插件 ID 归组会把它们又串起来，白白叠加耗时。
//
// 真正的约束是「同一个上游别瞬间打一堆请求」，因此：
//   - 组内（同一上游）串行 + BatchGap；
//   - 组间（不同上游）并发；
//   - 同一插件的总进程数仍由 max_workers 信号量在 Manager.call 里把守。
//
// 结果与错误按输入顺序返回；组内某条失败不影响同组后续与其它组。
func (m *Manager) CallByPlugin(ctx context.Context, jobs []Job) ([]jsonl.Msg, []error) {
	out := make([]jsonl.Msg, len(jobs))
	errs := make([]error, len(jobs))
	// 按分组键归组，保留原始下标以便回填结果
	groups := make(map[string][]int, len(jobs))
	order := make([]string, 0, len(jobs))
	for i, j := range jobs {
		key := j.Group
		if key == "" {
			key = j.PluginID
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}
	var wg sync.WaitGroup
	for _, key := range order {
		idxs := groups[key]
		wg.Add(1)
		go func(idxs []int) {
			defer wg.Done()
			for k, i := range idxs {
				if ctx.Err() != nil {
					errs[i] = ctx.Err()
					continue
				}
				if k > 0 { // 组内相邻两条之间留间隔
					select {
					case <-time.After(BatchGap):
					case <-ctx.Done():
						errs[i] = ctx.Err()
						continue
					}
				}
				out[i], errs[i] = m.call(ctx, jobs[i].Group, jobs[i].PluginID, jobs[i].Method, jobs[i].Params, nil)
			}
		}(idxs)
	}
	wg.Wait()
	return out, errs
}

// Job 是批量调用的一条。
//
// Group 是并发分组的键（**应填渠道实例 ID**）——同组串行、异组并发；
// 留空则按 PluginID 归组（此时同一插件的多实例会被串行，仅适合单实例插件）。
type Job struct {
	PluginID string
	Group    string
	Method   string
	Params   any
}

// SchemaTTL 是 config.schema 的缓存时长。
//
// config.schema 与插件版本绑定（插件升级后 runtime.Discover 重建缓存），可以长缓存；
// 面板每渲染一次渠道列表都去 spawn 一个进程问 schema 是纯浪费（W3-20 要避免的进程风暴）。
const SchemaTTL = 10 * time.Minute

// CachedCall 带 TTL 缓存的调用（目前只用于 config.schema 这类幂等元数据方法）。
//
// 缓存键 = 插件 ID + 方法；插件列表变更（升级/卸载）时调用方通过 ClearCache 清理。
func (m *Manager) CachedCall(ctx context.Context, pluginID, method string, params any) (jsonl.Msg, error) {
	key := pluginID + "|" + method
	m.mu.Lock()
	if c, ok := m.cache[key]; ok && time.Since(c.at) < SchemaTTL {
		msg := c.msg
		m.mu.Unlock()
		return msg, nil
	}
	m.mu.Unlock()

	msg, err := m.Call(ctx, pluginID, method, params)
	if err != nil {
		return msg, err
	}
	m.mu.Lock()
	m.cache[key] = cachedMsg{msg: msg, at: time.Now()}
	m.mu.Unlock()
	return msg, nil
}

// ClearCache 清空调用缓存（插件升级/卸载/重发现后调用）。
func (m *Manager) ClearCache() {
	m.mu.Lock()
	m.cache = map[string]cachedMsg{}
	m.mu.Unlock()
}

// ChatOptions 一次对话调用的选项。
type ChatOptions struct {
	PluginID string
	// ChannelID 是渠道实例 ID（q-cn / q-com 这类）。只用作并发分组键（gapBefore 间隔计时），
	// 不参与插件寻址；留空时退化为 PluginID。
	ChannelID      string
	ConversationID string
	OnEvent        func(name string, data json.RawMessage) error
	// Hooks 是本次请求要经过的拦截器链（W3-25）：按数组顺序执行，事件从链尾注入、
	// 反向穿过链后写入 OnEvent。为空则直通（零额外开销）。
	Hooks []HookSpec
}

// ChatStream 一次性进程执行一次 chat.generate，逐事件回调（W3-03 一请求一进程）。
//
// 返回 usage 与错误；正常结束时 err 为 nil。进程在 result 到达后立即回收，
// 不维护 conversation_id ↔ 进程映射（W3-11）。
//
// 配置了拦截器链时（opts.Hooks 非空），渠道事件先进一个内部 channel，再由
// RunHooks 以流水线方式反向穿过链尾→链头（见 hook.go 的包注释）。此时 OnEvent
// 收到的是**链处理之后**的事件；链上某层命中并返回错误时，本方法返回该错误，
// 调用方应把它作为终止事件发给下游（不补 [DONE]）。
func (m *Manager) ChatStream(ctx context.Context, req *chat.GenerateRequest, opts ChatOptions) (*chat.Usage, error) {
	m.mu.Lock()
	_, ok := m.plugins[opts.PluginID]
	m.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("插件未安装: %s", opts.PluginID)
	}
	// 对话不设额外超时：由 HTTP 客户端断连（ctx 取消）与插件侧 idle 看门狗共同决定生命周期。
	// 并发配额由 m.call 内部统一 acquire/release（同一插件信号量），此处**不得重复 acquire**——
	// 否则同一请求会占用两个配额，并发一旦超过 max_workers 就会互相饿死（实测 conc 6 时 30s 超时）。

	// 有拦截器时：渠道事件先入队，等渠道跑完后再由 RunHooks 反向穿过链。
	//	无拦截器时 sink 直接就是 OnEvent（不多一次跳转）。
	useHooks := len(opts.Hooks) > 0
	var queue chan HookEvent
	emit := opts.OnEvent
	if useHooks {
		queue = make(chan HookEvent, 256)
		emit = func(name string, data json.RawMessage) error {
			select {
			case queue <- HookEvent{Name: name, Data: data}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	var usage *chat.Usage
	var chanErr *streamErr // 渠道层错误（区别于拦截器层错误）
	cb := &rpc.Callback{OnEvent: func(msg *jsonl.Msg) error {
		var fields map[string]any
		if len(msg.Data) > 0 {
			_ = json.Unmarshal(msg.Data, &fields)
		}
		if msg.Event == "usage" && fields != nil {
			chat.MergeUsage(&usage, fields) // usage 可多帧更新；按字段合并，明确 0 仍视为上游实报
		}
		if msg.Event == "error" && fields != nil {
			msgText, _ := fields["message"].(string)
			kind, _ := fields["kind"].(string)
			chanErr = &streamErr{kind: kind, msg: msgText}
			if emit != nil {
				_ = emit(msg.Event, msg.Data)
			}
			return chanErr
		}
		if emit != nil {
			return emit(msg.Event, msg.Data)
		}
		return nil
	}}

	if req.ConversationID == "" {
		req.ConversationID = opts.ConversationID
	}
	_, callErr := m.call(ctx, opts.ChannelID, opts.PluginID, "chat.generate", req, cb)

	// 渠道事件已全部入队（或渠道失败）——关掉队列让 RunHooks 收尾。
	if useHooks {
		close(queue)
		up := &HookUpstream{OK: callErr == nil && chanErr == nil}
		if chanErr != nil {
			up.Kind, up.Message = chanErr.kind, chanErr.msg
		}
		// 下游最终落笔：直接写 OnEvent。hook 的产出已经在 RunHooks 内部按层路由。
		if err := m.RunHooks(ctx, opts.Hooks, req, up, queue, opts.OnEvent); err != nil {
			return usage, err
		}
	}

	if callErr != nil {
		// 插件以 ok=false 的 result 收尾时，错误在 result 里而不是传输层：
		// 统一转成带 kind 的 streamErr，让 route 能按 errkind 决定换号/冷却。
		var re *rpc.RPCError
		if errors.As(callErr, &re) {
			return usage, &streamErr{kind: re.Kind, msg: re.Message}
		}
		return usage, callErr
	}
	return usage, nil
}

func (m *Manager) callResultErr(msg jsonl.Msg) error {
	if msg.Error == nil {
		return nil
	}
	return &streamErr{kind: msg.Error.Kind, msg: msg.Error.Message}
}

type streamErr struct{ kind, msg string }

func (e *streamErr) Error() string { return e.msg }

// Kind 返回错误分类（供 rpc.KindOf 提取）。
func (e *streamErr) Kind() string { return e.kind }

// NopCloser 便于把 bytes.Buffer 当作 io.WriteCloser。
type NopCloser struct{ io.Writer }

// Close 实现 io.Closer。
func (NopCloser) Close() error { return nil }
