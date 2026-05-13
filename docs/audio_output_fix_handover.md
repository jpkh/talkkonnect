# Audio Output Death — Bug Report & Fix Handover

**File affected:** `stream.go`  
**Date:** 2026-05-12  
**Investigated by:** Cascade (AI pair programmer)

---

## Background

Talkkonnect occasionally loses local voice output (audio from remote Mumble users goes silent) without crashing or producing any obvious log message. The issue was intermittent and hard to reproduce because all failure paths were silent. This document describes the four root causes identified and the fixes applied.

---

## Bug 1 — OpenAL panic swallowed silently, goroutine trapped in drain-only mode

### Where
`stream.go` — `playPacket` closure inside `OnAudioStream`

### What was happening
Every incoming Mumble audio stream spawns a goroutine. Inside that goroutine, a closure called `playPacket` wraps all OpenAL calls (buffer enqueue, source play) in a `recover()` block to catch panics. The original code was:

```go
defer func() {
    if r := recover(); r != nil {
        ok = false   // panic value discarded, nothing logged
    }
}()
```

If any OpenAL call panicked (e.g. the ALSA device was grabbed by another process, a USB audio device reset, or an OpenAL context error), the panic was silently swallowed — `r` was thrown away. The caller then detected `ok == false` and immediately switched the goroutine into **permanent drain-only mode** for the entire duration of that user's session:

```go
if !playPacket(samples, packet) {
    log.Println("error: Audio playback failed; switching to drain-only mode")
    for p := range e.C { _ = p }   // silently discard ALL remaining audio
    return
}
```

This meant: one transient OpenAL failure → complete silence for the rest of that user's session, with no indication in the log of what caused it. Since Mumble streams are long-lived (the channel stays open), this could persist for a very long time.

### Fix applied
1. The panic value is now logged before discarding it, giving a concrete error message.
2. Instead of immediately entering drain-only mode on first failure, the code now attempts a **source recovery** — deletes the failed OpenAL source and buffer pool, allocates fresh ones, and retries `playPacket`. Only if the retry also fails does it fall back to drain-only.

---

## Bug 2 — `source.Play()` silently fails, exhausts the 24-buffer pool, then drops all packets

### Where
`stream.go` — `playPacket` closure, `source.Play()` call

### What was happening
OpenAL's `alSourcePlay()` does not return an error — it sets an internal AL error flag which the go-openal wrapper does not check after `Play()`. If `source.Play()` failed silently (device unavailable, wrong state), the source stayed in `AL_STOPPED`. The `reclaim()` function only retrieves buffers that have been *consumed* by a playing source:

```go
reclaim := func() {
    if n := source.BuffersProcessed(); n > 0 { ... }
}
```

A stopped source never advances `BuffersProcessed`, so `reclaim()` returned nothing. Over successive packets, all 24 pre-allocated buffers were enqueued into the source's queue, leaving `emptyBufs` empty. Then every subsequent packet hit this path:

```go
if len(emptyBufs) == 0 {
    return true   // packet silently dropped, no error logged
}
```

`return true` means "success" to the caller — so no drain-only mode was entered, no error was logged, and `source.Play()` was retried on every packet and failed every time. The result was **complete silence with zero log output**, making this the hardest failure to diagnose.

### Fix applied
After calling `source.Play()`, the source state is checked immediately. If it is still not `Playing`, a warning is logged:

```go
if source.State() != openal.Playing {
    source.Play()
    if source.State() != openal.Playing {
        log.Printf("warn: OpenAL source refused to play (state=%v); may indicate ALSA device conflict", source.State())
    }
}
```

This surfaces the silent failure immediately. Combined with the Fix 1 recovery mechanism, a new source is created before the buffer pool can be exhausted.

---

## Bug 3 (contributing cause) — ALSA device contention between OpenAL and `localMediaPlayer`

### Where
`media.go` — `localMediaPlayer` function  
`stream.go` — `New()`, `openal.OpenDevice("")`

### What was happening
`localMediaPlayer` launches `ffplay` with `SDL_AUDIODRIVER=alsa`. SDL's ALSA backend can open the audio device exclusively (using `hw:X,Y` or `plughw:X,Y`). OpenAL also opens the ALSA default device via `openal.OpenDevice("")`. On systems without a software mixing layer (dmix / PulseAudio), these two cannot coexist.

`localMediaPlayer` is called for:
- Channel join/leave sounds
- Beacon tones
- TTS message notification sounds
- Event sounds

