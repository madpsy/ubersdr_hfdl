# ubersdr_iq + hfdl_launcher

Tools for receiving HFDL using an [UberSDR](https://ubersdr.org) instance as the
radio back-end and [dumphfdl](https://github.com/szpajder/dumphfdl) as the decoder.

> **Note:** By default, this tool will send SBS messages to the [OARC](https://www.oarc.uk/) (Online Amateur Radio Club) plane tracker at [adsb.oarc.uk](https://adsb.oarc.uk/).

---

## Quick Start — Prebuilt Docker Image

> **Most users should start here.**  A prebuilt image is published on Docker Hub
> at [`madpsy/ubersdr_hfdl`](https://hub.docker.com/r/madpsy/ubersdr_hfdl) — no
> build step required.
>
> **Run this command on the same machine as your UberSDR installation:**

```bash
curl -fsSL https://raw.githubusercontent.com/madpsy/ubersdr_hfdl/refs/heads/main/install.sh | bash
```

This will download `docker-compose.yml` into `~/ubersdr/hfdl/`, pull the image,
and start the container automatically.

Once running, access the live statistics dashboard via the UberSDR proxy.

> **Note:** The container connects to UberSDR at `http://ubersdr:8080` by
> default.  To use a different address, edit `~/ubersdr/hfdl/docker-compose.yml`
> and set the `UBERSDR_URL` environment variable, then run `docker compose up -d`
> from that directory.

---

## Optimising Frequency Coverage

By default the launcher fetches the full HFDL frequency list from
[ubersdr.org/hfdl](https://ubersdr.org/hfdl/) and monitors **every** enabled
frequency.  Over time you may find that many of those frequencies are never
active from your location — monitoring them wastes IQ bandwidth and CPU.

The **⚙ Instances** tab in the dashboard lets you apply a new frequency
configuration directly from the browser, or export a file to apply manually.

### Apply from the UI (recommended)

The easiest way to update the frequency list is directly from the dashboard —
no SSH or file copying required.  To enable this you need to set a password in
your `docker-compose.yml`.

**1. Set `CONFIG_PASS` in `~/ubersdr/hfdl/docker-compose.yml`:**

```yaml
environment:
  CONFIG_PASS: "choose-a-strong-password-here"
```

> **Security note:** `CONFIG_PASS` protects the Apply endpoints from
> unauthorised use.  Choose a strong, unique password — anyone who knows it can
> overwrite your frequency file and trigger a service restart.

**2. Restart the container to pick up the new setting:**

```bash
cd ~/ubersdr/hfdl
./restart.sh
```

**3. Run for at least 24 hours** with the default (all frequencies enabled) so
the launcher has a chance to hear activity across different times of day and
propagation conditions.

**4. Open the dashboard → ⚙ Instances tab** and click one of the Apply buttons:

| Button | What it does |
|--------|-------------|
| **Apply Active Frequencies** | Overwrites the frequency file with only the frequencies that received at least one message during this session, then restarts the service |
| **Apply All Frequencies** | Overwrites the frequency file with every frequency marked as enabled (equivalent to the upstream default), then restarts the service |
| **Apply Latest Frequencies** | Fetches the current list from ubersdr.org, overwrites the frequency file, then restarts the service |

You will be prompted for the `CONFIG_PASS` password.  On success the service
restarts automatically and the page reloads after 5 seconds.

> **Tip:** If propagation changes and you start missing stations, use
> **Apply Latest Frequencies** to reset back to the full upstream list in one click.

---

### Export and apply manually (alternative)

If you prefer not to set a password, you can export the frequency file from the
dashboard and apply it manually.

The **⚙ Instances** tab provides three export buttons:

| Button | What it produces |
|--------|-----------------|
| **Export Active Frequencies** | A `hfdl_frequencies.jsonl` where only frequencies that received at least one message **during the current session** are marked as enabled |
| **Export All Frequencies** | A `hfdl_frequencies.jsonl` where **every** frequency is marked as enabled (equivalent to the upstream default) |
| **Export Latest Frequencies** | The current `hfdl_frequencies.jsonl` fetched directly from ubersdr.org, as-is |

1. **Run for at least 24 hours**, then click **Export Active Frequencies** to
   download the file.

2. Copy it to the host frequency file:

   ```bash
   cp ~/Downloads/hfdl_frequencies.jsonl ~/ubersdr/hfdl/hfdl_frequencies.jsonl
   ```

3. Restart the container:

   ```bash
   cd ~/ubersdr/hfdl
   ./restart.sh
   ```

   The launcher will now only open IQ windows for the frequencies you have
   actually heard, reducing CPU and bandwidth usage.

---

## Overview

```
UberSDR server
     │  WebSocket (IQ stream)
     ▼
ubersdr_iq          ← connects, decodes PCM-zstd packets, writes raw CS16 to stdout
     │  pipe (raw CS16 IQ)
     ▼
dumphfdl            ← decodes HFDL frames, outputs JSON / text / UDP / etc.
     │  stdout (JSON, always enabled)
     ▼
hfdl_launcher       ← aggregates stats, serves web dashboard on :6090
```

`hfdl_launcher` automates this: it fetches the live HFDL ground-station frequency
table, groups all frequencies into the minimum number of IQ windows using the
**smallest bandwidth that fits each cluster**, then launches and supervises one
pipeline per window.  The IQ mode (`iq48` / `iq96` / `iq192`) is chosen
automatically — no manual configuration required.

The launcher also runs a built-in **web statistics server** (default port 6090)
that shows per-frequency message counts, signal levels, and a live decoded
message feed.

---

## Building

A `build.sh` script is provided:

```bash
cd ubersdr_iq

# Build both binaries into the current directory
./build.sh

# Build and install to /usr/local/bin (requires sudo)
./build.sh install

# Install to a custom directory
INSTALL_DIR=~/.local/bin ./build.sh install

# Remove built binaries
./build.sh clean
```

Or build manually with `go`:

```bash
go build -o ubersdr_iq .
go build -o hfdl_launcher ./cmd/hfdl_launcher/
```

---

## Docker

A `Dockerfile` and `docker.sh` helper are provided.  Everything is built from
source inside the image — no host binaries required:

| Component | Source |
|-----------|--------|
| `ubersdr_iq`, `hfdl_launcher` | Go source in this repo |
| `libacars` | GitHub release tarball (`szpajder/libacars`) |
| `dumphfdl` | GitHub (`szpajder/dumphfdl`, `master` branch) |

### Prerequisites

- Docker installed and running

### Building the image

```bash
cd ubersdr_iq

# Build with default image name hfdl_launcher:latest
./docker.sh build

# Build with a custom name/tag
IMAGE=myregistry/hfdl_launcher:1.0 ./docker.sh build

# Build and push to a registry
IMAGE=myregistry/hfdl_launcher:1.0 ./docker.sh push

# Pin to a specific dumphfdl tag
DUMPHFDL_VERSION=v1.4.0 ./docker.sh build

# Pin to a specific libacars version
LIBACARS_VERSION=2.2.0 ./docker.sh build

# Cross-compile for a different architecture
PLATFORM=linux/arm64 ./docker.sh build
```

### Running the container

The container is configured via environment variables.  All `hfdl_launcher`
flags are available as env vars:

| Env var | Equivalent flag | Default |
|---------|----------------|---------|
| `UBERSDR_URL` | `-url` | `http://ubersdr:8080` |
| `PASS` | `-pass` | |
| `STATION` | `-station` | *(all)* |
| `SYSTEM_TABLE` | `-system-table` | |
| `FREQ_URL` | `-freq-url` | *(upstream default)* |
| `CONFIG_PASS` | `-config-pass` | *(Apply endpoints disabled if unset)* |
| `WEB_PORT` | `-web-port` | `6090` (set to `0` to disable) |
| `WEB_STATIC` | `-web-static` | `/usr/local/share/hfdl_launcher/static` |
| `DRY_RUN` | `-dry-run` | set to `1` to enable |
| `EXTRA_ARGS` | *(after `--`)* | extra dumphfdl args |
| `IQ_RECORD_DIR` | `-iq-record-dir` | *(disabled if unset)* |
| `IQ_RECORD_SECONDS` | `-iq-record-seconds` | `30` |

> **Note:** There is no `IQ_MODE` variable.  The bandwidth (`iq48`, `iq96`, or
> `iq192`) is chosen automatically per window based on how tightly the HFDL
> channels are clustered.
>
> **Note:** `--output decoded:json:file:path=-` is always injected automatically
> by the launcher for the web statistics server.  Do not add it to `EXTRA_ARGS`.

#### Basic usage (connect to UberSDR on the host)

```bash
docker run --rm \
  --name ubersdr_hfdl \
  -p 6090:6090 \
  -e UBERSDR_URL=http://ubersdr:8080 \
  hfdl_launcher:latest
```

Open `http://localhost:6090` in a browser to view the statistics dashboard.

#### Web statistics server on a custom port

```bash
docker run --rm \
  --name ubersdr_hfdl \
  -p 9000:9000 \
  -e UBERSDR_URL=http://ubersdr:8080 \
  -e WEB_PORT=9000 \
  hfdl_launcher:latest
```

#### Disable the web statistics server

```bash
docker run --rm \
  --name ubersdr_hfdl \
  -e UBERSDR_URL=http://ubersdr:8080 \
  -e WEB_PORT=0 \
  hfdl_launcher:latest
```

#### Send decoded output to a remote host via TCP (e.g. OARC aggregator)

The `--output` specifier format is `<what>:<format>:<type>:<params>`:

```bash
docker run --rm \
  --name ubersdr_hfdl \
  -e UBERSDR_URL=http://ubersdr:8080 \
  -e EXTRA_ARGS="--output decoded:basestation:tcp:address=adsb.oarc.uk,port=32010" \
  hfdl_launcher:latest
```

#### Send JSON to a UDP socket

```bash
docker run --rm \
  --name ubersdr_hfdl \
  -e UBERSDR_URL=http://ubersdr:8080 \
  -e EXTRA_ARGS="--output decoded:json:udp:address=192.168.1.20,port=5555" \
  hfdl_launcher:latest
```

#### Monitor specific ground stations only

```bash
docker run --rm \
  --name ubersdr_hfdl \
  -e UBERSDR_URL=http://ubersdr:8080 \
  -e STATION=1,2,3 \
  hfdl_launcher:latest
```

#### Use a system table mounted from the host

```bash
docker run --rm \
  --name ubersdr_hfdl \
  -e UBERSDR_URL=http://ubersdr:8080 \
  -e SYSTEM_TABLE=/data/systable.conf \
  -v /etc/dumphfdl:/data:ro \
  hfdl_launcher:latest
```

#### Record 30 seconds of raw IQ data to WAV files on the host

When `IQ_RECORD_DIR` is set, the launcher records the first N seconds of the
raw CS16 IQ stream from **each** frequency window as a standard PCM WAV file
(2-channel, 16-bit, at the window's sample rate).  Files are written to the
directory inside the container that you volume-mount from the host.

```bash
docker run --rm \
  --name ubersdr_hfdl \
  -e UBERSDR_URL=http://ubersdr:8080 \
  -e IQ_RECORD_DIR=/iq_recordings \
  -e IQ_RECORD_SECONDS=30 \
  -v /home/user/iq_recordings:/iq_recordings \
  hfdl_launcher:latest
```

Files are named `iq_<centerKHz>kHz_<iqMode>_<UTC-timestamp>.wav`, for example:

```
iq_10063kHz_iq48_20260328T141500Z.wav
iq_11184kHz_iq96_20260328T141500Z.wav
```

After recording completes the IQ stream continues to flow into `dumphfdl`
uninterrupted — recording does **not** stop decoding.

Using `docker-compose.yml`, uncomment the relevant lines:

```yaml
environment:
  IQ_RECORD_DIR: "/iq_recordings"
  IQ_RECORD_SECONDS: "30"
volumes:
  - "./iq_recordings:/iq_recordings"
```

#### Dry-run to preview what would be launched

```bash
docker run --rm \
  --name ubersdr_hfdl \
  -e UBERSDR_URL=http://ubersdr:8080 \
  -e DRY_RUN=1 \
  hfdl_launcher:latest
```

#### Using `docker.sh run` (passes env vars from the current shell)

```bash
export UBERSDR_URL=http://ubersdr:8080
export EXTRA_ARGS="--output decoded:basestation:tcp:address=adsb.oarc.uk,port=32010 --output decoded:text:file:path=-"
./docker.sh run
```

### Output specifier format

`dumphfdl` uses a four-field colon-separated specifier for `--output`:

```
<what>:<format>:<type>:<params>
```

| Field | Options |
|-------|---------|
| `<what>` | `decoded` — decoded frames; `raw` — undecoded raw bytes |
| `<format>` | `text`, `json`, `basestation` |
| `<type>` | `file`, `tcp`, `udp` |
| `<params>` | `path=` (file); `address=,port=` (tcp/udp) |

Multiple `--output` flags are supported — each instance receives all of them.

Common examples:

| Destination | Specifier |
|-------------|-----------|
| stdout (text) | `decoded:text:file:path=-` |
| stdout (JSON) | `decoded:json:file:path=-` |
| TCP host (Basestation) | `decoded:basestation:tcp:address=host,port=N` |
| TCP host (JSON) | `decoded:json:tcp:address=host,port=N` |
| UDP host (JSON) | `decoded:json:udp:address=host,port=N` |
| Log file (text, daily rotation) | `decoded:text:file:path=/var/log/hfdl/hfdl.log,rotate=daily` |

---

### Connecting to UberSDR on the host machine

If UberSDR is running on the same machine as Docker, use the host's gateway
address instead of `localhost`:

```bash
# Linux (host networking)
docker run --rm --network host \
  hfdl_launcher:latest

# Or use the Docker bridge gateway (typically 172.17.0.1)
docker run --rm \
  --name ubersdr_hfdl \
  -e UBERSDR_URL=http://172.17.0.1:8080 \
  hfdl_launcher:latest
```

---

## `ubersdr_iq`

Minimal UberSDR IQ stream client.  Connects to an UberSDR instance, requests an
IQ mode centred on the given frequency, and writes a continuous stream of raw
**CS16** (little-endian signed 16-bit interleaved I/Q) samples to stdout.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-url` | `http://ubersdr:8080` | UberSDR base URL |
| `-freq` | *(required)* | Centre frequency in Hz |
| `-iq-mode` | `iq` | IQ mode (see table below) |
| `-pass` | | Bypass password (if required by the server) |
| `-no-reconnect` | | Disable auto-reconnect on disconnect |

### IQ modes

| Mode | Sample rate | Bandwidth |
|------|------------|-----------|
| `iq` | 10 000 Hz | 10 kHz — single HFDL channel |
| `iq48` | 48 000 Hz | 48 kHz — covers ~5 channels |
| `iq96` | 96 000 Hz | 96 kHz — covers ~10 channels |
| `iq192` | 192 000 Hz | 192 kHz |

> **Note:** `iq48` and wider require bypass authentication on most public UberSDR
> instances.  The public `iq` mode (10 kHz) is always available.

### Examples

Single channel, piped directly into dumphfdl:

```bash
ubersdr_iq -url http://sdr.example.com:8080 -freq 10081000 | \
  dumphfdl --iq-file - --sample-format CS16 --sample-rate 10000 \
           --centerfreq 10081 10081
```

Multi-channel with `iq48` (48 kHz, covers several nearby channels):

```bash
ubersdr_iq -url http://sdr.example.com:8080 -freq 10063000 -iq-mode iq48 | \
  dumphfdl --iq-file - --sample-format CS16 --sample-rate 48000 \
           --centerfreq 10063 10027 10063 10066 10075 10081 10084
```

---

## `hfdl_launcher`

Automatic multi-instance launcher.  At startup it:

1. Fetches the live HFDL ground-station frequency table from
   [ubersdr.org/hfdl](https://ubersdr.org/hfdl/) (or a custom URL via `-freq-url`)
2. Groups all HFDL frequencies into the minimum number of IQ windows, choosing
   the **smallest bandwidth** (`iq48` → `iq96` → `iq192`) that fits each cluster
3. Launches one `ubersdr_iq | dumphfdl` pipeline per window
4. Supervises all pipelines — if one exits it is restarted after a 10 s delay

### Bandwidth selection

For each cluster of channels the launcher tries bandwidths in ascending order
and picks the smallest one where adding more bandwidth captures no additional
channels:

| Mode | Bandwidth | Used when… |
|------|-----------|-----------|
| `iq48` | 48 kHz | all channels in the cluster fit within 48 kHz |
| `iq96` | 96 kHz | channels span 48–96 kHz |
| `iq192` | 192 kHz | channels span 96–192 kHz |

Clusters wider than 192 kHz are split across multiple windows, each
independently sized.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-url` | `http://ubersdr:8080` | UberSDR base URL |
| `-pass` | | Bypass password |
| `-ubersdr-iq` | `ubersdr_iq` | Path to the `ubersdr_iq` binary |
| `-dumphfdl` | `dumphfdl` | Path to the `dumphfdl` binary |
| `-freq-url` | `https://ubersdr.org/hfdl/hfdl_frequencies.jsonl` | HFDL frequency list URL |
| `-station` | *(all)* | Comma-separated ground station IDs to monitor |
| `-system-table` | | Path to dumphfdl system table file |
| `-config-pass` | | Password to protect the Apply frequency endpoints (required to use Apply in the UI) |
| `-web-port` | `6090` | Port for the web statistics server (`0` = disabled) |
| `-web-static` | `/usr/local/share/hfdl_launcher/static` | Path to static web files directory |
| `-dry-run` | | Print planned instances and commands without launching |
| `-selcal-freqs` | *(disabled)* | HF voice channels to monitor for SELCAL: `kHz[:label[:audio]]`, comma-separated |
| `-selcal-audio` | `true` | Relay channel audio to dashboard listeners |
| `-selcal-max-listeners` | `10` | Maximum simultaneous listeners **across all channels** (`0` = unlimited) |
| `-selcal-record-dir` | *(disabled)* | Directory to write a WAV of each SELCAL detection |
| `-selcal-record-seconds` | `8` | Length of the audio window saved per detection |

Any arguments after `--` are passed verbatim to **every** dumphfdl instance.

> **Note:** `--output decoded:json:file:path=-` is always injected automatically.
> You do not need to add it yourself.

### Web statistics server

`hfdl_launcher` runs a built-in HTTP server (default port **6090**) that provides:

| Endpoint | Description |
|----------|-------------|
| `GET /` | HTML dashboard — per-frequency stats table and live message feed |
| `GET /stats` | JSON snapshot of all statistics (total messages, per-frequency counts, signal levels, last 200 decoded messages) |
| `GET /events` | Server-Sent Events stream — each decoded message is pushed as a JSON `data:` event in real time |
| `GET /selcal` | JSON snapshot of the SELCAL channels and decoded calls (only meaningful when `-selcal-freqs` is set) |
| `GET /selcal/audio?ch=<kHz>` | WebSocket — live mono 8-bit µ-law audio for one channel |

The port is set with `-web-port` (flag) or `WEB_PORT` (env var when using Docker).
Set to `0` to disable the server entirely.

The static files (`index.html`, `style.css`, `app.js`) are served from the
directory specified by `-web-static` / `WEB_STATIC`.  In the Docker image they
are pre-installed at `/usr/local/share/hfdl_launcher/static`.

### Examples

#### Decode all stations (automatic bandwidth)

```bash
hfdl_launcher \
  -url http://sdr.example.com:8080 \
  -ubersdr-iq ./ubersdr_iq \
  -dumphfdl /usr/local/bin/dumphfdl
```

#### Decode all stations, send output to a UDP socket

```bash
hfdl_launcher \
  -url http://sdr.example.com:8080 \
  -ubersdr-iq ./ubersdr_iq \
  -dumphfdl /usr/local/bin/dumphfdl \
  -- --output decoded:json:udp:address=127.0.0.1,port=5555
```

#### Decode a single ground station

```bash
hfdl_launcher \
  -url http://sdr.example.com:8080 \
  -station 7 \
  -ubersdr-iq ./ubersdr_iq \
  -dumphfdl /usr/local/bin/dumphfdl
```

#### Decode several stations with a system table

```bash
hfdl_launcher \
  -url http://sdr.example.com:8080 \
  -station 1,2,3 \
  -system-table /etc/dumphfdl/systable.conf \
  -ubersdr-iq ./ubersdr_iq \
  -dumphfdl /usr/local/bin/dumphfdl \
  -- --output decoded:text:file:path=/var/log/hfdl/hfdl.log,rotate=daily
```

#### Dry-run to preview what would be launched

```bash
hfdl_launcher \
  -url http://sdr.example.com:8080 \
  -ubersdr-iq ./ubersdr_iq \
  -dumphfdl /usr/local/bin/dumphfdl \
  -dry-run \
  -- --output decoded:json:udp:address=127.0.0.1,port=5555
```

Sample dry-run output:

```
2026/03/26 18:45:10 found 105 unique HFDL frequencies (2941 – 21997 kHz)
2026/03/26 18:45:10 grouped into 22 windows (auto-selected bandwidths)
2026/03/26 18:45:10   window  1: centre=2978 kHz  mode=iq48   channels=[2941 2944 2992 2998]
2026/03/26 18:45:10   window  2: centre=3476 kHz  mode=iq48   channels=[3455 3497]
2026/03/26 18:45:10   window  3: centre=4681 kHz  mode=iq96   channels=[4654 4681 4687 4693 4699]
...
2026/03/26 18:45:10 dry-run commands:
2026/03/26 18:45:10   ./ubersdr_iq -url http://... -freq 2978000 -iq-mode iq48 -no-reconnect | \
                        /usr/local/bin/dumphfdl --iq-file - --sample-format CS16 \
                        --sample-rate 48000 --centerfreq 2978 \
                        --output udp,address=127.0.0.1,port=5555 \
                        2941 2944 2992 2998
```

### Supervision

Each pipeline is supervised independently.  If `ubersdr_iq` disconnects or
crashes, the corresponding `dumphfdl` process is killed and the pipeline is
restarted after a **10 second** delay.  A clean shutdown (Ctrl+C / SIGTERM)
sets a stop flag that suppresses the auto-restart.

---

## SELCAL / SELCAL32

`hfdl_launcher` can also monitor HF **aeronautical voice** channels, decode
SELCAL selective calls in real time, and let dashboard users listen live.  This
is entirely separate from HFDL — it uses its own USB receiver sessions.

The shipped `docker-compose.yml` **enables this by default** on five Shannon
Aeradio (Shanwick) frequencies.  Set `SELCAL_FREQS: ""` to turn it off; the
launcher itself does nothing unless `-selcal-freqs` / `SELCAL_FREQS` is set, so
the tab disappears from the dashboard entirely when it is empty.

```
UberSDR server
     │  WebSocket (usb, pcm-zstd, version 2) — one session per frequency
     ▼
hfdl_launcher ──┬──▶ SELCAL detector  ──▶ /selcal, /events   (always running)
                └──▶ µ-law relay      ──▶ /selcal/audio      (on demand)
```

Every configured channel is decoded **continuously**, whether or not anyone is
listening.  Browser listeners attach to the launcher rather than to the
receiver, so the number of receiver sessions is fixed by the configuration and
does not grow with the audience.

### Configuration

```yaml
# Comma-separated  kHz[:label[:audio]].  Empty = feature disabled.
SELCAL_FREQS: "5598:NAT-A,8864:NAT-B,8879:NAT-C,8906:NAT-A,13306:NAT-A"
```

A third field of `audio` relays a channel to listeners without running the
decoder on it — useful for a continuous broadcast such as
`5505:Shannon Volmet:audio`, which carries no SELCAL but is a convenient way to
confirm the audio path works without waiting for a call.

| Env var | Flag | Description |
|---------|------|-------------|
| `SELCAL_FREQS` | `-selcal-freqs` | Channels to monitor (unset = disabled) |
| `SELCAL_AUDIO` | `-selcal-audio` | Set to `0` to decode only and not relay audio |
| `SELCAL_MAX_LISTENERS` | `-selcal-max-listeners` | Cap on **total** concurrent listeners |
| `SELCAL_RECORD_DIR` | `-selcal-record-dir` | Save a WAV of each detection (unset = off) |
| `SELCAL_RECORD_SECONDS` | `-selcal-record-seconds` | Audio window saved per detection |

The suggested default set covers **Shannon Aeradio** (Shanwick), which is the
station that transmits the SELCAL, spread across three bands so that at least
one is open at any hour: 5598 kHz at night, 8906/8864/8879 kHz around the clock,
13306 kHz by day.

### Bandwidth

Audio is relayed as 8-bit G.711 µ-law rather than 16-bit PCM, so each listener
costs **96 kbit/s** of upload at the 12 kHz USB sample rate.  The listener cap is
deliberately a **single receiver-wide budget** rather than a per-channel limit,
because upload bandwidth is shared across channels — ten listeners on one
frequency and one listener on each of ten frequencies cost exactly the same to
serve.  At the default cap of 10 the worst case is ~960 kbit/s.  The dashboard
shows the current total and the committed upload rate.

Receiving costs roughly 190 kbit/s per configured channel, regardless of
listeners.

### How the decoder works

A SELCAL transmission is two 1-second tone pulses, each carrying two
simultaneous tones, separated by a 0.2-second gap.  The 32-tone table from ICAO
Annex 10 Vol III Table 3-1 (Amendment 91) is used, so classic 16-tone codes and
the expanded SELCAL32 codes are both decoded by the same path; a call is flagged
`SELCAL32` when any of its four characters is from the expanded `T`–`9` set.

Audio arrives already demodulated, so a tone at audio frequency *f* is simply a
spectral line at *f*.  Each analysis frame is Hann-windowed and transformed,
peaks are located to sub-bin accuracy by parabolic interpolation, and a frame is
accepted only when exactly two peaks dominate: both well clear of the in-band
noise floor, within 8 dB of each other, and at least 8 dB above any third peak.
That last margin is what rejects harmonics and intermodulation products, which
the 15% audio distortion permitted by the standard can place close to real tone
frequencies.  Both tones must also share the same small frequency offset, since
a receiver or transmitter error shifts every tone by the same number of Hz.

Ground stations key SELCAL simultaneously on several frequencies of whichever
family is in use, so the same call arriving on more than one monitored channel
is collapsed into a single entry listing every frequency it was heard on.  That
makes each call a simultaneous propagation measurement from a transmitter at a
known location.

### Signal levels

For non-IQ modes the receiver attaches a signal-quality measurement to every
audio packet, so the dashboard shows a live signal and noise figure in dBFS for
every configured channel at all times — no listener required.  These are pushed
over the existing `/events` SSE stream as `selcal_signal` events.  The meters
read empty at 30 dB and full at 60 dB, since an idle HF voice channel already
sits around 30–35 dB.

A decoded call is quoted with the **peak receiver level measured over the burst
itself** (`snr_db`), which is the same calibrated scale as the meters — so a
call and a channel meter can be read against one another.  The peak rather than
the mean, because a burst is two tone pulses either side of a silent gap.

Each call also carries `margin_db`: how far the weaker of the two tones stood
above the in-band noise floor, taken from the worse of the two pulses.  That is
a decode-confidence figure, not a signal report — it carries the FFT's
processing gain and so reads roughly 13–21 dB above the true audio SNR.  The two
are kept as separate fields precisely because they are not comparable; the UI
shows the margin as a tooltip.  When the receiver supplies no measurement
(protocol version 1, or an older server) `has_snr` is false and only the margin
is available.

### Squelch

Each channel has its own squelch threshold, set with a slider on the same 30-60
dB scale as the signal meters, so a slider position can be read directly against
that channel's signal bar.  It is a listener-side gate: it mutes what you hear
and never affects decoding, which continues on every channel regardless.

The bottom of the range is "Off" rather than a 30 dB gate, since an idle channel
already sits at 30-35 dB and a gate there would never close.

**Auto** sets the threshold to **3 dB above that channel's average level over
the last 3 seconds** -- the average being the ambient noise floor, so the margin
puts the gate just clear of it and anything louder than ambient opens it.  Press
it during a quiet moment: taken mid-transmission the average includes the speech
itself and the threshold lands too high.

The gate opens the instant the level reaches the threshold, and closes only once
the level has stayed below it **continuously for 0.5 seconds**.  That dwell is
what prevents chattering, and it rides over the gaps between syllables rather
than clipping them.  It is used in place of a hysteresis band, which would have
been the wrong tool here -- a signal parked just under the threshold would have
held the gate open indefinitely instead of closing.

For the gate to react quickly enough, the receiver's signal level travels **with
the audio** rather than only on the two-second status stream: each relay frame is
a two-byte little-endian signed header carrying the level in hundredths of a dB,
followed by the u-law samples.  That is under 1% overhead and lets the gate
respond within one packet (~20 ms); gating on the status feed instead would clip
the opening of every transmission.  A sentinel marks packets for which the
receiver supplied no measurement, and the gate fails **open** on those rather
than muting silently.

A useful side effect: the channel being listened to gets a level roughly fifty
times a second, so its meter tracks the signal live instead of stepping every two
seconds.  Thresholds are stored per channel in `localStorage` and survive a
reload.

### When a call is audible but not decoded

Set `SELCAL_DEBUG: "1"` (or `-selcal-debug`).  The launcher then logs every
candidate pulse it saw, why any that were rejected failed, and how close each
threshold came:

```
selcal[NAT-A]: pulse AB  0.68 s  margin 34.2 dB  offset +0.4 Hz  [no rejections]
selcal[NAT-A]: did not pair AB with CD: gap of 1.10 s exceeds 0.90 s
selcal[NAT-A]: pulse CD  0.66 s  margin 31.8 dB  offset +0.3 Hz
               [the two tones differ in level by too much (7); worst imbalance 18.3 dB (limit 16)]
```

That distinguishes the failure modes quickly:

| What the log shows | What it means |
|---|---|
| Two pulses logged, no code | The pairing rules rejected them — the reason is printed |
| One pulse logged | The other pulse was rejected frame by frame; the counts say which threshold |
| No pulses logged | Nothing cleared the per-frame tests at all — check the signal actually reached the passband |
| `largest offset N Hz` near the limit | The station is off frequency; tone identification becomes ambiguous past half the minimum tone spacing |

The worst-case figures (`worst imbalance`, `closest third peak`, `largest
offset`) are printed alongside their limits, so the log says directly which
constant to move.

### Testing against a real receiver

```bash
SELCAL_LIVE_URL=https://receiver.example.org \
SELCAL_LIVE_FREQS=8906:NAT-A,5505:Volmet \
SELCAL_LIVE_SECONDS=60 \
  go test ./cmd/hfdl_launcher/ -run TestLiveReceiver -v
```

This connects for real and checks the WebSocket handshake, the pcm-zstd/version-2
negotiation, and that audio and signal-quality data are flowing.  The rest of
the test suite is entirely offline.

---

## MQTT / Home Assistant

If the UberSDR receiver has MQTT enabled, the launcher publishes through it
automatically.  **There is nothing to configure and nothing to add to
`docker-compose.yml`** — the ingest endpoint is derived from `UBERSDR_URL`.

When the receiver has MQTT turned off, or this container is not a recognised
addon, publishing is silently skipped and decoding is unaffected.  Every publish
is enqueued, never sent inline, so a slow receiver cannot stall the decode path.

### Why the feed is curated

HFDL squitters arrive on a fixed ~32 s cadence *per ground station per
frequency*, so the raw message floor scales with (ground stations heard ×
frequencies monitored).  On a well-sited receiver running several frequency
groups that alone can approach the receiver's whole 120 msg/min ingest budget —
while carrying nothing anyone would read.

So the raw feed is **not** published.  Four event streams carry the interesting
fraction, each with its own token bucket so a burst on one cannot starve the
others:

| Topic | Retained | Contents | Cap |
|-------|----------|----------|-----|
| `…/addons/hfdl/acars` | no | ACARS messages carrying operator text | 30/min |
| `…/addons/hfdl/aircraft` | no | **First** sighting of an airframe, not every message from it | 10/min |
| `…/addons/hfdl/selcal` | no | Selective calls, deduplicated across channels | 10/min |
| `…/addons/hfdl/events` | no | Logon, logoff and ground-station change notes (`kind` field) | 10/min |
| `…/addons/hfdl/summary` | yes | Counters, rollups and last-seen state, every 30 s | — |
| `…/addons/hfdl/status` | yes | `online` / `offline`, maintained by UberSDR | — |

Anything the caps discard is counted and reported as `dropped_events` (with a
per-topic `dropped_detail`) in the summary, so a curated feed never silently
looks complete.

An ACARS event:

```json
{
  "time": 1755500000,
  "freq_khz": 10081,
  "sig_level": -18.5,
  "flight": "BAW117",
  "reg": "G-STBA",
  "icao": "4008F3",
  "label": "H1",
  "msg_type": "Unnumbered data",
  "text": "POS N51.5 W000.1 FL380"
}
```

A SELCAL spot lists every channel that carried it, which makes each call a
simultaneous multi-frequency propagation measurement from a transmitter at a
known location:

```json
{
  "time": 1755500100,
  "code": "AB-CD",
  "selcal32": false,
  "snr_db": 12.5,
  "freqs_khz": [8864, 8906],
  "channels": [
    {"freq_khz": 8864, "label": "NAT-B", "snr_db": 12.5, "margin_db": 20.1},
    {"freq_khz": 8906, "label": "NAT-A", "margin_db": 17.3}
  ]
}
```

ACARS text is sanitised to printable ASCII and capped at 1 KiB.  It comes off a
noisy HF link, so a corrupt decode can carry arbitrary bytes — those are not
valid UTF-8, and a JSON string must be, or the payload reaches the broker and
then fails to parse in Home Assistant.

### Home Assistant

Eleven entities, all reading the single retained `summary` topic:

| Entity | Type | Notes |
|--------|------|-------|
| Messages Received | sensor | Running total, `total_increasing` |
| Message Rate | sensor | msg/min since start |
| Aircraft Tracked | sensor | Seen in the last 30 minutes |
| Ground Stations Heard | sensor | Same window |
| Frequencies Active | sensor | Channels that have produced messages |
| Last Aircraft | sensor | ICAO, reg, frequency and GS as attributes |
| Last ACARS | sensor | Label, frequency and a 256-char text preview as attributes |
| Last SELCAL | sensor | Code, with frequencies and SNR as attributes |
| Furthest Aircraft | sensor | km from the receiver — the best-DX figure |
| Decoders Running | sensor | Diagnostic |
| Decoder Problem | binary sensor | On when an instance is down and not deliberately stopping |

**Furthest Aircraft** needs the receiver's coordinates, which are fetched once
from UberSDR's `/api/description` and cached.  A receiver with no GPS configured
simply omits the figure rather than reporting a distance from 0°N 0°E.

**There are deliberately no per-aircraft entities.**  Aircraft churn constantly
while Home Assistant's entity registry is permanent, so per-airframe entities
would accumulate thousands of dead rows within a week and blow the receiver's
20-entity cap immediately.  Aircraft are published as *events*; build a map from
those if you want one.

### Overriding the endpoint

Only needed if the operator has changed `mqtt.addon_ingest.port` from its
default of 6926:

```yaml
environment:
  UBERSDR_INGEST_URL: "http://ubersdr:7000"
```

See `addon_mqtt.md` in the ka9q_ubersdr repository for the full ingest API.

---

## Protocol notes

UberSDR delivers IQ data as binary PCM-zstd WebSocket messages.  `ubersdr_iq`
handles both packet types emitted by the server:

- **Full header** (`0x5043` "PC") — carries sample rate, channel count, and
  optional signal quality fields (v2)
- **Minimal header** (`0x504D` "PM") — continuation packets that reuse the
  parameters from the last full header

Samples arrive as big-endian int16; `ubersdr_iq` byte-swaps them to
little-endian CS16 before writing to stdout, which is the format dumphfdl
expects via `--sample-format CS16`.
