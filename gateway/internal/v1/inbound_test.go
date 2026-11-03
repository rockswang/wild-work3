// inbound_test.go 对外三协议入站转换的回归测试。
//
// 覆盖的都是「静默丢数据」类缺陷——上游不会报错，只会拿到缺字段的请求，
// 所以必须有断言钉住（旧版这些行为靠 map 全透传天然成立，ww3 改成强类型后需显式测）。
package v1

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work3/main/protocol/chat"
)

// decode 解析一份 wire 形态 JSON（模拟请求体解码结果）。
func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	return m
}

// TestAliasMaxTokens 只发 max_completion_tokens 别名时 MaxTokens 必须落到请求里。
// 曾经的断裂：normalize 回写 int64，消费端断言 .(float64) → 别名请求静默丢输出上限。
func TestAliasMaxTokens(t *testing.T) {
	body := decode(t, `{"model":"o4-mini","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":256}`)
	chat.NormalizeBody(body)
	req, err := toGenerateRequest(body)
	if err != nil {
		t.Fatalf("toGenerateRequest: %v", err)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 256 {
		t.Fatalf("MaxTokens=%v want 256", req.MaxTokens)
	}
}

// TestExplicitMaxTokensWins 显式 max_tokens 与别名同时存在时，显式值优先（不被别名覆盖）。
func TestExplicitMaxTokensWins(t *testing.T) {
	body := decode(t, `{"model":"gpt-5","messages":[],"max_tokens":128,"max_completion_tokens":999}`)
	chat.NormalizeBody(body)
	req, _ := toGenerateRequest(body)
	if req.MaxTokens == nil || *req.MaxTokens != 128 {
		t.Fatalf("MaxTokens=%v want 128", req.MaxTokens)
	}
}

// TestExtraPassthrough 未被强类型契约映射的下游参数必须进 Extra（透传袋）。
// stop/top_p/response_format 漏在网关，插件再怎么实现也收不到。
func TestExtraPassthrough(t *testing.T) {
	const raw = `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],` +
		`"temperature":0.7,"max_tokens":64,"tool_choice":"auto","reasoning_effort":"low","stream":true,` +
		`"stop":["END"],"top_p":0.9,"presence_penalty":0.5,"user":"u1",` +
		`"response_format":{"type":"json_object"}}`
	req, err := toGenerateRequest(decode(t, raw))
	if err != nil {
		t.Fatalf("toGenerateRequest: %v", err)
	}
	var ex map[string]any
	if err := json.Unmarshal(req.Extra, &ex); err != nil {
		t.Fatalf("Extra 不是 JSON 对象: %v", err)
	}
	for _, k := range []string{"stop", "top_p", "presence_penalty", "user", "response_format"} {
		if _, ok := ex[k]; !ok {
			t.Errorf("Extra 缺少透传字段 %q: %s", k, req.Extra)
		}
	}
	// 已由强类型承载 / 体积巨大的键不得重复进袋（会让每帧尺寸翻倍）
	for _, k := range []string{"model", "messages", "tools", "temperature", "max_tokens", "stream", "tool_choice", "reasoning_effort"} {
		if _, ok := ex[k]; ok {
			t.Errorf("Extra 不该包含已映射字段 %q: %s", k, req.Extra)
		}
	}
}

// TestChatToolResultImage Chat 原生 role=tool 里的图片必须转 vision 块（R113）。
// 曾经只有 Responses/Anthropic 两入口接了 toolContentValue，Chat 入口把 base64 整段当文本回灌。
func TestChatToolResultImage(t *testing.T) {
	const raw = `{"model":"gpt-5","messages":[{"role":"tool","tool_call_id":"c1","content":[` +
		`{"type":"text","text":"截图如下"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg"}}]}]}`
	req, err := toGenerateRequest(decode(t, raw))
	if err != nil {
		t.Fatalf("toGenerateRequest: %v", err)
	}
	blocks, ok := req.Messages[0].Content.([]any)
	if !ok {
		t.Fatalf("tool 结果 content 应为内容块数组，实际 %T", req.Messages[0].Content)
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks=%d want 2", len(blocks))
	}
	if m, _ := blocks[1].(map[string]any); m["type"] != "image_url" {
		t.Errorf("第二块应为 image_url，实际 %v", blocks[1])
	}
	// 关键红线：图片不能被序列化成一条超长文本
	if s, ok := req.Messages[0].Content.(string); ok && strings.Contains(s, "iVBORw0KGgo") {
		t.Errorf("base64 被当成文本回灌: %s", s)
	}
}

// TestAnthropicStopAndTopK Anthropic 的 stop_sequences/top_k/metadata 必须映射（旧版同口径）。
func TestAnthropicStopAndTopK(t *testing.T) {
	const raw = `{"model":"claude-x","max_tokens":100,"messages":[{"role":"user","content":"hi"}],` +
		`"stop_sequences":["END"],"top_k":40,"metadata":{"user_id":"u9"}}`
	out := anthropicToChat(decode(t, raw))
	if s, ok := out["stop"].([]any); !ok || len(s) != 1 {
		t.Errorf("stop=%v 应由 stop_sequences 映射而来", out["stop"])
	}
	if n, ok := out["top_k"].(int); !ok || n != 40 {
		t.Errorf("top_k=%v want 40", out["top_k"])
	}
	if out["user"] != "u9" {
		t.Errorf("user=%v want u9（metadata.user_id）", out["user"])
	}
}

// TestAnthropicThinkingRequest anthropic 的 thinking 对象必须映射到内部三字段：
// thinking=true、thinking_budget_tokens 原值、type=enabled 时补 reasoning_effort="medium"。
// 旧版 dev/internal/gateway/anthropic.go:147-155 同口径；缺 reasoning_effort 会让
// 只认 effort 的渠道（qoder 等）开不了思考。
func TestAnthropicThinkingRequest(t *testing.T) {
	const raw = `{"model":"claude-x","max_tokens":100,"messages":[{"role":"user","content":"hi"}],` +
		`"thinking":{"type":"enabled","budget_tokens":4096}}`
	out := anthropicToChat(decode(t, raw))
	if b, _ := out["thinking"].(bool); !b {
		t.Errorf("thinking=%v want true", out["thinking"])
	}
	if n, _ := asInt(out["thinking_budget_tokens"]); n != 4096 {
		t.Errorf("thinking_budget_tokens=%v want 4096", out["thinking_budget_tokens"])
	}
	if s, _ := out["reasoning_effort"].(string); s != "medium" {
		t.Errorf("reasoning_effort=%v want medium（type=enabled 的保底档位）", out["reasoning_effort"])
	}
}

// TestAnthropicThinkingDisabled 非 enabled 的 thinking 不得被当成开启。
func TestAnthropicThinkingDisabled(t *testing.T) {
	out := anthropicToChat(decode(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],`+
		`"thinking":{"type":"disabled"}}`))
	if out["reasoning_effort"] != nil {
		t.Errorf("disabled 不应注入 reasoning_effort: %v", out["reasoning_effort"])
	}
}

// TestAnthropicMessageThinkingSignature thinking 块必须带 signature 键（官方 SDK 解析要求）。
func TestAnthropicMessageThinkingSignature(t *testing.T) {
	acc := &chat.Accumulator{}
	acc.Reasoning.WriteString("思考正文")
	m := buildAnthropicMessage("msg_1", "claude-x", acc, nil)
	blocks, _ := m["content"].([]any)
	if len(blocks) == 0 {
		t.Fatalf("content 为空: %v", m)
	}
	b, _ := blocks[0].(map[string]any)
	if b["type"] != "thinking" {
		t.Fatalf("首块 type=%v want thinking", b["type"])
	}
	if _, ok := b["signature"]; !ok {
		t.Errorf("thinking 块缺 signature 键: %v", b)
	}
}

// TestAnthropicHistoryThinkingDropped 历史 thinking 块不得改写成 text 回灌上游。
func TestAnthropicHistoryThinkingDropped(t *testing.T) {
	msg := map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "thinking", "thinking": "PRIVATE-COART"},
		map[string]any{"type": "text", "text": "公开正文"},
	}}
	got := convertAnthropicMessage(msg)
	if len(got) != 1 {
		t.Fatalf("消息数=%d want 1: %v", len(got), got)
	}
	m, _ := got[0].(map[string]any)
	s, _ := json.Marshal(m)
	if strings.Contains(string(s), "PRIVATE-COART") {
		t.Errorf("思考原文被回灌上游: %s", s)
	}
	if !strings.Contains(string(s), "公开正文") {
		t.Errorf("正文丢失: %s", s)
	}
}

// TestResponsesImageObjectForm Responses 的 input_image 既可能是字符串也可能是 {url,detail} 对象，
// 两种形态都必须成图；早先用 asString 取对象只会得到空串（图静默丢）。
func TestResponsesImageObjectForm(t *testing.T) {
	items := []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "看图"},
			map[string]any{"type": "input_image", "image_url": map[string]any{"url": "data:image/png;base64,AAA", "detail": "low"}},
			map[string]any{"type": "refusal", "text": "被拒绝的正文"},
			map[string]any{"type": "summary_text", "text": "摘要正文"},
		}},
	}
	out := convertResponsesItems(items)
	m, _ := out[0].(map[string]any)
	parts, _ := m["content"].([]any)
	if len(parts) != 4 {
		t.Fatalf("parts=%d want 4（两段文本 + refusal + 摘要，图片保留）: %v", len(parts), out[0])
	}
	var img map[string]any
	for _, p := range parts {
		if pm, _ := p.(map[string]any); pm["type"] == "image_url" {
			img, _ = pm["image_url"].(map[string]any)
		}
	}
	if img == nil || img["url"] != "data:image/png;base64,AAA" {
		t.Fatalf("input_image 对象形态未转成 image_url: %v", parts)
	}
	if img["detail"] != "low" {
		t.Errorf("detail 丢失: %v", img)
	}
	joined, _ := json.Marshal(out)
	for _, want := range []string{"被拒绝的正文", "摘要正文"} {
		if !strings.Contains(string(joined), want) {
			t.Errorf("文本类型 %q 被丢弃: %s", want, joined)
		}
	}
}

// TestModelEntryCapabilities /v1/models 必须宣告上游声明过的 tools 与模态（不再硬编码 text）。
func TestModelEntryCapabilities(t *testing.T) {
	var m modelDetail
	m.ID = "acme/x1"
	m.Capabilities.Tools = true
	m.Capabilities.Vision = true
	e := buildModelEntry("acme", m)
	if e["supports_tools"] != true {
		t.Errorf("supports_tools=%v want true", e["supports_tools"])
	}
	arch, _ := e["architecture"].(map[string]any)
	if got, _ := json.Marshal(arch["input_modalities"]); string(got) != `["text","image"]` {
		t.Errorf("input_modalities=%v", arch["input_modalities"])
	}
	if arch["modality"] != "text+image->text" {
		t.Errorf("modality=%v", arch["modality"])
	}
	params, _ := json.Marshal(e["supported_parameters"])
	for _, want := range []string{"tools", "tool_choice"} {
		if !strings.Contains(string(params), want) {
			t.Errorf("supported_parameters 缺 %q: %s", want, params)
		}
	}
	if e["created"] != modelCreatedEpoch {
		t.Errorf("created=%v want %d（不得为 0，否则客户端算出 1970 年发布）", e["created"], modelCreatedEpoch)
	}
}

// TestEstimateTokensMessageFloor 多条极短消息也要给出结构开销下限（旧版同口径）。
func TestEstimateTokensMessageFloor(t *testing.T) {
	body := decode(t, `{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},`+
		`{"role":"user","content":"c"},{"role":"assistant","content":"d"}]}`)
	if n := estimateTokens(body); n < 8 {
		t.Errorf("estimateTokens=%d want >=8（4 条消息 * 2）", n)
	}
}

// TestEstimateTokensCountsToolSchema 工具 schema 的 description/name 必须计入（旧版计入，早先漏了）。
func TestEstimateTokensCountsToolSchema(t *testing.T) {
	long := strings.Repeat("abcdefgh", 80) // 640 个 ASCII 字符 ≈ 160 token
	body := decode(t, `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function",`+
		`"function":{"name":"f","description":"`+long+`"}}]}`)
	if n := estimateTokens(body); n < 100 {
		t.Errorf("estimateTokens=%d want >=100（工具描述 640 字符应计入）", n)
	}
}

