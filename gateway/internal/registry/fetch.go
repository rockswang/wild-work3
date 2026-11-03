// 多源并发竞速下载（W3-38 定案 3）。
//
// 与旧版 PR188 的关键差异：PR188 是**串行候选链**（第一个不报错的赢），
// 于是「连上了但 83KB/s 龟速」这种软失败会把请求永久卡在 downloading（实测 gh-proxy 12s 只走 994KB/18.6MB）。
// 这里所有候选源同时开跑，用三条判据主动弃源，谁先完整且校验通过谁赢：
//
//	首字节超时（缺省 8s）：连上了但一直不吐第一个字节 → 弃
//	读空闲（缺省 15s）：吐着吐着停了 → 弃（比整体 Timeout 早得多地发现僵流）
//	吞吐过低（缺省 10s 窗口内 <20KB/s）：能连但慢得不可用 → 弃
//
// 弃掉的源保留半包 tmp，下一轮带 Range 续传（实测 gh-proxy/ghfast 都回 206+Content-Range）。
package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// 判据缺省值（可被 Options 覆盖）。
const (
	defFirstByte = 8 * time.Second
	defIdle      = 15 * time.Second
	defStallWin  = 10 * time.Second
	defMinRate   = 20 * 1024 // B/s
	defMaxBody   = 8 << 20   // 清单类小文件上限
	defTotal     = 10 * time.Minute
	readChunk    = 64 * 1024
	userAgent    = "wild-work3-updater"
	ProbeBytes   = 64 << 10 // 提示「可更新」前的 Range 探包字节数
)

// Options 竞速参数。零值即用缺省。
type Options struct {
	Proxies   []string      // 代理前缀（如 https://gh-proxy.com/）
	FirstByte time.Duration // 0 → 8s
	Idle      time.Duration // 0 → 15s
	StallWin  time.Duration // 0 → 10s
	MinRate   int64         // 0 → 20KB/s
	MaxBody   int64         // 0 → 8MiB（仅 Fetch/Probe 用）
	Total     time.Duration // 0 → 10min
	Client    *http.Client  // nil → 新建（不设 Timeout，靠 ctx + 三条判据；nil Transport 以继承环境代理）
}

func (o Options) withDefaults() Options {
	c := o
	if c.FirstByte <= 0 {
		c.FirstByte = defFirstByte
	}
	if c.Idle <= 0 {
		c.Idle = defIdle
	}
	if c.StallWin <= 0 {
		c.StallWin = defStallWin
	}
	if c.MinRate <= 0 {
		c.MinRate = defMinRate
	}
	if c.MaxBody <= 0 {
		c.MaxBody = defMaxBody
	}
	if c.Total <= 0 {
		c.Total = defTotal
	}
	if c.Client == nil {
		c.Client = &http.Client{}
	}
	return c
}

// Progress 下载进度回调。
type Progress func(src string, done, total int64, rateBps float64)

// ErrAllSources 全部候选源失败（带逐源原因，便于反查是哪个镜像 rot 了）。
type ErrAllSources struct{ Reasons []string }

func (e *ErrAllSources) Error() string {
	return "所有源均失败: " + strings.Join(e.Reasons, "; ")
}

// Fetch 竞速取一个小文件（清单/签名），返回内容与命中的源 URL。
func Fetch(ctx context.Context, g GitHubRelease, o Options) ([]byte, string, error) {
	return FetchURLs(ctx, g.Sources(o.Proxies), o)
}

// FetchURLs 与 Fetch 同，但源集合由调用方展开（绝对 URL 场景）。
func FetchURLs(ctx context.Context, urls []string, o Options) ([]byte, string, error) {
	opt := o.withDefaults()
	if len(urls) == 0 {
		return nil, "", errors.New("源集合为空")
	}
	ctx, cancel := context.WithTimeout(ctx, opt.Total)
	defer cancel()
	type res struct {
		body []byte
		src  string
		err  error
	}
	ch := make(chan res, len(urls))
	var wg sync.WaitGroup
	for _, u := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			w := &limitWriter{cap: opt.MaxBody}
			_, err := pull(ctx, opt, u, "", w, nil)
			if err == nil {
				w.b = w.buf.Bytes()
			}
			ch <- res{w.b, u, err}
		}(u)
	}
	go func() { wg.Wait(); close(ch) }()
	var errs []string
	for r := range ch {
		if r.err == nil {
			cancel() // 我赢了，其余作废
			return r.body, r.src, nil
		}
		errs = append(errs, shortURL(r.src)+": "+r.err.Error())
	}
	return nil, "", &ErrAllSources{Reasons: errs}
}

// Probe 用 Range 探目标资产前 64KB，确认可达且不太慢再提示「可更新」。
// 依据：实测 64B/64KB 小请求三源全通（1.3–1.5s），而 18MB 整包经 gh-proxy 12s 只走 994KB。
func Probe(ctx context.Context, g GitHubRelease, o Options) (int64, string, error) {
	return ProbeURLs(ctx, g.Sources(o.Proxies), o)
}

