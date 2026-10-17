package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Per-principal limits (ADR 0006). Requests per minute protect the gateway;
// audio seconds per day is what maps to upstream speech spend. Audio is
// estimated from Content-Length before the body is read, so the decision is
// made without buffering:
//
//	seconds = ceil(Content-Length / audioBytesPerSecond)   (default 32000 =
//	16 kHz PCM16 mono WAV)
//
// The multipart envelope and the definition JSON inflate the estimate a
// little, which errs on the conservative side. Compressed audio is
// under-counted; set audioBytesPerSecond on the operation for such inputs.

const (
	defaultMaxBodyBytes        int64 = 6 << 20
	defaultAudioBytesPerSecond int64 = 32000
	meterAudio                       = "audio"
	defaultMaxStreams                = 2
	defaultMaxSessionSeconds   int64 = 300
)

// PrincipalLimits are optional caps; zero means unlimited, except
// MaxStreams, where zero means the default of 2 concurrent WebSocket
// sessions and -1 means no cap.
type PrincipalLimits struct {
	RequestsPerMinute  int   `json:"requestsPerMinute,omitempty"`
	AudioSecondsPerDay int64 `json:"audioSecondsPerDay,omitempty"`
	MaxStreams         int   `json:"maxStreams,omitempty"`
}

func (l *PrincipalLimits) validate() error {
	if l == nil {
		return nil
	}
	if l.RequestsPerMinute < 0 || l.AudioSecondsPerDay < 0 {
		return fmt.Errorf("limits must be >= 0")
	}
	if l.MaxStreams < -1 {
		return fmt.Errorf("maxStreams must be -1 (no cap), 0 (default %d) or positive", defaultMaxStreams)
	}
	return nil
}

func (l *PrincipalLimits) maxStreams() int {
	if l == nil || l.MaxStreams == 0 {
		return defaultMaxStreams
	}
	return l.MaxStreams
}

type usageLimiter struct {
	mu      sync.Mutex
	windows map[string]*rateWindow // owner/principal
	audio   map[string]*audioDay   // owner/principal
	streams map[string]int         // owner/principal → open WebSocket sessions
}

type rateWindow struct {
	minute int64
	n      int
}

type audioDay struct {
	day  string
	secs int64
}

func newUsageLimiter() *usageLimiter {
	return &usageLimiter{windows: map[string]*rateWindow{}, audio: map[string]*audioDay{}, streams: map[string]int{}}
}

// admit charges one request and audioSecs against p's limits, or returns a
// 429 denial with the seconds until the limit resets. Nothing is charged on
// a denial. The daily audio total is seeded from today's usage records, so a
// restart does not reset it.
func (u *usageLimiter) admit(st *GatewayStore, p *Principal, now time.Time, audioSecs int64) (*denial, int) {
	lim := p.Limits
	if lim == nil || (lim.RequestsPerMinute == 0 && lim.AudioSecondsPerDay == 0) {
		return nil, 0
	}
	key := st.owner + "/" + p.ID
	u.mu.Lock()
	defer u.mu.Unlock()

	w, d, retry := u.window(key, lim, now)
	if d != nil {
		return d, retry
	}
	var a *audioDay
	if lim.AudioSecondsPerDay > 0 && audioSecs > 0 {
		a = u.today(st, p, key, now)
		if a.secs+audioSecs > lim.AudioSecondsPerDay {
			return audioExhausted(now)
		}
	}
	w.n++
	if a != nil {
		a.secs += audioSecs
	}
	return nil, 0
}

// window returns key's current minute, or a denial when it is full.
func (u *usageLimiter) window(key string, lim *PrincipalLimits, now time.Time) (*rateWindow, *denial, int) {
	minute := now.Unix() / 60
	w := u.windows[key]
	if w == nil || w.minute != minute {
		w = &rateWindow{minute: minute}
		u.windows[key] = w
	}
	if lim != nil && lim.RequestsPerMinute > 0 && w.n >= lim.RequestsPerMinute {
		return nil, &denial{http.StatusTooManyRequests, "rate limit: requests per minute exceeded"}, int(60 - now.Unix()%60)
	}
	return w, nil, 0
}