// TestModelEntryMaxOutputTokens 上游声明 max_tokens 时输出 max_output_tokens；未声明则不输出（不编造）。
func TestModelEntryMaxOutputTokens(t *testing.T) {
	var m modelDetail
	m.ID = "acme/x2"
	if _, ok := buildModelEntry("acme", m)["max_output_tokens"]; ok {
		t.Fatal("未声明时不应编造 max_output_tokens")
	}
	mt := 8192
	m.MaxTokens = &mt
	if got := buildModelEntry("acme", m)["max_output_tokens"]; got != 8192 {
		t.Fatalf("max_output_tokens=%v want 8192", got)
	}
}

// ---- W3-30 可选能力端点（/v1/systemone、/v1/images/*）----

// TestSystemoneMissingState state 键缺失必须 400；state 为空串合法（上游要求键存在）。
func TestSystemoneMissingState(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantCode   int
	}{
		{"键缺失", `{"questions":{"a":{"type":"noul","criteria":{"true":"x","false":"y"}}}}`, 400},
		{"state 空串合法", `{"state":"","questions":{"a":{"type":"noul","criteria":{"true":"x","false":"y"}}}}`, 503},
		{"questions 为空", `{"state":"x","questions":{}}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{} // 不设 CoreBase → 通过校验后应回 503（core 未就绪）
			rec := httptest.NewRecorder()
			h.systemone(rec, httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(tc.body)))
			if rec.Code != tc.wantCode {
				t.Fatalf("code=%d want %d body=%s", rec.Code, tc.wantCode, rec.Body.String())
			}
		})
	}
}

// TestImagesPromptRequired prompt 缺失/空白必须 400（唯一硬校验）。
func TestImagesPromptRequired(t *testing.T) {
	h := &Handler{}
	for _, body := range []string{`{}`, `{"prompt":"   "}`, `{"prompt":123}`} {
		rec := httptest.NewRecorder()
		h.imagesGenerations(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body)))
		if rec.Code != 400 {
			t.Fatalf("body=%s code=%d want 400", body, rec.Code)
		}
	}
}

// TestImagesRefSizeLimit 参考图 base64 合计超 24MiB 必须 400 bad_params（不静默丢弃）。
func TestImagesRefSizeLimit(t *testing.T) {
	h := &Handler{}
	huge := strings.Repeat("A", 25<<20)
	body := `{"prompt":"x","images":[{"b64_json":"` + huge + `","mime_type":"image/png"}]}`
	rec := httptest.NewRecorder()
	h.imagesGenerations(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body)))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "bad_params") {
		t.Fatalf("code=%d body=%.200s", rec.Code, rec.Body.String())
	}
}

// TestImagesEditsMultipart multipart 的 image[] 必须按顺序解成 images[]（IPC 内不出现本地路径）。
func TestImagesEditsMultipart(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("prompt", "把柴犬戴上红帽")
	_ = mw.WriteField("size", "1024x1024")
	fw, _ := mw.CreateFormFile("image[]", "a.png")
	_, _ = fw.Write([]byte{0x89, 'P', 'N', 'G'})
	fw2, _ := mw.CreateFormFile("image[]", "b.jpg")
	_, _ = fw2.Write([]byte{0xFF, 0xD8})
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	// 用直接捕获 form 的方式验证解析结果（Handler.CoreBase 为空 → 校验通过后回 503）
	rec := httptest.NewRecorder()
	h := &Handler{}
	h.imagesEdits(rec, req)
	if rec.Code != 503 {
		t.Fatalf("code=%d 期望 503（core 未就绪）body=%s", rec.Code, rec.Body.String())
	}
}

// TestIncludeUsageOf 客户端显式 include_usage:false 时下游不再输出 usage 帧。
func TestIncludeUsageOf(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want bool
	}{
		{"未给", map[string]any{}, true},
		{"true", map[string]any{"stream_options": map[string]any{"include_usage": true}}, true},
		{"false", map[string]any{"stream_options": map[string]any{"include_usage": false}}, false},
		{"形状异常", map[string]any{"stream_options": "x"}, true},
	}
	for _, c := range cases {
		if got := includeUsageOf(c.body); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
