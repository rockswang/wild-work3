package api

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"wild-work3/main/core/internal/config"
	"wild-work3/main/core/internal/pool"
	"wild-work3/main/protocol/errkind"
	"wild-work3/main/protocol/meta"
	"wild-work3/main/protocol/stat"
)

// 本文件集中定义管理面「对外视图」结构。
//
// 视图形状以 gateway/web/app.js（原 demo 前端）为权威契约 —— 前端是既有资产，
// 后端适配前端，而不是让前端适配后端。字段名/嵌套/裸数组 vs 包裹对象都不可随意改动：
//   GET /core/channels          → 裸数组
//   GET /core/accounts          → 裸数组（?channel= 过滤）
//   GET /core/fees              → 裸数组
//   GET /core/logs              → 裸字符串数组
//   GET /core/state             → 对象（含 accounts/channels/listen{host,port}/api_key）
//   GET /core/usage|stats       → 对象（聚合值）
//   GET /gw/plugins|plugins/store → 裸数组
//   GET /gw/version|updates     → 对象

// ChannelView 渠道卡片视图。
type ChannelView struct {
	ChannelID    string   `json:"channel_id"`
	PluginID     string   `json:"plugin_id"`
	DisplayName  string   `json:"display_name"`
	Kind         string   `json:"kind"` // channel | hook
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
	Color        string   `json:"color"`
	Icon         string   `json:"icon"`
	Accounts     int      `json:"accounts"`
	Healthy      bool     `json:"healthy"`
	Cooling      bool     `json:"cooling"`
	CreditSum    string   `json:"credit_sum"`
	Enabled      bool     `json:"enabled"`
	Models       int      `json:"models"` // 数量（前端 `c.models 模型`）；明细在 /core/fees
}

// AccountView 账号卡片视图。
type AccountView struct {
	UID      string  `json:"uid"`
	Nickname string  `json:"nickname"`
	Channel  string  `json:"channel"` // 渠道实例 ID
	Credits  float64 `json:"credits"`
	Unit     string  `json:"unit"` // "" = 积分 | "token"
	Stale    bool    `json:"stale"`
	Cooling  bool    `json:"cooling"`
	Until    string  `json:"until"`
	// Reason 冷却/异常原因（pool.LastReason 透传，仅展示；空=未知）。
	Reason   string  `json:"reason,omitempty"`
	Disabled bool    `json:"disabled"`
	Expiring float64 `json:"expiring"`
	Unusable float64 `json:"unusable"`
}