// ProbeURLs 与 Probe 同，但源集合由调用方给出（测试/绝对 URL 场景）。
func ProbeURLs(ctx context.Context, urls []string, o Options) (int64, string, error) {
	opt := o.withDefaults()
	if len(urls) == 0 {
		return 0, "", errors.New("源集合为空")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	type res struct {
		n   int64
		src string
		err error
	}
	ch := make(chan res, len(urls))
	var wg sync.WaitGroup
	for _, u := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			n, err := pullRange(ctx, opt, u, ProbeBytes)
			ch <- res{n, u, err}
		}(u)
	}
	go func() { wg.Wait(); close(ch) }()
	var errs []string
	for r := range ch {
		if r.err == nil && r.n > 0 {
			cancel()
			return r.n, r.src, nil
		}
		if r.err != nil {
			errs = append(errs, shortURL(r.src)+": "+r.err.Error())
		}
	}
	if len(errs) == 0 {
		return 0, "", errors.New("所有源都返回空体")
	}
	return 0, "", &ErrAllSources{Reasons: errs}
}

// pullRange 取前 n 字节（只做可达性/速度判断，不落盘）。
func pullRange(ctx context.Context, o Options, url string, n int) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", n-1))
	resp, err := o.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, int64(n)))
	return int64(len(b)), err
}

// FetchFile 竞速下载一个包到 dest，校验 sha256（必填）。
// 每源各写自己的 tmp（`dest.<i>`，避免并发写同一文件互相截断），赢家 rename、输家删；
// 失败的源保留半包 tmp 供下一轮 Range 续传。
func FetchFile(ctx context.Context, urls []string, dest, wantSHA string, o Options, onProgress Progress) (string, error) {
	opt := o.withDefaults()
	if !isHexSHA(wantSHA) {
		return "", fmt.Errorf("sha256 缺失或格式错误（W3-38 起必填）: %q", wantSHA)
	}
	if len(urls) == 0 {
		return "", errors.New("源集合为空")
	}
	ctx, cancel := context.WithTimeout(ctx, opt.Total)
	defer cancel()
	type res struct {
		src string
		err error
	}
	ch := make(chan res, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			tmp := fmt.Sprintf("%s.%d", dest, i)
			err := pullFile(ctx, opt, u, tmp, wantSHA, onProgress)
			if err == nil {
				_ = os.Remove(dest)
				if rerr := os.Rename(tmp, dest); rerr != nil {
					err = fmt.Errorf("落盘失败: %w", rerr)
				}
			}
			// 失败的源：pullFile 已在 sha 不匹配时删 tmp；软失败保留半包供下一轮续传
			ch <- res{u, err}
		}(i, u)
	}
	go func() { wg.Wait(); close(ch) }()
	var errs []string
	for r := range ch {
		if r.err == nil {
			cancel() // 赢家产生，其余源作废（它们的 tmp 保留，下次可续传）
			return r.src, nil
		}
		errs = append(errs, shortURL(r.src)+": "+r.err.Error())
	}
	return "", &ErrAllSources{Reasons: errs}
}

// pullFile 把单个源下到 tmp（支持续传）并校验整包 sha256。
func pullFile(ctx context.Context, o Options, url, tmp, wantSHA string, onProgress Progress) error {
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	var off int64
	if st, _ := f.Stat(); st != nil {
		off = st.Size() // 半包续传起点
	}
	hw, err := newHashWriter(f, off)
	if err != nil {
		return err
	}
	if _, err := pull(ctx, o, url, tmp, hw, onProgress); err != nil {
		return err
	}
	if !strings.EqualFold(hw.sum(), wantSHA) {
		_ = os.Remove(tmp) // 内容不对，续传也没意义
		return fmt.Errorf("SHA256 mismatch from %s: 期望 %s 实际 %s", shortURL(url), wantSHA, hw.sum())
	}
	return nil
}

