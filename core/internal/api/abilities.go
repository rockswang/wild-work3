// 标准可选业务能力（W3-30）：systemone.decide / images.generate 的 core 侧转发层。
//
// 两者都是**非流式一次性控制面调用**（一调用一进程、只回一帧 result），因此走 RT.Call
// 而非 ChatStream。与对话链路的三个关键差别：
//  1. 超时用对话级 ChatTimeout（生图 12~22s、决策 10s+），不能用控制面 DefaultTimeout=30s
//     （AGENTS 陷阱 23 同类）；
//  2. 不参与模型路由/降级/换号——虚拟模型键（glm/image、oczen/systemone）只用于账号池
//     冷却与流水归因（W3-30）；
//  3. 能力未声明时回 kind=not_supported（scope=channel），不冷却不换号。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"wild-work3/main/core/internal/pool"
	"wild-work3/main/core/internal/runtime"
	"wild-work3/main/protocol/errkind"
	"wild-work3/main/protocol/rpc"
)

// abilityReq 是 /api/{systemone,images} 的入参（前端/网关传入）。
type abilityReq struct {
	ChannelID string          `json:"channel_id"` // 必填：目标渠道实例
	UID       string          `json:"uid"`        // 可选：账号 UID（RemoteUID 口径）
	Params    json.RawMessage `json:"params"`     // 原样透传给插件的业务参数（不含 channel_id/uid）
}

// systemoneDecide 处理 POST /api/systemone。
func (s *Server) systemoneDecide(w http.ResponseWriter, r *http.Request) error {
	return s.abilityCall(w, r, "systemone.decide", "systemone")
}

// imagesGenerate 处理 POST /api/images/generate。
func (s *Server) imagesGenerate(w http.ResponseWriter, r *http.Request) error {
	return s.abilityCall(w, r, "images.generate", "image")
}

// abilityCall 是两条能力的公共转发：解析入参 → 校验能力声明 → 以 ChatTimeout 调用插件 →
// 原样回插件 result；失败按 errkind 映射到 HTTP 状态与冷却策略（W3-30 §5.8.2 错误映射）。
func (s *Server) abilityCall(w http.ResponseWriter, r *http.Request, method, virtualModel string) error {
	var req abilityReq
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20)) // 生图参考图上限口径同 gateway（32MiB）
	if err != nil {
		return errf("读取请求体失败: %v", err)
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return errf("请求体不是合法 JSON: %v", err)
		}
	}
	ch := s.Store.Channel(req.ChannelID)
	if ch == nil {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("渠道不存在: %s", req.ChannelID))
		return nil
	}
	if !ch.Enabled {
		writeErr(w, http.StatusConflict, fmt.Sprintf("渠道已停用: %s", req.ChannelID))
		return nil
	}
	// 能力声明为准：未声明 → not_supported（不冷却、不换号、不罚号）
	if !s.hasCap(ch.PluginID, method) {
		writeAbilityErr(w, errkind.NotSupported, errkind.ScopeChannel,
			fmt.Sprintf("渠道 %s 不支持 %s", ch.DisplayName, abilityName(method)))
		return nil
	}
	// 账号 UID：入参给的即插件侧 RemoteUID（W3-19：只有 core 内部主键才掺渠道 ID）。
	uid := req.UID
	params := map[string]any{"config": ch.Config}
	if uid != "" {
		params["uid"] = uid
	}
	// 业务参数原样透传（state/questions/prompt/images/… 由插件本地严格校验，W3-30）
	if len(req.Params) > 0 {
		var extra map[string]json.RawMessage
		if err := json.Unmarshal(req.Params, &extra); err == nil {
			for k, v := range extra {
				params[k] = v
			}
		}
	}
	// 长超时：生图 12~22s、决策 10s+，不能用控制面 DefaultTimeout=30s（W3-30 §5.8）。
	// runtime.call 只在 ctx **无** deadline 时才套默认值，故这里显式给对话级超时。
	ctx, cancel := context.WithTimeout(r.Context(), runtime.ChatTimeout)
	defer cancel()
	res, err := s.RT.Call(ctx, ch.PluginID, method, params)
	if err != nil {
		k := rpc.KindOf(err)
		// 失败按 kind 写冷却：虚拟模型键只用于流水/冷却归因，不进 models.list（W3-30）
		if uid != "" {
			switch k {
			case errkind.SoftRate:
				s.Pool.CooldownModel(uid, virtualModel, s.Pool.SoftCooldown())
			case errkind.HardCredit, errkind.AccountFault:
				s.Pool.CooldownFor(uid, virtualModel, pool.CoolSoft, s.Pool.SoftCooldown())
			case errkind.SessionDead:
				s.Pool.Cooldown(uid, virtualModel, pool.CoolDead)
			}
		}
		Logf("%s 失败（渠道 %s）: %v", abilityName(method), ch.ID, err)
		writeAbilityErr(w, k, errkind.ScopeChannel, rpc.MessageOf(err))
		return nil
	}
	writeJSON(w, json.RawMessage(res.Result))
	return nil
}

// abilityName 取能力的中文名（错误文案与日志共用）。
func abilityName(method string) string {
	if method == "images.generate" {
		return "生图"
	}
	return "结构化决策"
}

// writeAbilityErr 按统一错误形状回错：非 2xx 一律 {ok:false,error,kind,scope}，
// 前端 api() 读 data.error 抛异常（W3-17），另两字段供脚本判定是否可重试。
// 状态码映射沿用旧版 runGLM 口径（W3-30 §5.8.2 错误映射）；core 只做映射，
// 冷却/换号策略在 abilityCall 里写给账号池。
func writeAbilityErr(w http.ResponseWriter, kind, scope, msg string) {
	code := http.StatusBadGateway // 默认 502（渠道/上游侧问题）
	switch kind {
	case errkind.NotSupported:
		code = http.StatusNotImplemented
	case errkind.BadParams, errkind.ContentBlocked, errkind.Passthrough, errkind.PromptTooLong,
		errkind.ImageInvalid, errkind.ModelBlocked:
		code = http.StatusBadRequest
	case errkind.SessionDead, errkind.Auth:
		code = http.StatusUnauthorized
	case errkind.SoftRate, errkind.Queued:
		code = http.StatusTooManyRequests
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code) // Truncated/Server/Conn/IdleTimeout 走默认 502：产物不完整或上游故障都不得回成功
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": false, "error": msg, "kind": kind, "scope": scope,
	})
}