// PluginView 插件视图（商店/已安装共用）。
type PluginView struct {
	PluginID     string   `json:"plugin_id"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Kind         string   `json:"kind"`
	Description  string   `json:"description"`
	Source       string   `json:"source"`
	Installed    bool     `json:"installed"`
	Latest       string   `json:"latest"`
	Capabilities []string `json:"capabilities"`
	Author       string   `json:"author"`
	// Icon/Color/ConfigSchema 供「添加渠道」向导动态渲染（W3-06）
	Icon         string          `json:"icon,omitempty"`
	Color        string          `json:"color,omitempty"`
	ConfigSchema json.RawMessage `json:"config_schema,omitempty"`
}

// FeeModel 费率条目；未上报为 null（不臆造）。
type FeeModel struct {
	ID      string   `json:"id"`
	Input   *float64 `json:"input"`
	Output  *float64 `json:"output"`
	Context *int     `json:"context"`
	// 思考档位（W3-32，与旧版 dev feesModelRow.reasoning_efforts 同口径）：
	// 上游目录**显式声明**才输出，未声明则省略（前端不展示，不编造）。
	Efforts []string `json:"efforts,omitempty"`
	// DefaultEffort 上游声明的默认档位；空 = 未声明。
	DefaultEffort string `json:"default_effort,omitempty"`
	// CanDisableThinking 思考能否真正关闭；nil = 未声明（假关 / 不可关时为 false）。
	CanDisableThinking *bool `json:"can_disable_thinking,omitempty"`
}

// FeeChannel 渠道费率。
type FeeChannel struct {
	Channel string     `json:"channel"`
	Name    string     `json:"name"`
	Models  []FeeModel `json:"models"`
}

// channels 构造渠道视图列表。
func (s *Server) channelsView() []ChannelView {
	cfg := s.Store.Get()
	now := time.Now()
	out := make([]ChannelView, 0, len(cfg.Channels))
	for _, ch := range cfg.Channels {
		v := ChannelView{
			ChannelID: ch.ID, PluginID: ch.PluginID, DisplayName: ch.DisplayName,
			Enabled: ch.Enabled, Models: len(ch.Models), Healthy: true,
			Color: "#52C41A", Icon: ch.PluginID, Kind: "channel",
			Capabilities: []string{},
		}
		if p := s.RT.Plugin(ch.PluginID); p != nil {
			v.Version = p.Met.Version
			v.Kind = orDefault(p.Met.Kind, "channel")
			v.Color = orDefault(p.Met.Color, v.Color)
			v.Icon = orDefault(p.Met.Icon, v.Icon)
			v.Capabilities = append(append([]string{}, p.Met.Capabilities.Control...), p.Met.Capabilities.Conversation...)
		}
		// 账号聚合：数量 / 冷却 / 余额合计
		var sum float64
		var haveCredit bool
		for _, e := range s.Pool.List() {
			if e.ChannelID != ch.ID {
				continue
			}
			v.Accounts++
			if e.Cool != pool.CoolNone {
				v.Cooling = true
			}
			if e.Credits != nil {
				sum += *e.Credits
				haveCredit = true
			}
			if st := pool.Status(e, now); st == "session_dead" || st == "disabled" {
				v.Healthy = false
			}
		}
		if haveCredit {
			v.CreditSum = "¥ " + trimFloat(sum)
		}
		if !ch.Enabled {
			v.Healthy = false
		}
		out = append(out, v)
	}
	return out
}

// accountsView 构造账号视图列表（可按渠道过滤）。
func (s *Server) accountsView(channelFilter string) []AccountView {
	cfg := s.Store.Get()
	now := time.Now()
	pentries := map[string]pool.Entry{}
	for _, e := range s.Pool.List() {
		pentries[e.UID] = e
	}
	out := make([]AccountView, 0, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		if channelFilter != "" && a.ChannelID != channelFilter {
			continue
		}
		v := AccountView{UID: a.UID, Nickname: orDefault(a.Nickname, a.UID), Channel: a.ChannelID,
			Unit: a.Unit, Disabled: a.Disabled}
		if a.Credits != nil {
			v.Credits = *a.Credits
		}
		if e, ok := pentries[a.UID]; ok {
			if e.Credits != nil {
				v.Credits = *e.Credits
			}
			v.Unit = orDefault(e.Unit, v.Unit)
			st := pool.Status(e, now)
			v.Cooling = st == "cooling"
			v.Stale = st == "session_dead"
			v.Reason = e.LastReason
			if e.CoolUntil.After(now) {
				v.Until = e.CoolUntil.Format("01-02 15:04")
			}
			// Unusable：硬冷却（402 余额/权益不足）中且已无可用额度——旧版三口径
			// （可用/不可用/未知）里的「不可用」，面板用它把号折叠到末尾。
			// 只对 CoolHard 判：软冷却/登录失效属临时故障，不代表额度不可用。
			if e.Cool == pool.CoolHard && v.Credits <= 0 {
				v.Unusable = v.Credits
			}
			// 24h 内到期的额度（仅面板展示）
			if e.ExpireAt != "" {
				if t, err := time.Parse("2006-01-02", e.ExpireAt); err == nil && t.Sub(now) < 24*time.Hour && t.After(now) {
					v.Expiring = v.Credits
				}
			}
		}
		if a.ExpiresAt != "" {
			if t, err := time.Parse("2006-01-02", a.ExpiresAt); err == nil && t.Sub(now) < 24*time.Hour && t.After(now) {
				v.Expiring = v.Credits
			}
		}
		out = append(out, v)
	}
	return out
}

// pluginViews 构造已安装插件视图（读取 runtime 扫描结果）。
func (s *Server) pluginViews() []PluginView {
	out := make([]PluginView, 0, len(s.RT.Plugins())) // 空时必须是 []，前端 .map 不能收 null
	for _, p := range s.RT.Plugins() {
		pv := PluginView{
			PluginID: p.Met.ID, Name: p.Met.Name, Version: p.Met.Version,
			Kind: orDefault(p.Met.Kind, "channel"), Description: p.Met.Description,
			Source: orDefault(p.Source, "official"), Installed: true, Latest: p.Met.Version,
			Capabilities: append(append([]string{}, p.Met.Capabilities.Control...), p.Met.Capabilities.Conversation...),
			Author:       "wild-work3",
			Icon:         p.Met.Icon, Color: p.Met.Color,
		}
		// 附 config.schema（「添加渠道」向导据此动态生成表单）
		if pv.Capabilities != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if res, err := s.RT.CachedCall(ctx, p.Met.ID, "config.schema", nil); err == nil && len(res.Result) > 0 {
				pv.ConfigSchema = json.RawMessage(res.Result)
			}
			cancel()
		}
		out = append(out, pv)
	}
	return out
}

// feesView 构造费率视图（模型来自渠道配置；价格/上下文由插件上报，缺省 null）。
func (s *Server) feesView(ctx context.Context) []FeeChannel {
	cfg := s.Store.Get()
	out := make([]FeeChannel, 0, len(cfg.Channels))
	for _, ch := range cfg.Channels {
		models := make([]FeeModel, 0, len(ch.Models))
		// 档位明细优先读缓存，未命中现拉一次（失败退化为「未声明」，不影响费率表渲染）
		infos := s.modelInfosOfChannel(ctx, ch)
		byID := make(map[string]meta.ModelInfo, len(infos))
		for _, mi := range infos {
			byID[mi.ID] = mi
		}
		for _, m := range ch.Models {
			fm := FeeModel{ID: m}
			if mi, ok := byID[m]; ok {
				fm.Efforts = meta.CleanEfforts(mi.Efforts)
				fm.DefaultEffort = mi.DefaultEffort
				fm.CanDisableThinking = mi.CanDisableThinking
			}
			models = append(models, fm)
		}
		out = append(out, FeeChannel{Channel: ch.ID, Name: ch.DisplayName, Models: models})
	}
	return out
}

// usageAgg 用量聚合（供 /core/usage）。
type usageAgg struct {
	Tokens    int
	Requests  int
	Spend     *float64
	Earn      *float64
	CacheRate *float64
	ByChannel []usageByChannel
	ByModel   []usageByModel
}

type usageByChannel struct {
	Channel string   `json:"channel"`
	Name    string   `json:"name"`
	Tokens  int      `json:"tokens"`
	Spend   *float64 `json:"spend"`
}

// usageByModel 模型用量排行（旧版 stats.Models / 面板「模型榜」）。
type usageByModel struct {
	Model    string   `json:"model"`
	Channel  string   `json:"channel"`
	Tokens   int      `json:"tokens"`
	Requests int      `json:"requests"`
	Spend    *float64 `json:"spend"`
}

// usageView 聚合 stat 记账的用量 + 积分流水。
func (s *Server) usageView(days int) usageAgg {
	recs := s.Stat.QueryUsage(days)
	agg := usageAgg{ByChannel: []usageByChannel{}, ByModel: []usageByModel{}} // 空时必须是 []，前端 .map 不能收 null
	var cached, covPrompt int
	byCh := map[string]int{}
	type mkey struct{ model, ch string }
	byM := map[mkey]*usageByModel{}
	var morder []mkey
	for _, x := range recs {
		agg.Tokens += x.TotalTokens
		agg.Requests++
		if x.CachedReported { // 只累计「上报了缓存维度」的请求，未上报的整条排除（旧版 CRC 语义）
			cached += x.CachedTokens
			covPrompt += x.PromptTokens
		}
		byCh[x.ChannelID] += x.TotalTokens
		if x.Model == "" {
			continue
		}
		k := mkey{x.Model, x.ChannelID}
		m := byM[k]
		if m == nil {
			m = &usageByModel{Model: x.Model, Channel: x.ChannelID}
			byM[k], morder = m, append(morder, k)
		}
		m.Tokens += x.TotalTokens
		m.Requests++
	}
	delete(byCh, "") // 路由失败等无渠道记录不计入渠道维度
	// 缓存利用率的分母必须是「上报了该维度」的请求 PT 合计（旧版 CRCov 加总口径）：
	// 用全部 PT 会把 glm/raccoon 这类不提供 cache 字段的渠道当"命中 0"，把命中率稀释下去。
	// covPrompt==0 → 窗口内无任何上报样本 → 前端显示「—」（null 不等于 0）。
	if covPrompt > 0 {
		r := float64(cached) / float64(covPrompt)
		agg.CacheRate = &r
	}
	// 积分：spend = delta<0 的绝对值合计；earn = delta>0 合计
	var spend, earn float64
	for _, c := range s.Stat.QueryCredits(days) {
		if c.Delta == nil {
			continue
		}
		if *c.Delta < 0 {
			spend -= *c.Delta
		} else {
			earn += *c.Delta
		}
	}
	if spend > 0 {
		agg.Spend = &spend
	}
	if earn > 0 {
		agg.Earn = &earn
	}
	// 渠道名解析
	names := map[string]string{}
	for _, ch := range s.Store.Get().Channels {
		names[ch.ID] = ch.DisplayName
	}
	ids := make([]string, 0, len(byCh))
	for id := range byCh {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return byCh[ids[i]] > byCh[ids[j]] })
	for _, id := range ids {
		agg.ByChannel = append(agg.ByChannel, usageByChannel{
			Channel: id, Name: orDefault(names[id], id), Tokens: byCh[id],
		})
	}
	// 模型榜：按 token 降序，最多 20 条（面板一次展示不了更多）
	sort.SliceStable(morder, func(i, j int) bool { return byM[morder[i]].Tokens > byM[morder[j]].Tokens })
	for i, k := range morder {
		if i >= 20 {
			break
		}
		m := byM[k]
		agg.ByModel = append(agg.ByModel, usageByModel{
			Model: m.Model, Channel: m.Channel, Tokens: m.Tokens, Requests: m.Requests,
		})
	}
	return agg
}

// statsView 统计视图（accounts_* / today_* / daily / warnings）。
func (s *Server) statsView() map[string]any {
	cfg := s.Store.Get()
	now := time.Now()
	accts := s.accountsView("")

	expiring, stale, cooling := 0, 0, 0
	for _, a := range accts {
		if a.Expiring > 0 {
			expiring++
		}
		if a.Stale {
			stale++
		}
		if a.Cooling {
			cooling++
		}
	}

	// 近 7 天 token 曲线
	daily := make([]map[string]any, 0, 7)
	recs := s.Stat.QueryUsage(7)
	byDay := map[string]int{}
	for _, x := range recs {
		if len(x.TS) >= 10 {
			byDay[x.TS[:10]] += x.TotalTokens
		}
	}
	for i := 6; i >= 0; i-- {
		d := now.AddDate(0, 0, -i)
		key := d.Format("2006-01-02")
		daily = append(daily, map[string]any{"day": d.Format("01-02"), "tokens": byDay[key]})
	}

	// 今日消耗/入账：取 credit 流水今日 delta 合计
	today := now.Format("2006-01-02")
	var spend, earn float64
	for _, c := range s.Stat.QueryCredits(1) {
		if c.Delta == nil || len(c.TS) < 10 || c.TS[:10] != today {
			continue
		}
		if *c.Delta < 0 {
			spend -= *c.Delta
		} else {
			earn += *c.Delta
		}
	}

	// 轮换次数：今日错误重试计数
	rotations := 0
	for _, x := range recs {
		if !x.OK && errkind.Rotatable(errkind.Kind(x.ErrKind)) {
			rotations++
		}
	}

	// 余额汇总：可消耗额度合计 + 临期（7 日内）+ 今日/明日到期账号数
	var totalCredits float64
	var expiring7d, expireToday, expireTomorrow float64
	var acctToday, acctTomorrow int
	tomorrow := now.AddDate(0, 0, 1).Format("2006-01-02")
	for _, e := range s.Pool.List() {
		if e.Credits != nil && *e.Credits > 0 {
			totalCredits += *e.Credits
		}
		switch e.ExpireAt {
		case "":
		case today:
			if e.Credits != nil {
				expireToday += *e.Credits
				acctToday++
			}
		case tomorrow:
			if e.Credits != nil {
				expireTomorrow += *e.Credits
				acctTomorrow++
			}
		default:
			if d, err := time.Parse("2006-01-02", e.ExpireAt); err == nil && d.Sub(now) < 7*24*time.Hour && e.Credits != nil {
				expiring7d += *e.Credits
			}
		}
	}

	// 渠道维度：今日 token / 请求 / 错误 / 轮换
	type chRow struct {
		Channel  string `json:"channel"`
		Name     string `json:"name"`
		Tokens   int    `json:"tokens"`
		Requests int    `json:"requests"`
		Errors   int    `json:"errors"`
		Rotates  int    `json:"rotations"`
	}
	chMap := map[string]*chRow{}
	var chOrder []string
	chNames := map[string]string{}
	for _, ch := range cfg.Channels {
		chNames[ch.ID] = orDefault(ch.DisplayName, ch.ID)
	}
	for _, x := range recs {
		if len(x.TS) < 10 || x.TS[:10] != today {
			continue
		}
		k := x.ChannelID
		if k == "" {
			continue
		}
		r := chMap[k]
		if r == nil {
			r = &chRow{Channel: k, Name: orDefault(chNames[k], k)}
			chMap[k], chOrder = r, append(chOrder, k)
		}
		r.Tokens += x.TotalTokens
		r.Requests++
		if !x.OK {
			r.Errors++
			if errkind.Rotatable(errkind.Kind(x.ErrKind)) {
				r.Rotates++
			}
		}
	}
	sort.SliceStable(chOrder, func(i, j int) bool { return chMap[chOrder[i]].Tokens > chMap[chOrder[j]].Tokens })
	channels := make([]chRow, 0, len(chOrder))
	for _, k := range chOrder {
		channels = append(channels, *chMap[k])
	}

	// 异常账号清单（旧版 stats.Abnormal）：冷却/失效/停用
	abnormal := make([]map[string]any, 0, 4)
	for _, a := range accts {
		switch {
		case a.Disabled:
			abnormal = append(abnormal, map[string]any{"uid": a.UID, "nickname": a.Nickname, "channel": a.Channel, "reason": "已停用"})
		case a.Stale:
			abnormal = append(abnormal, map[string]any{"uid": a.UID, "nickname": a.Nickname, "channel": a.Channel, "reason": orDefault(a.Reason, "登录态失效")})
		case a.Cooling:
			abnormal = append(abnormal, map[string]any{"uid": a.UID, "nickname": a.Nickname, "channel": a.Channel, "reason": orDefault(a.Reason, "冷却至 "+a.Until)})
		}
	}

	// 提醒
	warnings := []string{}
	for _, a := range accts {
		if a.Stale {
			warnings = append(warnings, a.UID+" 登录态可能失效")
		}
		if a.Cooling {
			warnings = append(warnings, a.UID+" 进入冷却")
		}
		if a.Expiring > 0 {
			warnings = append(warnings, a.UID+" 有额度今日到期")
		}
	}
	if len(cfg.Channels) == 0 {
		warnings = append(warnings, "尚未添加任何渠道")
	}

	return map[string]any{
		"accounts_total":          len(cfg.Accounts),
		"accounts_expiring_today": expiring,
		"accounts_stale":          stale,
		"accounts_cooling":        cooling,
		"today_spend":             nullableFloat(spend),
		"today_earn":              nullableFloat(earn),
		"rotations_today":         rotations,
		"daily":                   daily,
		"warnings":                warnings,
		// 额度口径（旧版 stats 的 TotalCredits / Expiring7d / ExpireToday / ExpireTomorrow）
		"total_credits":      nullableFloat(totalCredits),
		"expiring_7d":        nullableFloat(expiring7d),
		"expire_today":       nullableFloat(expireToday),
		"expire_tomorrow":    nullableFloat(expireTomorrow),
		"accts_expire_today": acctToday,
		"accts_expire_tmrw":  acctTomorrow,
		// 渠道维度（今日）与异常账号清单
		"channels": channels,
		"abnormal": abnormal,
		// 今日模型消耗榜（旧版 stats.Models；键名与 /core/usage 的 by_model 冾同）
		"by_model": s.usageView(1).ByModel,
		// 降级记录（旧版 stats.Fallbacks；空 = 无事件，前端隐藏区块）
		"fallbacks": s.fallbacksView(),
	}
}

// splitListen 拆分 "127.0.0.1:7864" → host/port。
func splitListen(l string) (string, int) {
	host, port := l, 0
	if i := strings.LastIndex(l, ":"); i > 0 {
		host = l[:i]
		for _, c := range l[i+1:] {
			if c < '0' || c > '9' {
				break
			}
			port = port*10 + int(c-'0')
		}
	}
	return strings.Trim(host, "[]"), port
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// nullableFloat 0 视作「无记录」返回 nil（前端 yuan(null) 显示 —），与上游 usage null 语义一致。
func nullableFloat(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

// 确保 config 包被引用（视图层使用其类型）。
var _ = config.ChannelConfig{}

// channelStatsView 聚合各渠道插件自记统计（W3-22）。
//
// 渠道插件知道 core 不知道的事实：上游真实缓存命中、积分包消耗、单次耗时/首字。
// 每个插件把 <data>/stat/stats.jsonl 汇总成 stat.Summary 经 `stats.get` 上报，
// core 只做合并与口径统一（只统计 kind=chat；-1=未上报不计入分母）。
//
// 说明：这是**按插件**聚合（一个插件可能被多个渠道实例复用），实例级细分由插件侧
// stat.Record.instance 承载；面板以渠道名展示，故这里同时给出 channel/name。
func (s *Server) channelStatsView(days int) map[string]any {
	if days <= 0 {
		days = 7
	}
	type row struct {
		Channel string `json:"channel"`
		Name    string `json:"name"`
		stat.Summary
	}

	// 按渠道实例并发拉取（不同渠道是不同上游，串行只会线性叠加面板加载时间）；
	// 结果按下标回填，保持面板顺序稳定。
	type slot struct {
		ch  *config.ChannelConfig
		row row
		ok  bool
	}
	slots := make([]slot, 0, 8)
	for _, ch := range s.Store.Get().Channels {
		pl := s.RT.Plugin(ch.PluginID)
		if pl == nil || !pl.Met.Capabilities.HasControl("stats.get") {
			continue
		}
		slots = append(slots, slot{ch: ch})
	}
	var wg sync.WaitGroup
	for i := range slots {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sl := &slots[i]
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			// Group 传渠道实例 ID：同一插件的不同区域实例（q-cn/q-com）不受彼此间隔限制
			res, err := s.RT.CallGroup(ctx, sl.ch.ID, sl.ch.PluginID, "stats.get",
				map[string]any{"days": days, "config": sl.ch.Config})
			if err != nil || res.Error != nil {
				return // 插件没实现 stats.get 是合法的，静默跳过
			}
			var sum stat.Summary
			if json.Unmarshal(res.Result, &sum) != nil {
				return
			}
			name := sl.ch.DisplayName
			if name == "" {
				name = sl.ch.ID
			}
			sl.row = row{Channel: sl.ch.ID, Name: name, Summary: sum}
			sl.ok = true
		}(i)
	}
	wg.Wait()

	out := make([]row, 0, len(slots))
	total := stat.Summary{Days: []stat.Day{}}
	for _, sl := range slots {
		if !sl.ok {
			continue // 顺序遍历以保持面板顺序稳定；失败静默
		}
		sum := sl.row.Summary
		out = append(out, sl.row)
		total.Requests += sum.Requests
		total.OK += sum.OK
		total.Errors += sum.Errors
		total.Prompt += sum.Prompt
		total.Completion += sum.Completion
		total.Total += sum.Total
		total.Cached += sum.Cached
		total.Credits += sum.Credits
	}
	// 合并每日曲线（插件各自按日聚合，core 相加）
	dayMap := map[string]*stat.Day{}
	for _, r := range out {
		for _, d := range r.Days {
			p := dayMap[d.Day]
			if p == nil {
				cp := d
				dayMap[d.Day] = &cp
				continue
			}
			p.Requests += d.Requests
			p.OK += d.OK
			p.Errors += d.Errors
			p.Prompt += d.Prompt
			p.Completion += d.Completion
			p.Total += d.Total
			p.Cached += d.Cached
			p.Credits += d.Credits
		}
	}
	keys := make([]string, 0, len(dayMap))
	for k := range dayMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		total.Days = append(total.Days, *dayMap[k])
	}
	return map[string]any{"days": days, "total": total, "channels": out}
}
