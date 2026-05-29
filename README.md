# frigate-ffmpeg-proxy

Offload Frigate NVR's ffmpeg work to a native binary on the macOS host, gaining access to **VideoToolbox** hardware-accelerated decoding without patching Frigate or its Docker image.

### Note 
> This project has largely been vibe-coded; I've only done high-level reviews of the code in this repository and I stipulated the architecture, but apart from that it's all YOLO AI... so here be dragons.
> That said, this works _really_ well. I acquired an old Macbook M1 Pro 14" machine. It has limited RAM and limited storage... but as it turns out it's really good at running frigate with FFMpeg and ONNX/ANE-based object detection. With four 2.4k cameras it uses about 4w for both motion detction, object detection and recording. It's quite incredible. This repository enables that use case - without this code FFMpeg runs inside the frigate container, which means the CPU has to do all the video transcoding work. 
> Of course running FFMpeg in a proper container setup on Linux would be first-class, but my attempts at reverse engineering the video accelerator co-processor on M1 were frustrating hah

### WIP

- Name change from wrapper-coordinator and 

## How it works

Frigate runs inside a Docker container and invokes ffmpeg for every camera stream. By pointing Frigate's `ffmpeg.path` at this proxy, every invocation is transparently forwarded over TCP to a lightweight daemon (`coordinator`) running natively on the host, which then execs the real ffmpeg binary.

```
┌─────────────────────────────────────────┐
│  Docker container (linux/arm64)         │
│                                         │
│  Frigate ──► wrapper (drop-in ffmpeg)   │
│                  │                      │
│                  │ TCP (host.docker.internal:17346)
└──────────────────┼──────────────────────┘
                   │
┌──────────────────▼──────────────────────┐
│  macOS host                             │
│                                         │
│  coordinator ──► /opt/homebrew/bin/ffmpeg│
│                  (VideoToolbox enabled)  │
└─────────────────────────────────────────┘
```

The proxy is **transparent**: stdin, stdout, stderr, OS signals, and the exit code are all relayed faithfully. From Frigate's perspective, it is calling ffmpeg directly.

### Why TCP instead of a Unix socket?

Docker Desktop on macOS routes the container filesystem through a virtiofs/FUSE bridge that cannot relay Unix domain socket connections. TCP through `host.docker.internal` works without any special setup.

### Wire protocol

Each connection carries one ffmpeg invocation. Communication uses a simple 5-byte length-prefixed framing:

```
┌──────────────┬──────────────────┬───────────────┐
│  Type (1B)   │  Length (4B BE)  │  Payload      │
└──────────────┴──────────────────┴───────────────┘
```

| Direction          | Type          | Payload                        |
|--------------------|---------------|--------------------------------|
| wrapper → coord    | `ExecReq`     | JSON: args, env, cwd           |
| wrapper → coord    | `Stdin`       | raw stdin bytes                |
| wrapper → coord    | `StdinEOF`    | empty — signals stdin closed   |
| wrapper → coord    | `Signal`      | 1-byte OS signal number        |
| coord → wrapper    | `Stdout`      | raw stdout bytes               |
| coord → wrapper    | `Stderr`      | raw stderr bytes               |
| coord → wrapper    | `Exit`        | 4-byte int32 exit code (BE)    |

### Path mapping

When ffmpeg writes recordings or cache files, it uses the container-side path (e.g. `/media/frigate/recordings`). The coordinator rewrites these to host-side paths before spawning ffmpeg, so files land in the correct location on the macOS filesystem.

---

## Prerequisites

### Host (macOS)

- **Go 1.22+** — to build from source (`brew install go`)
- **ffmpeg with VideoToolbox** — `brew install ffmpeg`
- **Docker Desktop** — with `host.docker.internal` available (enabled by default)

### Container

No changes to the Frigate image are required. The wrapper binary is bind-mounted read-only into the container.

---

## Building

```bash
# Build everything (coordinator for macOS arm64, wrapper for Linux arm64 + amd64)
make all

# Or build individual targets
make coordinator       # dist/coordinator-darwin-arm64
make wrapper-arm64     # dist/wrapper-linux-arm64  (Apple Silicon Docker)
make wrapper-amd64     # dist/wrapper-linux-amd64  (Intel Docker)
```

Binaries land in `dist/`. They are statically linked (wrapper) and stripped of debug symbols for minimal size.

If you don't have Go installed locally, you can build the wrapper binaries using Docker:

```bash
make docker-wrapper    # writes to dist/docker/
```

---

## Configuration

### Coordinator (`config/coordinator.yaml`)

```yaml
# Absolute path to the native macOS ffmpeg binary.
# Install via Homebrew: brew install ffmpeg
ffmpeg_path: /opt/homebrew/bin/ffmpeg

# TCP address the coordinator listens on.
# Bind to loopback — the container reaches the host via host.docker.internal.
listen_addr: 127.0.0.1:17346

# Log verbosity: debug | info | warn | error
log_level: info

# Translate container-side path prefixes to host-side paths.
# The first matching prefix wins. List more-specific prefixes first.
# rtsp://, pipe:, and bare flags (-) are never remapped.
path_mappings:
  - container: /config
    host: /Users/you/frigate/config
  - container: /media/frigate/recordings
    host: /Users/you/frigate/recordings
  - container: /media/frigate/clips
    host: /Users/you/frigate/clips
  - container: /media/frigate/exports
    host: /Users/you/frigate/exports
  - container: /tmp/cache
    host: /tmp/frigate-cache
```

