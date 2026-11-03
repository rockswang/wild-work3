// 标准可选业务能力（W3-30）的对外 HTTP 端点：
//
//	POST /v1/systemone            → core POST /api/systemone（→ IPC systemone.decide）
//	POST /v1/images/generations   → core POST /api/images/generate（→ IPC images.generate）
//	POST /v1/images/edits         → 同上，先把 multipart 的 image/image[] 解成 images[]
//
// 三条都是**非流式** JSON 应答，不进 chat 流式管线（W3-13 #9 只约束对话数据面）。
// 鉴权与三协议一致（h.auth，Bearer / x-api-key）。
//
// 分层红线（W3-30 / W3-09）：core 与 gateway 只管**快速失败**（必填键存在、参考图体积）
// 并把参数原样透传；尺寸/比例合法性、参考图怎么吸收（拼版/直传/图床）全归插件——
// 旧版实证上游对非法比例**静默降级 1:1**，在 gateway 兜就会产出错误图还回成功。
package v1

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// imagesBodyLimit 生图请求体上限 32MiB（W3-30 §5.8.2）：
	// 参考图 base64 膨胀 4/3，而 jsonl 单帧上限 64MB → 原始字节合计限制在 ~24MB 以内，
	// 这里留足余量（也远小于 chat 的 128MiB）。
	imagesBodyLimit = 32 << 20
	// abilityTimeout 生图是 12~22s 的同步长调用、决策 10s+，必须用对话级超时量级，
	// 不能沿用控制面 30s（W3-30 共同约定 / AGENTS 陷阱 23 同类）。
	abilityTimeout = 300 * time.Second
)

