package talkkonnect

import (
	"fmt"
	"sync/atomic"
	"time"
)

var (
	lastRemoteCommandUnixNs int64
	lastMumbleEventUnixNs   int64
	lastMumblePingUnixNs    int64

	lastRemoteCommandInfo atomic.Value
	lastMumbleEventInfo   atomic.Value
	lastMumblePingInfo    atomic.Value
)

func init() {
	lastRemoteCommandInfo.Store("never")
	lastMumbleEventInfo.Store("never")
	lastMumblePingInfo.Store("never")
}

func markRemoteCommand(source string, command string) {
	now := time.Now()
	atomic.StoreInt64(&lastRemoteCommandUnixNs, now.UnixNano())
	lastRemoteCommandInfo.Store(fmt.Sprintf("%s cmd=%s", source, command))
}

func markMumbleEvent(event string) {
	now := time.Now()
	atomic.StoreInt64(&lastMumbleEventUnixNs, now.UnixNano())
	lastMumbleEventInfo.Store(event)
}

func markMumblePingResult(addr string, ok bool, detail string) {
	now := time.Now()
	atomic.StoreInt64(&lastMumblePingUnixNs, now.UnixNano())
	state := "fail"
	if ok {
		state = "ok"
	}
	if detail != "" {
		lastMumblePingInfo.Store(fmt.Sprintf("%s %s (%s)", state, addr, detail))
		return
	}
	lastMumblePingInfo.Store(fmt.Sprintf("%s %s", state, addr))
}

func formatLastSeen(ts int64, info string) string {
	if ts <= 0 {
		return fmt.Sprintf("never (%s)", info)
	}
	t := time.Unix(0, ts)
	return fmt.Sprintf("%s ago @ %s (%s)", time.Since(t).Round(time.Second), t.Format("2006-01-02 15:04:05"), info)
}

func healthStatusLine() string {
	remoteInfo, _ := lastRemoteCommandInfo.Load().(string)
	mumbleInfo, _ := lastMumbleEventInfo.Load().(string)
	pingInfo, _ := lastMumblePingInfo.Load().(string)

	return fmt.Sprintf("remote=%s | mumble=%s | ping=%s",
		formatLastSeen(atomic.LoadInt64(&lastRemoteCommandUnixNs), remoteInfo),
		formatLastSeen(atomic.LoadInt64(&lastMumbleEventUnixNs), mumbleInfo),
		formatLastSeen(atomic.LoadInt64(&lastMumblePingUnixNs), pingInfo),
	)
}