> **Tip:** Match the `path_mappings` entries to the `volumes:` section of your `docker-compose.yml`.

### Wrapper (environment variables)

| Variable           | Default                         | Description                        |
|--------------------|---------------------------------|------------------------------------|
| `FRIGATE_IPC_ADDR` | `host.docker.internal:17346`    | Address of the coordinator daemon  |

---

## Running

### Directly (development / testing)

```bash
# Start the coordinator
./dist/coordinator-darwin-arm64 --config config/coordinator.yaml
```

Press **Ctrl+C** to exit immediately, or send **SIGTERM** for a graceful drain (waits up to 30 s for in-flight ffmpeg processes to finish).

### As a launchd user agent (recommended for production)

Install the coordinator so it starts automatically when you log in and restarts if it crashes:

```bash
# 1. Edit the plist — replace YOUR_USERNAME with your macOS username
sed -i '' "s/YOUR_USERNAME/$(whoami)/g" launchd/com.frigate.coordinator.plist

# 2. Install the binary and the config
sudo mkdir -p /usr/local/etc/frigate-coordinator
sudo cp config/coordinator.yaml /usr/local/etc/frigate-coordinator/config.yaml
# Edit /usr/local/etc/frigate-coordinator/config.yaml to match your paths

# 3. Install and load
make install-launchd

# Check it is running
launchctl list com.frigate.coordinator

# View logs
tail -f /usr/local/var/log/frigate-coordinator.log

# Stop / uninstall
make uninstall-launchd
```

---

## Frigate configuration

### `docker-compose.yml`

Bind-mount the wrapper binary into the container at the path Frigate will call, and set the coordinator address:

```yaml
services:
  frigate:
    image: ghcr.io/blakeblackshear/frigate:stable-standard-arm64
    environment:
      - FRIGATE_IPC_ADDR=host.docker.internal:17346
    extra_hosts:
      - host.docker.internal:host-gateway
    volumes:
      # Bind-mount the wrapper as the ffmpeg binary Frigate will invoke.
      # Frigate appends /bin/ffmpeg to the configured path, so the full
      # destination path must include that suffix.
      - /path/to/wrapper-coordinator/dist/wrapper-linux-arm64:/usr/local/bin/frigate-wrapper/bin/ffmpeg:ro
      - /Users/you/frigate/config:/config
      - /Users/you/frigate/recordings:/media/frigate/recordings
      - /Users/you/frigate/clips:/media/frigate/clips
      - /Users/you/frigate/exports:/media/frigate/exports
```

### `config/config.yaml` (Frigate)

```yaml
ffmpeg:
  # Frigate appends /bin/ffmpeg to this value to get the actual binary path.
  path: /usr/local/bin/frigate-wrapper

  # Enable VideoToolbox hardware-accelerated decoding.
  # nv12 is VideoToolbox's native output format; ffmpeg converts to yuv420p
  # as needed for downstream filters and the detect pipe.
  hwaccel_args:
    - -hwaccel
    - videotoolbox
    - -hwaccel_output_format
    - nv12
```

> **Note on `ffmpeg.path`:** Frigate treats this as a directory prefix and always appends `/bin/ffmpeg`. The wrapper binary must be mounted at `<path>/bin/ffmpeg` inside the container.

---

## Verifying hardware acceleration

After restarting Frigate, check that the coordinator is executing ffmpeg with the VideoToolbox flags:

```
# coordinator terminal output should show:
coordinator: exec: /opt/homebrew/bin/ffmpeg [-hwaccel videotoolbox -hwaccel_output_format nv12 ...]
coordinator: ffmpeg exited with code 0
```

On the host, confirm VideoToolbox is active:

```bash
# CPU usage should drop significantly vs. software decoding
ps aux | grep ffmpeg

# macOS Activity Monitor → Window → GPU History shows VT decoder activity
```

---

## Project structure

```
wrapper-coordinator/
├── cmd/
│   ├── coordinator/    # macOS host daemon
│   └── wrapper/        # Linux drop-in ffmpeg replacement
├── internal/
│   ├── proto/          # Wire protocol (frame encoding/decoding)
│   └── pathmap/        # Container→host path prefix rewriting
├── config/
│   └── coordinator.yaml
├── docker/
│   └── Dockerfile.wrapper   # Multi-arch wrapper build (no local Go needed)
├── launchd/
│   └── com.frigate.coordinator.plist
└── Makefile
```

---

## Security notes

- The coordinator binds to **loopback only** (`127.0.0.1`). It is not reachable from the network or from other machines.
- The coordinator executes whatever arguments the wrapper sends. Keep the bind-mount `:ro` and ensure only trusted containers can reach port `17346`.
- No credentials or sensitive data are forwarded in the wire protocol; only ffmpeg arguments, a small set of environment variables (`HOME`, `USER`, `TERM`, `LANG`, `TZ`), and I/O streams.

---

## License

MIT
