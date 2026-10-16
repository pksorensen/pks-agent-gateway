package main

import (
	"fmt"
	"io"
	"net/http"
	"sync"
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
)

// PrincipalLimits are optional caps; zero means unlimited.
type PrincipalLimits struct {
	RequestsPerMinute  int   `json:"requestsPerMinute,omitempty"`
	AudioSecondsPerDay int64 `json:"audioSecondsPerDay,omitempty"`
}

func (l *PrincipalLimits) validate() error {
	if l == nil {
		return nil
	}
	if l.RequestsPerMinute < 0 || l.AudioSecondsPerDay < 0 {
		return fmt.Errorf("limits must be >= 0")
	}
	return nil
}

type usageLimiter struct {
	mu      sync.Mutex
	windows map[string]*rateWindow // owner/principal
	audio   map[string]*audioDay   // owner/principal
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
	return &usageLimiter{windows: map[string]*rateWindow{}, audio: map[string]*audioDay{}}
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

	minute := now.Unix() / 60
	w := u.windows[key]
	if w == nil || w.minute != minute {
		w = &rateWindow{minute: minute}
		u.windows[key] = w
	}
	if lim.RequestsPerMinute > 0 && w.n >= lim.RequestsPerMinute {
		return &denial{http.StatusTooManyRequests, "rate limit: requests per minute exceeded"}, int(60 - now.Unix()%60)
	}

	var a *audioDay
	if lim.AudioSecondsPerDay > 0 && audioSecs > 0 {
		day := now.UTC().Format("2006-01-02")
		a = u.audio[key]
		if a == nil || a.day != day {
			a = &audioDay{day: day, secs: usedAudioSeconds(st, p.ID, day)}
			u.audio[key] = a
		}
		if a.secs+audioSecs > lim.AudioSecondsPerDay {
			midnight := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
			return &denial{http.StatusTooManyRequests, "quota: audio seconds per day exceeded"}, int(midnight.Sub(now.UTC()).Seconds()) + 1
		}
	}
	w.n++
	if a != nil {
		a.secs += audioSecs
	}
	return nil, 0
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
