package channel

import (
	"encoding/json"

	"wild-work3/main/protocol/errkind"
	"wild-work3/main/protocol/jsonl"
)

// Raw / Err 是 main/protocol/jsonl 的类型别名：方法签名里出现十几次，
// 用短名保持一屏内可读（两种运行形态共用同一份方法表）。
type Raw = jsonl.RawMessage
type Err = jsonl.ErrObj

// mustJSON 序列化失败即 panic（plugin.Registry 会把 panic 转成 server 类错误）。
func mustJSON(v any) Raw {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func jsonMarshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// errf 构造协议错误对象（短名，减少方法表里的视觉噪声）。
func errf(kind errkind.Kind, scope errkind.Scope, format string, a ...any) *Err {
	return jsonl.Errorf(kind, scope, format, a...)
}
