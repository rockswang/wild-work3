// sched.go 调度器需要的三类动作（scheduler.Tasks 的实现）：
// 凭据保活续期、每日签到、额度明细刷新（含差分记账）。
//
// 三者都走 runtime.CallByPlugin：组内（同一渠道实例）串行 + BatchGap 间隔，
// 组间（不同上游）并行——这些动作都会真实打上游，不能对同一上游瞬间拉起一串进程。
package api

import (
	"context"
	"encoding/json"
	"time"

	"wild-work3/main/core/internal/config"
	"wild-work3/main/core/internal/runtime"
	"wild-work3/main/protocol/errkind"
	"wild-work3/main/protocol/meta"
	"wild-work3/main/protocol/rpc"
)

// schedTimeout 单批调用的超时预算（按 job 数线性放宽，至少 1 分钟）。
func schedTimeout(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	return time.Duration(n) * time.Minute
}

// RefreshAccounts 凭据保活续期（旧版 RunKeepaliveNow）：只对声明 accounts.refresh 的渠道发。
//
// 与旧版对齐的一点：刷新回 SessionDead（refresh token 也死了）时把账号**停用**并记日志——
// 旧版 dev/internal/scheduler/scheduler.go:669-704 的 `Pool.Disable(uid, "12153 session dead")` 同口径。
// 不停用的话该账号会一直被挑中、一直 401，把整条渠道拖成不可用。
func (s *Server) RefreshAccounts(ctx context.Context) (ok, failed int) {
	type job struct {
		runtime.Job
		acctUID string
	}
	var jobs []job
	for _, ac := range s.Store.Get().Accounts {
		if ac.Disabled {
			continue
		}
		ch := s.Store.Channel(ac.ChannelID)
		if ch == nil || !ch.Enabled || !s.hasCap(ch.PluginID, "accounts.refresh") {
			continue
		}
		jobs = append(jobs, job{
			Job: runtime.Job{PluginID: ch.PluginID, Group: ch.ID, Method: "accounts.refresh",
				Params: map[string]any{"uid": orDefault(ac.RemoteUID, ac.UID), "config": ch.Config}},
			acctUID: ac.UID,
		})
	}
	if len(jobs) == 0 {
		return 0, 0
	}
	rj := make([]runtime.Job, len(jobs))
	for i := range jobs {
		rj[i] = jobs[i].Job
	}
	c, cancel := context.WithTimeout(ctx, schedTimeout(len(rj)))
	defer cancel()
	_, errs := s.RT.CallByPlugin(c, rj)
	for i := range errs {
		if errs[i] == nil {
			ok++
			continue
		}
		failed++
		Logf("保活续期失败 %s/%s: %v", jobs[i].Group, jobs[i].Params.(map[string]any)["uid"], errs[i])
		// refresh token 也失效 → 停用账号（旧版 Disable），避免持续 401 拖垮渠道
		if rpc.KindOf(errs[i]) == errkind.SessionDead {
			s.disableAccount(jobs[i].acctUID)
			Logf("保活：登录态失效已停用账号 %s（需重新登录）", jobs[i].acctUID)
		}
	}
	return ok, failed
}

// disableAccount 把账号置为停用（幂等，账号不存在时无操作）。
func (s *Server) disableAccount(uid string) {
	_ = s.Store.Update(func(c *config.Config) error {
		for _, a := range c.Accounts {
			if a.UID == uid {
				a.Disabled = true
			}
		}
		return nil
	})
}

// Checkin 每日签到：只对声明 accounts.checkin 的渠道发（批次级，uid 留空）。
// 签到成功后顺带刷新该渠道的额度明细，让签到发放立刻进流水。
func (s *Server) Checkin(ctx context.Context) (ok, failed int) {
	var jobs []runtime.Job
	var chans []config.ChannelConfig
	for _, ch := range s.Store.Get().Channels {
		if !ch.Enabled || !s.hasCap(ch.PluginID, "accounts.checkin") {
			continue
		}
		chans = append(chans, *ch)
		jobs = append(jobs, runtime.Job{PluginID: ch.PluginID, Group: ch.ID, Method: "accounts.checkin",
			Params: map[string]any{"config": ch.Config}})
	}
	if len(jobs) == 0 {
		return 0, 0
	}
	c, cancel := context.WithTimeout(ctx, schedTimeout(len(jobs)))
	defer cancel()
	_, errs := s.RT.CallByPlugin(c, jobs)
	for i := range errs {
		if errs[i] == nil {
			ok++
		} else {
			failed++
			Logf("自动签到失败 %s: %v", chans[i].ID, errs[i])
		}
	}
	// 签到可能发放额度：刷新一次明细做差分记账（失败不影响签到结果）
	s.RefreshCredits(ctx)
	return ok, failed
}

// RefreshCredits 拉取全部渠道的额度明细 → 差分记账 + 回写选号摘要。
//
// 只对声明 accounts.resource_detail 的渠道发；uid 必须逐个指定（插件接口是单账号粒度）。
// 并发度由 CallByPlugin 按渠道实例分组控制。
func (s *Server) RefreshCredits(ctx context.Context) (ok, failed int) {
	var jobs []runtime.Job
	var targets []config.Account
	for _, ac := range s.Store.Get().Accounts {
		if ac.Disabled {
			continue
		}
		ch := s.Store.Channel(ac.ChannelID)
		if ch == nil || !ch.Enabled || !s.hasCap(ch.PluginID, "accounts.resource_detail") {
			continue
		}
		targets = append(targets, *ac)
		jobs = append(jobs, runtime.Job{PluginID: ch.PluginID, Group: ch.ID, Method: "accounts.resource_detail",
			Params: map[string]any{"uid": orDefault(ac.RemoteUID, ac.UID), "config": ch.Config}})
	}
	if len(jobs) == 0 {
		return 0, 0
	}
	c, cancel := context.WithTimeout(ctx, schedTimeout(len(jobs)))
	defer cancel()
	res, errs := s.RT.CallByPlugin(c, jobs)
	for i := range errs {
		if errs[i] != nil {
			failed++
			Logf("刷新额度失败 %s: %v", targets[i].UID, errs[i])
			continue
		}
		s.applyResourceFacts(targets[i].UID, targets[i].ChannelID, res[i].Result)
		ok++
	}
	return ok, failed
}

// refreshCreditsOne 单账号额度刷新（签到后调用；失败只记日志）。
func (s *Server) refreshCreditsOne(ctx context.Context, channelID, uid, remoteUID string, chConfig map[string]any) {
	ch := s.Store.Channel(channelID)
	if ch == nil || !s.hasCap(ch.PluginID, "accounts.resource_detail") {
		return
	}
	c, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	res, err := s.RT.Call(c, ch.PluginID, "accounts.resource_detail",
		map[string]any{"uid": remoteUID, "config": chConfig})
	if err != nil {
		Logf("账号 %s 签到后刷新额度失败: %v", uid, err)
		return
	}
	s.applyResourceFacts(uid, channelID, res.Result)
}

// resourceItems 解析插件返回的 ResourceItem 数组（供视图层复用）。
func resourceItems(raw json.RawMessage) []meta.ResourceItem {
	var items []meta.ResourceItem
	_ = json.Unmarshal(raw, &items)
	return items
}