// today returns key's audio total for now's UTC day, seeded from the usage log.
func (u *usageLimiter) today(st *GatewayStore, p *Principal, key string, now time.Time) *audioDay {
	day := now.UTC().Format("2006-01-02")
	a := u.audio[key]
	if a == nil || a.day != day {
		a = &audioDay{day: day, secs: usedAudioSeconds(st, p.ID, day)}
		u.audio[key] = a
	}
	return a
}

func audioExhausted(now time.Time) (*denial, int) {
	midnight := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	return &denial{http.StatusTooManyRequests, "quota: audio seconds per day exceeded"}, int(midnight.Sub(now.UTC()).Seconds()) + 1
}

// stream is one admitted WebSocket session. audioLeft is today's remaining
// audio quota in seconds when the operation is metered and the principal has
// one, else -1.
type stream struct {
	audioLeft int64
	release   func(audioSecs int64)
}

// admitStream admits one WebSocket session before any token is minted or
// upstream dialed: one request against rpm, a free slot under maxStreams,
// and (metered) some audio quota left today. release ends the session and
// charges its audio seconds to the in-memory day; the caller appends the
// usage record first, so a reseed between the two can only over-count.
func (u *usageLimiter) admitStream(st *GatewayStore, p *Principal, now time.Time, metered bool) (*stream, *denial, int) {
	lim := p.Limits
	key := st.owner + "/" + p.ID
	u.mu.Lock()
	defer u.mu.Unlock()

	w, d, retry := u.window(key, lim, now)
	if d != nil {
		return nil, d, retry
	}
	if n := lim.maxStreams(); n > 0 && u.streams[key] >= n {
		return nil, &denial{http.StatusTooManyRequests, fmt.Sprintf("concurrent stream limit (%d) reached", n)}, 5
	}
	left := int64(-1)
	if metered && lim != nil && lim.AudioSecondsPerDay > 0 {
		if left = lim.AudioSecondsPerDay - u.today(st, p, key, now).secs; left <= 0 {
			d, retry := audioExhausted(now)
			return nil, d, retry
		}
	}
	w.n++
	u.streams[key]++
	var once sync.Once
	return &stream{audioLeft: left, release: func(audioSecs int64) {
		once.Do(func() {
			u.mu.Lock()
			defer u.mu.Unlock()
			u.streams[key]--
			if a := u.audio[key]; a != nil && audioSecs > 0 && a.day == now.UTC().Format("2006-01-02") {
				a.secs += audioSecs
			}
		})
	}}, nil, 0
}

func usedAudioSeconds(st *GatewayStore, principal, day string) int64 {
	recs, err := st.ReadUsage(day, day)
	if err != nil {
		return 0
	}
	var n int64
	for _, r := range recs {
		if r.Principal == principal {
			n += r.AudioSeconds
		}
	}
	return n
}

// estimateAudioSeconds rounds up: a 1-byte overflow is a whole second.
func estimateAudioSeconds(contentLength, bytesPerSecond int64) int64 {
	if bytesPerSecond <= 0 {
		bytesPerSecond = defaultAudioBytesPerSecond
	}
	return (contentLength + bytesPerSecond - 1) / bytesPerSecond
}

// countingConn counts the bytes of a hijacked (upgraded) client connection:
// Read is caller → upstream, Write is upstream → caller.
type countingConn struct {
	net.Conn
	in, out atomic.Int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.in.Add(int64(n))
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.out.Add(int64(n))
	return n, err
}

// countingBody counts request bytes as the proxy streams them upstream.
type countingBody struct {
	io.ReadCloser
	n int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}
