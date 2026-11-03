// models.go 模型明细（含思考档位）的取用与缓存。
//
// 背景（W3-32）：渠道插件的 `models.list` 会带上游目录**显式声明**的思考档位
// （`efforts` / `default_effort` / `can_disable_thinking`，见 meta.ModelInfo）。
// 但 core 的 `ChannelConfig.Models` 只落盘 ID 字符串列表（路由与面板计数都用它，
// 不该为档位改结构），所以档位明细走这里的两级取用：
//
//	① 内存缓存（`models.list` 刷新 / 渠道 test / 面板请求时顺手回填，TTL 见下）
//	② 缓存未命中 → 现拉一次 `models.list`（控制面一次性进程，成本可接受）
//
// 刻意**不**落盘：档位是上游目录的易变事实，落盘只会让面板展示过期档位；
// 拿不到就如实报「未声明」（nil），绝不臆造（R49）。
package api

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"wild-work3/main/core/internal/config"
	"wild-work3/main/protocol/meta"
)

// modelInfoTTL 档位明细的内存缓存时长。取 10 分钟与 scheduler 的积分刷新同量级：
// 上游目录变动不频繁，但面板不该为每次渲染都 spawn 一轮插件进程。
const modelInfoTTL = 10 * time.Minute

// modelInfoEntry 缓存条目（pluginID + 模型 ID → 明细）。
type modelInfoEntry struct {
	Info     meta.ModelInfo
	Fetched  time.Time
	PluginID string
}

// modelInfoCache 按「插件 ID + 模型 ID」索引档位明细。
//
// 键必须掺插件 ID：同一模型 ID 可能被多个插件声明（如 gpt-6 出现在 chatgpt 与
// custom-api 两个渠道），档位归插件所有，不能按模型名去重。
var modelInfoCache = struct {
	sync.RWMutex
	m map[string]modelInfoEntry
}{m: map[string]modelInfoEntry{}}

func modelInfoKey(pluginID, modelID string) string { return pluginID + "\x00" + modelID }

// cacheModelInfos 把一次 `models.list` 的结果写进缓存（批量，一次加锁）。
func cacheModelInfos(pluginID string, ms []meta.ModelInfo) {
	if pluginID == "" || len(ms) == 0 {
		return
	}
	now := time.Now()
	modelInfoCache.Lock()
	defer modelInfoCache.Unlock()
	for _, m := range ms {
		if m.ID == "" {
			continue
		}
		modelInfoCache.m[modelInfoKey(pluginID, m.ID)] = modelInfoEntry{Info: m, Fetched: now, PluginID: pluginID}
	}
}

// cachedModelInfo 查缓存（过期视同未命中）。
func cachedModelInfo(pluginID, modelID string) (meta.ModelInfo, bool) {
	modelInfoCache.RLock()
	e, ok := modelInfoCache.m[modelInfoKey(pluginID, modelID)]
	modelInfoCache.RUnlock()
	if !ok || time.Since(e.Fetched) > modelInfoTTL {
		return meta.ModelInfo{}, false
	}
	return e.Info, true
}

// fetchModelInfos 现拉一次插件的 `models.list`（控制面调用，失败返回 nil）。
//
// 失败不报错：档位是**展示增强**，拉不到就退化成「未声明」，绝不能因此让
// `/core/fees` 或 `/v1/models` 整体 500。
func (s *Server) fetchModelInfos(ctx context.Context, ch *config.ChannelConfig) []meta.ModelInfo {
	if s.RT == nil || ch == nil {
		return nil
	}
	res, err := s.RT.Call(ctx, ch.PluginID, "models.list", map[string]any{"config": ch.Config})
	if err != nil {
		Logf("拉取渠道 %s 模型明细失败（档位将显示为未声明）: %v", ch.ID, err)
		return nil
	}
	var payload struct {
		Models []meta.ModelInfo `json:"models"`
	}
	if err := json.Unmarshal(res.Result, &payload); err != nil {
		return nil
	}
	cacheModelInfos(ch.PluginID, payload.Models)
	return payload.Models
}

// modelInfosOfChannel 返回某渠道的模型明细（缓存优先，未命中现拉）。
func (s *Server) modelInfosOfChannel(ctx context.Context, ch *config.ChannelConfig) []meta.ModelInfo {
	if ch == nil || len(ch.Models) == 0 {
		return nil
	}
	// 全部命中缓存就不发 IPC（面板渲染是高频路径）
	hit := make([]meta.ModelInfo, 0, len(ch.Models))
	miss := false
	for _, id := range ch.Models {
		if mi, ok := cachedModelInfo(ch.PluginID, id); ok {
			hit = append(hit, mi)
		} else {
			miss = true
			break
		}
	}
	if !miss {
		return hit
	}
	if fresh := s.fetchModelInfos(ctx, ch); len(fresh) > 0 {
		return fresh
	}
	// 现拉失败：回退到「只有 ID」的最小形状（档位为空 = 未声明）
	out := make([]meta.ModelInfo, 0, len(ch.Models))
	for _, id := range ch.Models {
		if mi, ok := cachedModelInfo(ch.PluginID, id); ok {
			out = append(out, mi)
			continue
		}
		out = append(out, meta.ModelInfo{ID: id})
	}
	return out
}

// modelInfoOf 取单个模型明细（供 /v1/models 与 /core/fees 逐行填充）。
func (s *Server) modelInfoOf(ctx context.Context, ch *config.ChannelConfig, modelID string) meta.ModelInfo {
	if mi, ok := cachedModelInfo(ch.PluginID, modelID); ok {
		return mi
	}
	for _, mi := range s.modelInfosOfChannel(ctx, ch) {
		if mi.ID == modelID {
			return mi
		}
	}
	return meta.ModelInfo{ID: modelID}
}