// systemone POST /v1/systemone：结构化决策。
func (h *Handler) systemone(w http.ResponseWriter, r *http.Request) {
	body, ok := readLimited(w, r, 4<<20)
	if !ok {
		return
	}
	var req struct {
		State     *string         `json:"state"` // 指针：区分「键缺失」与「空串」（上游要求键存在，空串合法）
		Questions json.RawMessage `json:"questions"`
		Model     string          `json:"model"`
		ChannelID string          `json:"channel_id"`
		UID       string          `json:"uid"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "bad_params")
		return
	}
	if req.State == nil {
		errJSON(w, http.StatusBadRequest, "missing required field: state", "bad_params")
		return
	}
	var qs map[string]any
	if len(req.Questions) > 0 {
		_ = json.Unmarshal(req.Questions, &qs)
	}
	if len(qs) == 0 {
		errJSON(w, http.StatusBadRequest, "missing required field: questions", "bad_params")
		return
	}
	params := map[string]any{"state": *req.State, "questions": json.RawMessage(req.Questions)}
	if req.Model != "" {
		params["model"] = req.Model
	}
	h.callAbility(w, r, "systemone", req.ChannelID, req.UID, params)
}

// imagesGenerations POST /v1/images/generations：文生图（也接受参考图数组）。
func (h *Handler) imagesGenerations(w http.ResponseWriter, r *http.Request) {
	body, ok := readLimited(w, r, imagesBodyLimit)
	if !ok {
		return
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "bad_params")
		return
	}
	h.imagesFromJSON(w, r, req)
}

// imagesEdits POST /v1/images/edits：OpenAI 形状用 multipart（image / image[]），
// 也兼容直接发 JSON（images 数组）。multipart 的图片元素按出现顺序编号为 reference1..N。
func (h *Handler) imagesEdits(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/") {
		save := r.Body
		// 请求体上限交给 MaxBytesReader：参考图 base64 前是原始字节，多张累计超限即 400。
		r.Body = http.MaxBytesReader(w, save, imagesBodyLimit)
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			errJSON(w, http.StatusBadRequest, "解析 multipart 失败（或超出 32MiB 上限）: "+err.Error(), "bad_params")
			return
		}
		req := map[string]json.RawMessage{}
		for _, k := range []string{"prompt", "n", "size", "aspect_ratio", "response_format", "output_format", "model", "channel_id", "uid"} {
			if v := r.FormValue(k); v != "" {
				b, _ := json.Marshal(v)
				req[k] = b
			}
		}
		// 参考图：image 与 image[] 都要吃（OpenAI SDK 用 image[]，curl 常用多个 image）
		var images []any
		for _, key := range []string{"image", "image[]"} {
			for _, fh := range r.MultipartForm.File[key] {
				f, err := fh.Open()
				if err != nil {
					errJSON(w, http.StatusBadRequest, "读取上传图片失败: "+err.Error(), "bad_params")
					return
				}
				data, err := io.ReadAll(f)
				f.Close()
				if err != nil {
					errJSON(w, http.StatusBadRequest, "读取上传图片失败: "+err.Error(), "bad_params")
					return
				}
				mimeType := fh.Header.Get("Content-Type")
				if mimeType == "" {
					mimeType = mimeByExt(fh.Filename)
				}
				// IPC 里绝不出现本地路径（W3-30）：统一转成 b64 对象形态
				images = append(images, map[string]any{
					"b64_json":  base64.StdEncoding.EncodeToString(data),
					"mime_type": mimeType,
					"name":      fh.Filename,
				})
			}
		}
		if len(images) > 0 {
			b, _ := json.Marshal(images)
			req["images"] = b
		}
		h.imagesFromJSON(w, r, req)
		return
	}
	body, ok := readLimited(w, r, imagesBodyLimit)
	if !ok {
		return
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid JSON body: "+err.Error(), "bad_params")
		return
	}
	h.imagesFromJSON(w, r, req)
}

// imagesFromJSON 校验必填并透传：prompt 非空是唯一硬校验，其余交给插件（W3-30 校验分界）。
func (h *Handler) imagesFromJSON(w http.ResponseWriter, r *http.Request, req map[string]json.RawMessage) {
	var prompt string
	if raw, ok := req["prompt"]; ok {
		_ = json.Unmarshal(raw, &prompt)
	}
	if strings.TrimSpace(prompt) == "" {
		errJSON(w, http.StatusBadRequest, "missing required field: prompt", "bad_params")
		return
	}
	// OpenAI Images 兼容：请求侧叫 `image` / `image[]`，契约叫 `images`（W3-30 单一契约）。
	// 不归一就会被当成「无参考图」⇒ 静默走文生图，参考图整个丢掉。
	if _, ok := req["images"]; !ok {
		var merged []json.RawMessage
		for _, k := range []string{"image", "image[]"} {
			if raw, ok := req[k]; ok {
				var arr []json.RawMessage
				if json.Unmarshal(raw, &arr) == nil {
					merged = append(merged, arr...)
				} else {
					merged = append(merged, raw)
				}
			}
		}
		if len(merged) > 0 {
			if b, err := json.Marshal(merged); err == nil {
				req["images"] = b
			}
		}
	}
	// 参考图体积快速失败：JSONL 单帧 64MB、base64 膨胀 4/3 → 原始字节合计限 24MB
	// （W3-30 §5.8.2）。超限回 bad_params，不做任何静默丢弃。
	if raw, ok := req["images"]; ok {
		var imgs []json.RawMessage
		if json.Unmarshal(raw, &imgs) == nil {
			var b64Bytes int
			for _, im := range imgs {
				var s string
				if json.Unmarshal(im, &s) == nil && strings.HasPrefix(s, "data:") {
					b64Bytes += len(s) // data URI 已是 base64 文本，长度即上行体积
					continue
				}
				var o struct {
					B64 string `json:"b64_json"`
				}
				if json.Unmarshal(im, &o) == nil {
					b64Bytes += len(o.B64)
				}
			}
			const maxRefB64 = 24 << 20 // 原始≈24MB 时 base64 约 32MB
			if b64Bytes > maxRefB64 {
				errJSON(w, http.StatusBadRequest,
					fmt.Sprintf("参考图体积超出上限（base64 合计最多 %d MiB，当前 %.1f MiB）", maxRefB64>>20, float64(b64Bytes)/(1<<20)),
					"bad_params")
				return
			}
		}
	}
	params := map[string]any{}
	for k, v := range req {
		switch k {
		case "channel_id", "uid", "model", "image", "image[]":
			continue // 路由/账号键与 OpenAI 兼容别名不进业务参数
		}
		params[k] = v
	}
	var chID, uid string
	_ = json.Unmarshal(req["channel_id"], &chID)
	_ = json.Unmarshal(req["uid"], &uid)
	h.callAbility(w, r, "images", chID, uid, params)
}

// callAbility 把请求转给 core 管理面端点，并把 core 的错误体原样回给客户端
// （core 已按 kind 映射好 HTTP 状态与文案，gateway 不重复判）。
func (h *Handler) callAbility(w http.ResponseWriter, r *http.Request, name, channelID, uid string, params map[string]any) {
	if h.CoreBase == "" {
		errJSON(w, http.StatusServiceUnavailable, "core 未就绪", "unavailable")
		return
	}
	if channelID == "" {
		// 未指定渠道：交给 core 用默认渠道（与对话链路的默认渠道语义一致）
		channelID = h.defaultChannel(r.Context())
	}
	body, _ := json.Marshal(map[string]any{
		"channel_id": channelID, "uid": uid, "params": params,
	})
	ep := "/api/systemone"
	if name == "images" {
		ep = "/api/images/generate"
	}
	ctx, cancel := context.WithTimeout(r.Context(), abilityTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.CoreBase+ep, bytes.NewReader(body))
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		errJSON(w, http.StatusBadGateway, "core 调用失败: "+err.Error(), "upstream_error")
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode >= 300 {
		// core 的错误体是 ww3 内部形状 {ok,error,kind,scope}；/v1/* 是对外 OpenAI 兼容面，
		// 必须统一成 {"error":{message,type,code}}（OpenAI SDK 只认这个），同时保留 kind/scope 供 ww3 客户端。
		var ce struct {
			Error string `json:"error"`
			Kind  string `json:"kind"`
			Scope string `json:"scope"`
		}
		_ = json.Unmarshal(respBody, &ce)
		if ce.Error != "" {
			kind := ce.Kind
			if kind == "" {
				kind = "upstream_error"
			}
			e := map[string]any{"message": ce.Error, "type": kind, "code": kind}
			if ce.Scope != "" {
				e["scope"] = ce.Scope
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(resp.StatusCode)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": e})
			return
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// defaultChannel 取 core 的默认渠道（未配置时返回空串 → core 自己报错）。
func (h *Handler) defaultChannel(ctx context.Context) string {
	if h.CoreBase == "" {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.CoreBase+"/api/state", nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var v struct {
		DefaultChannel string `json:"default_channel"`
	}
	if json.NewDecoder(resp.Body).Decode(&v) != nil {
		return ""
	}
	return v.DefaultChannel
}

// readLimited 读请求体并强制上限；超限直接 400（不静默截断）。
func readLimited(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		errJSON(w, http.StatusBadRequest, "读取请求体失败: "+err.Error(), "bad_params")
		return nil, false
	}
	if int64(len(body)) > limit {
		errJSON(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("请求体超出上限（%d MiB）", limit>>20), "bad_params")
		return nil, false
	}
	return body, true
}

// mimeByExt 从文件名推 MIME（推不出返回空串，由插件按内容判断，绝不猜错）。
func mimeByExt(name string) string {
	switch strings.ToLower(name[strings.LastIndex(name, ".")+1:]) {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	case "gif":
		return "image/gif"
	case "bmp":
		return "image/bmp"
	}
	return ""
}
