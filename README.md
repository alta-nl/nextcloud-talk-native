# Nextcloud Talk Native Load Tester

A lightweight, browserless load-test client for **Nextcloud Talk with the High Performance Backend (HPB)**. Written in Go, using [Pion WebRTC](https://github.com/pion/webrtc), it creates multiple independent guest participants and publishes Opus audio and, optionally, VP8 video without launching Chromium or other browsers.

> **Community project, not an official Nextcloud tool.** Use only with authorization, ideally against a dedicated non-production test environment.

Yeah, this is vibecoded. If it doesn't work for you, vibetroubleshoot it.

## What it tests

For each simulated participant, the tool:

1. Activates a Nextcloud Talk guest session and sets a distinct display name (`Tester 001`, `Tester 002`, ...).
2. Obtains the room and signaling settings through Nextcloud's OCS APIs.
3. Connects to the HPB over its public WebSocket (`wss://`) endpoint, authenticates, and joins the room.
4. Creates an independent Pion WebRTC PeerConnection and publishes Opus audio, plus VP8 video when media files are supplied.
5. Maintains the media session for the configured duration, then leaves the call and reports the result.

**Requires a Nextcloud Talk deployment with a functioning HPB/media gateway (Janus).** A room backed by the standard peer-to-peer Talk setup is not the intended target. The tool uses normal public HTTPS/WSS endpoints; it does *not* require exposing a Janus admin/API port.

## Scope and limitations

- **Publisher-oriented load test:** simulated clients publish audio/video but **do not subscribe** to other participants' streams. This does *not* emulate a fully connected video meeting where everyone receives everyone else's media.
- `OK` indicates the sender-side WebRTC transport connected and remained connected for the test duration. It is **not proof of end-to-end audio/video playback**. Verify reception using a real Talk client and/or server-side metrics.
- The current implementation uses **direct ICE only**, without applying the STUN/TURN servers returned by Nextcloud. Network traversal therefore depends on your topology. Do not assume it will work through every NAT/firewall.
- The media sender reads an Ogg **page** as one Opus packet. For real audio, encode using the `-page_duration 20000` setting shown below; arbitrary Ogg files may otherwise produce silence or invalid playback.
- It does not dynamically react to server requests to renegotiate or subscribe to streams. It is not a drop-in replacement for the full browser client.
- Each tester runs for `-duration` **after its own PeerConnection connects**. With a long `-ramp`, earlier testers can finish before later ones have started.
- Up to 500 testers are allowed by the CLI, but this is a configurable software limit, **not a tested capacity or performance guarantee**.

## Requirements

- Linux/macOS/Windows environment with a supported Go toolchain (Go modules enabled).
- Go source and module files included in this repository.
- HTTPS access to Nextcloud and WSS access to its HPB.
- A Talk room allowing guest participation and permission to join its call.
- [FFmpeg](https://ffmpeg.org/) if you want to create media samples.

## Build and install

Clone the repository and build the application:

```bash
git clone https://github.com/alta-nl/nextcloud-talk-native.git
cd nextcloud-talk-native
go build -o talk-native .
```

The program uses Go modules to obtain dependencies; no browser installation is required. The module currently declares Go 1.26.8; `GOTOOLCHAIN=auto` can download the required Go toolchain when an older Go installation is used. Network access to the Go module proxy is needed on first build.

## Examples

One guest, Opus silence, 60 seconds (defaults):

```bash
./talk-native -nc https://cloud.example.org -talkid ROOM_TOKEN
```

Fifty guest publishers, launched 500 ms apart, for two minutes each:

```bash
./talk-native \
  -nc https://cloud.example.org \
  -talkid ROOM_TOKEN \
  -testers 50 \
  -ramp 500ms \
  -duration 120s
```

Publish prerecorded audio and video with verbose diagnostics:

```bash
./talk-native \
  -nc https://cloud.example.org \
  -talkid ROOM_TOKEN \
  -media-dir ./media \
  -testers 5 \
  -duration 2m \
  -debug
```

The room token is the final component of the Talk URL, for example `https://cloud.example.org/call/ROOM_TOKEN`. Supply only `ROOM_TOKEN` to `-talkid`.

## Command-line options

| Option | Default | Description |
| --- | --- | --- |
| `-nc` | Required | Nextcloud HTTPS hostname or URL |
| `-talkid` | Required | Nextcloud Talk room token |
| `-testers` | `1` | Number of independent guest sessions (1–500) |
| `-ramp` | `2s` | Time between starting successive guest sessions |
| `-duration` | `60s` | Media session duration per tester after connecting |
| `-media-dir` | Empty | Directory containing `test-audio.ogg` and `test-video.ivf`; if empty, publish Opus silence only |
| `-debug` | `false` | Enable detailed guest, HTTP/HPB milestone, ICE/SDP and RTP statistics logging |

By default, output is kept concise: test start, per-client successes/failures, warnings and a final summary. Errors are always reported. `-debug` enables per-participant diagnostics; do not publish full debug logs without checking them for environment-specific information.

## Creating media files

The tool expects these **exact file names** in the directory passed to `-media-dir`:

```text
media/
  test-audio.ogg
  test-video.ivf
```

From a source file you have permission to use, create Opus audio (48 kHz, stereo, 20 ms frames, with Ogg pages aligned to 20 ms):

```bash
ffmpeg -y -i input.mp4 \
  -map 0:a:0 -vn \
  -c:a libopus -ar 48000 -ac 2 \
  -b:a 96k -frame_duration 20 \
  -page_duration 20000 \
  media/test-audio.ogg
```

Create VP8 video in IVF (25 fps, fitted/padded within 900 × 720):

```bash
ffmpeg -y -i input.mp4 \
  -an -c:v libvpx \
  -vf "scale=900:720:force_original_aspect_ratio=decrease,pad=900:720:(ow-iw)/2:(oh-ih)/2,setsar=1" \
  -r 25 -b:v 1500k \
  -deadline realtime -cpu-used 4 -g 50 \
  -f ivf media/test-video.ivf
```

Create `media/` first (`mkdir -p media`). Audio and video are looped independently and may drift out of sync across repeated loops. Do not commit copyrighted or private sample videos to a public repository.

## Responsible use

Load tests can exhaust CPU, memory, network bandwidth and media gateway capacity. Run them only against systems you own or are expressly authorized to test. Start with one participant, confirm audio/video in a browser, then increase the load gradually. Monitor HPB, Janus, host CPU/memory, bandwidth, and packet loss. Avoid production conferences.

## License

Released under [The Unlicense](LICENSE): a public-domain dedication with a liability disclaimer. You may copy, modify, redistribute, and use this software for any purpose, including commercial use, without attribution requirements.