// pull 对单源发一次 GET（可带 Range），边读边写 w，并跑三条弃源判据。
// 返回写出的总字节数（含续传前缀）。
//
// 读体必须放在独立 goroutine：一开始写成「主循环直接 Read 后 select 定时器」，
// select 会在没有定时器到期时阻塞，于是**一个窗口只能读到一坨**（实测 httptest 的 ServeContent
// 每次只交 512B，健康源被误判成 1KB/s 吐掉）。现在数据与定时器在同一个 select 里争抢，
// 窗口内能连续收数据，判据才是真的「吞吐」而不是「轮询频率」。
func pull(ctx context.Context, o Options, url, tmp string, w io.Writer, onProgress Progress) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	var off int64
	if hw, ok := w.(*hashWriter); ok && hw.off > 0 {
		off = hw.off
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", off))
	}
	resp, err := o.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
	case http.StatusRequestedRangeNotSatisfiable:
		if tmp != "" {
			_ = os.Remove(tmp) // 本地半包比远端还长，删掉重来
		}
		return 0, errors.New("range 不可满足（本地半包长于远端资产）")
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	// 200 = 服务端不理 Range，只能整包重来：必须截断本地半包并回卷 hasher
	if resp.StatusCode == http.StatusOK && off > 0 {
		if hw, ok := w.(*hashWriter); ok {
			if err := hw.reset(); err != nil {
				return 0, err
			}
		}
		off = 0
	}
	total := resp.ContentLength
	if total >= 0 {
		total += off
	}

	// 只读不改的背压：两个缓冲乒乓，避免 18MB 包产生 18MB 垃圾
	type chunk struct {
		b   []byte
		err error
	}
	bufs := make(chan []byte, 2)
	bufs <- make([]byte, readChunk)
	bufs <- make([]byte, readChunk)
	ch := make(chan chunk, 2)
	go func() {
		defer close(ch)
		for b := range bufs {
			n, rerr := resp.Body.Read(b)
			if n > 0 {
				ch <- chunk{b: b[:n]}
			}
			if rerr != nil {
				ch <- chunk{err: rerr}
				return
			}
		}
	}()

	var (
		done     = off
		firstC   <-chan time.Time
		idleC    <-chan time.Time
		stallC   <-chan time.Time
		winStart = time.Now()
		winBytes int64
	)
	tFirst := time.NewTimer(o.FirstByte)
	defer tFirst.Stop()
	firstC = tFirst.C
	tIdle := time.NewTimer(o.Idle)
	defer tIdle.Stop()
	idleC = tIdle.C
	tStall := time.NewTicker(o.StallWin)
	defer tStall.Stop()
	stallC = tStall.C

	handleData := func(b []byte) error {
		if firstC != nil { // 首字节已到，停掉首字节判据（置 nil 让 select 忽略）
			tFirst.Stop()
			firstC = nil
		}
		if _, werr := w.Write(b); werr != nil {
			return werr
		}
		done += int64(len(b))
		winBytes += int64(len(b))
		resetTimer(tIdle, o.Idle)
		idleC = tIdle.C
		if onProgress != nil {
			secs := time.Since(winStart).Seconds()
			onProgress(url, done, total, float64(winBytes)/max(0.001, secs))
		}
		return nil
	}
	abort := func(format string, args ...any) (int64, error) {
		return done, fmt.Errorf(format, args...)
	}

	for {
		select {
		case c, ok := <-ch:
			if !ok { // 读体 goroutine 已退（理论上先到 err）
				return done, io.ErrUnexpectedEOF
			}
			if len(c.b) > 0 {
				if err := handleData(c.b); err != nil {
					return done, err
				}
				bufs <- c.b[:cap(c.b)] // 回坑位，接着读
			}
			if c.err != nil {
				if c.err == io.EOF {
					return done, nil
				}
				if ctx.Err() != nil {
					return done, ctx.Err() // 被赢家取消，不算自身失败
				}
				return done, c.err
			}
		case <-ctx.Done():
			return done, ctx.Err()
		case <-firstC:
			return abort("首字节超时（%v 内无数据）", o.FirstByte)
		case <-idleC:
			return abort("读空闲超时（%v 无新字节）", o.Idle)
		case <-stallC:
			secs := time.Since(winStart).Seconds()
			if firstC != nil || float64(winBytes)/secs < float64(o.MinRate) {
				return abort("吞吐过低（%.1fs 内 %.1fKB/s < %.1fKB/s）",
					secs, float64(winBytes)/1024/secs, float64(o.MinRate)/1024)
			}
			winStart, winBytes = time.Now(), 0
		}
	}
}

// limitWriter 收小文件到内存，超 cap 直接报错（防镜像返回超大 HTML 错误页把内存吃穿）。
type limitWriter struct {
	buf bytes.Buffer
	cap int64
	b   []byte
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if int64(w.buf.Len()+len(p)) > w.cap {
		return 0, fmt.Errorf("响应体超过上限 %d 字节（可能连到了 HTML 页面而非资产）", w.cap)
	}
	return w.buf.Write(p)
}

// hashWriter 边写文件边算整包 sha256。
// off 是续传前缀长度：构造时把已有前缀喂给 hasher 并把写位置移到末尾；
// reset 用于服务端不支持 Range（回 200 而非 206）时回卷重来。
type hashWriter struct {
	f   *os.File
	off int64
	h   hash.Hash
}

func newHashWriter(f *os.File, off int64) (*hashWriter, error) {
	hw := &hashWriter{f: f, off: off, h: sha256.New()}
	if off > 0 {
		if _, err := io.Copy(hw.h, io.NewSectionReader(f, 0, off)); err != nil {
			return nil, fmt.Errorf("读本地半包失败: %w", err)
		}
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return hw, nil
}

func (w *hashWriter) Write(p []byte) (int, error) {
	if _, err := w.f.Write(p); err != nil {
		return 0, err
	}
	return w.h.Write(p)
}

func (w *hashWriter) reset() error {
	w.h.Reset()
	if err := w.f.Truncate(0); err != nil {
		return err
	}
	_, err := w.f.Seek(0, io.SeekStart)
	w.off = 0
	return err
}

func (w *hashWriter) sum() string { return hex.EncodeToString(w.h.Sum(nil)) }

func shortURL(u string) string {
	if i := strings.LastIndex(u, "/releases/"); i > 0 {
		return "…" + u[i:]
	}
	if len(u) > 60 {
		return u[len(u)-57:]
	}
	return u
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}