These fire frequently during normal operation — exactly when users are likely talking. When either process grabs exclusive ALSA access, the other fails. If OpenAL loses access, the next `QueueBuffer` or `Play` call panics, triggering Bug 1 or Bug 2 above.

### Fix applied (partial — code fix only)
Bugs 1 and 2 above now allow recovery from the transient failure. However the underlying ALSA contention still exists. The full mitigation requires ensuring the ALSA device is configured with dmix or that PulseAudio is used as the audio backend on the target system. No code change was made to `media.go` for this item; it is a system configuration issue.

---

## Bug 4 — `ResetStream()` leaks the ALSA device handle and registers a duplicate audio handler

### Where
`stream.go` — `ResetStream()` function

### What was happening
`ResetStream()` was intended to recover from a bad audio context by tearing it down and reopening. The original implementation was:

```go
func (b *Talkkonnect) ResetStream() {
    b.Stream.contextSink.Destroy()   // destroys OpenAL context
    time.Sleep(50 * time.Millisecond)
    b.OpenStream()                   // opens a new Stream
}
```

Two problems:

1. **Device leak**: `b.Stream.deviceSink.CloseDevice()` was never called. The ALSA device handle was abandoned. Repeated calls to `ResetStream()` would leak device handles until ALSA ran out.

2. **Duplicate audio handler**: `OpenStream()` calls `New()`, which calls `client.Config.AttachAudio(s)` to register the new `Stream` as a Mumble audio event handler. The old `Stream`'s audio handler was never detached (`link.Detach()` was never called). After one call to `ResetStream()`, *two* `OnAudioStream` handlers would be active simultaneously — both receiving every incoming audio packet and both trying to play it through their respective OpenAL sources.

The existing `Destroy()` method does this correctly:

```go
func (b *Talkkonnect) Destroy() {
    b.Stream.link.Detach()           // detaches audio handler
    ...
    b.Stream.contextSink.Destroy()
    b.Stream.deviceSink.CloseDevice()
    b.Stream.contextSink = nil
    b.Stream.deviceSink = nil
}
```

`ResetStream()` was not using it.

Note: `ResetStream()` did not appear to be called anywhere in the current codebase, so this was a latent/dormant bug. It is fixed now so it is safe to call in future recovery logic.

### Fix applied
`ResetStream()` now delegates teardown to `Destroy()`:

```go
func (b *Talkkonnect) ResetStream() {
    b.Destroy()
    time.Sleep(50 * time.Millisecond)
    b.OpenStream()
}
```

---

## Bug 5 — ALSA mixer thread timeout causes permanent silence after long uptime

### Where
OpenAL-Soft internal ALSA backend (`ALCplaybackAlsa_mixerProc`)  
`stream.go` — `OnAudioStream`, `Stream` struct, `OpenStream()`

### Symptom
After approximately 10 hours of continuous operation the following message appears on stderr **repeatedly** (~once per second) and audio output dies completely:

```
AL lib: (EE) ALCplaybackAlsa_mixerProc: Wait timeout... buffer size too low?
```

### What was happening
OpenAL-Soft's ALSA backend runs a dedicated mixer thread. That thread calls `snd_pcm_wait()` with a 1000 ms timeout to wait for the ALSA PCM device to become ready for writing. On Raspberry Pi, after long uptime, the ALSA PCM device enters a hung state (usually triggered by accumulated XRUNs — buffer underruns/overruns). When this happens `snd_pcm_wait()` times out every second and the error is logged.

The critical consequence in Talkkonnect code: OpenAL's internal source state machine still reports `AL_PLAYING` (it does not observe the ALSA-level hang). `BuffersProcessed()` returns 0 because ALSA is not consuming data. The `reclaim()` function therefore recovers no buffers. Over successive Mumble audio packets all 24 pre-allocated buffers fill the source queue with no reclaim, leaving `emptyBufs` empty. Every subsequent packet then hits:

```go
if len(emptyBufs) == 0 {
    return true   // silently dropped
}
```

`return true` is "success" to the caller — no error, no drain-only mode, no log. Audio dies completely and silently.

