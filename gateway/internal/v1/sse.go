package v1

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
)

// sseWriter 惰性写 SSE 响应头 + 统一帧序列化。
//
// 惰性的意义：模型降级结果要等 core 的 `model.fallback` 事件才知道，而
// `X-Model-Fallback` 一旦写出就改不了。把 header 推迟到「首帧写出」那一刻，
// 就能在流开始前把降级头补上（降级必留痕，旧版模型降级链规则 3）。
type sseWriter struct {
	w       http.ResponseWriter
	fl      http.Flusher
	mu      sync.Mutex
	hdrs    [][2]string // start 之前累积的额外头
	started bool
	wrote   bool // 是否已写出过任何帧（判「已发出部分响应」，W3-10）
}

func newSSE(w http.ResponseWriter) *sseWriter {
	fl, _ := w.(http.Flusher)
	return &sseWriter{w: w, fl: fl}
}

// setHeader 在响应头写出前设置额外头；已开始写帧则忽略（改了也不会生效）。
func (s *sseWriter) setHeader(k, v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return
	}
	s.hdrs = append(s.hdrs, [2]string{k, v})
}

func (s *sseWriter) startLocked() {
	if s.started {
		return
	}
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	for _, kv := range s.hdrs {
		h.Set(kv[0], kv[1])
	}
}

// wroteAny 报告是否已写出过帧。
func (s *sseWriter) wroteAny() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wrote
}

// frame 写一帧。event 为空时只写 data 行（OpenAI Chat 风格），
// 非空时写 `event: <name>` + data（Anthropic / Responses 风格）。
func (s *sseWriter) frame(event string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startLocked()
	if event != "" {
		fmt.Fprintf(s.w, "event: %s\n", event)
	}
	fmt.Fprintf(s.w, "data: %s\n\n", b)
	if s.fl != nil {
		s.fl.Flush()
	}
	s.wrote = true
}

// raw 写一段原样文本（如 `data: [DONE]`）。
func (s *sseWriter) raw(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startLocked()
	fmt.Fprint(s.w, text)
	if s.fl != nil {
		s.fl.Flush()
	}
	s.wrote = true
}
