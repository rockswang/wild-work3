package channel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"wild-work3/main/protocol/errkind"
	"wild-work3/main/protocol/meta"
)

// ModelsResult 是命令行拉模型的返回。
type ModelsResult struct {
	Models []meta.ModelInfo
	Err    error
}

// ListModelsCLI 供命令行 `models` 命令直接调用。
func ListModelsCLI(ctx context.Context, cfg Config) ModelsResult {
	ms, err := listModels(ctx, cfg)
	return ModelsResult{Models: ms, Err: err}
}

// listModels 调用上游 GET /models 拉取模型列表。
func listModels(ctx context.Context, cfg Config) ([]meta.ModelInfo, error) {
	if cfg.BaseURL == "" || cfg.APIKey == "" {
		return nil, &StatusError{Kind: errkind.BadParams, Scope: errkind.ScopeChannel, Msg: "未配置 base_url 或 api_key"}
	}
	timeout := time.Duration(cfg.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.BaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, &StatusError{Kind: errkind.BadParams, Scope: errkind.ScopeChannel, Msg: "构造请求失败: " + err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	c, err := newHTTPClient(cfg, timeout)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, &StatusError{Kind: errkind.Conn, Scope: errkind.ScopeChannel, Msg: "上游连接失败: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		kind, scope := classifyHTTP(resp.StatusCode, msg)
		return nil, &StatusError{Kind: kind, Scope: scope, Msg: "上游 " + itoa(resp.StatusCode) + ": " + truncate(string(msg), 300)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, probeBodyCap+1))
	if err != nil {
		return nil, &StatusError{Kind: errkind.Conn, Scope: errkind.ScopeChannel, Msg: "读取响应失败: " + err.Error()}
	}
	if len(body) > probeBodyCap {
		return nil, &StatusError{Kind: errkind.Passthrough, Scope: errkind.ScopeChannel, Msg: "上游响应超过 8 MiB，已放弃解析"}
	}
	// 先用宽容探测（W3-32）：能拿到 context / 思考档位就一并带上，拿不到就是 nil（不编造）。
	// 旧版 dev/internal/custom/probe.go 就是这个口径，新版早期实现只取了 id。
	ms, perr := parseModelInfos(body)
	if perr != nil {
		return nil, &StatusError{Kind: errkind.Passthrough, Scope: errkind.ScopeChannel, Msg: "响应解析失败: " + perr.Error()}
	}
	// 上游未提供时回退到配置里手写的模型清单
	if len(ms) == 0 && len(cfg.Models) > 0 {
		for _, m := range cfg.Models {
			ms = append(ms, meta.ModelInfo{ID: m, Name: m})
		}
	}
	return ms, nil
}

// probeBodyCap 上游 /models 响应体上限（8 MiB）。
// 早先限 1 MiB，而 OpenRouter 这类大目录上游实测 3~4 MiB，截断后 JSON 必报
// 「不是合法 JSON」——这就是旧版「点导入模型不是每次都能列出列表」的真凶（R122）。
const probeBodyCap = 8 << 20

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// handleModelsList 控制面 models.list：从上游 /models 拉取（config 由 core 传入）。
func handleModelsList(ctx context.Context, p Raw) (Raw, *Err) {
	var params struct {
		Config Config `json:"config"`
	}
	_ = json.Unmarshal(p, &params)
	ms, err := listModels(ctx, params.Config)
	if err != nil {
		kind, scope := errKindOf(err)
		return nil, errf(kind, scope, "拉取模型失败: %v", err)
	}
	return mustJSON(map[string]any{"models": ms}), nil
}