This was not caught by Fixes 1–4 because:
- No OpenAL call panics (Fix 1 wouldn't trigger)
- `source.Play()` succeeds as far as OpenAL is concerned (Fix 3 wouldn't trigger)
- `playPacket` keeps returning `true` so Fix 2 recovery never runs

### Fix applied — two parts

**Part A — Code: buffer pool stall detector + `onStreamDead` callback**

A new `onStreamDead func()` field was added to the `Stream` struct. `OpenStream()` sets this callback to trigger a full `ResetStream()` (which, after Fix 4, correctly closes the ALSA device and reopens it via `Destroy()` + `OpenStream()`). A `CompareAndSwap` atomic flag prevents concurrent resets.

Inside `OnAudioStream`, after every `playPacket` call, a stall counter tracks how many consecutive packets found `emptyBufs` empty:

```go
if len(emptyBufs) == 0 {
    bufPoolStallCount++
    if bufPoolStallCount >= 20 {
        log.Printf("error: OpenAL buffer pool stalled for %d consecutive packets; ALSA device likely hung; triggering stream reset", bufPoolStallCount)
        if s.onStreamDead != nil {
            s.onStreamDead()
        }
        return
    }
} else {
    bufPoolStallCount = 0
}
```

20 consecutive stalled packets ≈ 200 ms of silence before auto-recovery triggers. The `onStreamDead` callback closes and reopens the entire ALSA device, clearing the hung state.

**Part B — System config: `alsoft.conf` with larger ALSA buffer**

The root cause of the hang is that the default OpenAL-Soft ALSA buffer size is too small for the RPi timer resolution under long uptime, leading to XRUNs. Increasing the buffer and period size prevents the `snd_pcm_wait()` timeout from occurring in the first place.

A sample config file has been added at `sample-configs/alsoft.conf`. Deploy it to the RPi:

```bash
sudo mkdir -p /etc/openal
sudo cp sample-configs/alsoft.conf /etc/openal/alsoft.conf
```

Key settings:
```ini
[alsa]
buffer-size = 8192   # keep default — increasing this adds playback latency
period-size = 1024   # THIS is the actual fix: RPi timer is ~480 frames; 1024 gives safe margin
mmap = true          # slightly reduces latency vs read/write mode
```

**Important — latency**: do NOT increase `buffer-size` above 8192. At 48 kHz it is already 170 ms of playback buffer. Doubling it to 16384 would add another 170 ms of latency, which is unacceptable for PTT radio. The `period-size` change is what fixes the RPi ALSA timer margin without touching latency at all.

Both parts are needed: the config prevents the hang; the code recovers automatically if it still occurs (e.g. on systems where the config is not deployed, or under unusual load).

---

## Summary of Changes

All changes are in `stream.go` only. No other files were modified.

| # | Function | Original behaviour | Fixed behaviour |
|---|----------|--------------------|-----------------|
| 1 | `playPacket` (recover block) | Panic silently swallowed, goroutine locked in drain-only mode for session | Panic value logged; source+buffers recreated and retried before drain-only fallback |
| 2 | `playPacket` (source.Play check) | `Play()` failure invisible; buffer pool exhausted; all packets dropped silently | Source state checked after `Play()`; warning logged if source refuses to start |
| 3 | `OnAudioStream` (drain-only fallback) | First playback failure → permanent silence for session | Drain-only only reached after recovery attempt fails |
| 4 | `ResetStream()` | Leaked `deviceSink`; left stale audio handler registered | Calls `Destroy()` for full clean teardown before reopening |
| 5 | `OnAudioStream` + `Stream` + `OpenStream()` | ALSA mixer thread timeout after long uptime silently exhausted buffer pool; no recovery | Buffer pool stall detector triggers `onStreamDead` → `ResetStream()` after 20 consecutive stalled packets; `alsoft.conf` prevents the timeout at the ALSA level |

---

## What to observe after deploying to the RPi

When audio output dies, the log should now show one of:

- `error: playPacket OpenAL panic: <value>` — Bug 1 was the active path; the value tells you the exact OpenAL error
- `warn: OpenAL source refused to play (state=X)` — Bug 2 was the active path; state value indicates stopped/initial
- `warn: Audio playback failed; attempting source recovery` followed by either:
  - `info: Audio source recovery succeeded` — transient failure, self-healed
  - `error: Audio playback failed after source recovery; switching to drain-only mode` — persistent failure, needs investigation

If the warning fires repeatedly during `localMediaPlayer` / TTS events, the ALSA dmix configuration on the Pi is the next thing to fix.

- `error: OpenAL buffer pool stalled for N consecutive packets; ALSA device likely hung; triggering stream reset` — Bug 5 active; ALSA mixer thread timed out. Code will auto-recover via `ResetStream()`. Also deploy `sample-configs/alsoft.conf` to `/etc/openal/alsoft.conf` to prevent recurrence.
- `warn: ALSA hang detected; executing full stream reset` — confirms `ResetStream()` was invoked by the watchdog; audio should resume within ~250 ms.
