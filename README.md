# OSLog Live

A local web app that captures Apple unified logging (**OSLog**) output from a booted **iOS
simulator** or a **connected iOS device**, shows it live in the browser, and lets you filter it
by **subsystem** and **category** (plus level, process and free-text search) and export exactly
what you filtered.

Written in Go with zero third-party dependencies. The UI is embedded into the binary, so the
whole app is a single executable.

```
                 ┌──────────────────────────────┐
  xcrun simctl ──▶│                              │
    spawn log     │   log-server (Go)            │      ┌─────────────────┐
     stream       │   • subprocess + ndjson      │ SSE  │  web UI         │
                 │   • ring buffer + facets      │─────▶│  live table     │
   log stream ───▶│   • filter / export           │      │  filters/export │
   --device-udid  │                              │      └─────────────────┘
                 └──────────────────────────────┘
```

## Requirements

* macOS (the app drives `log stream` and `xcrun simctl`)
* Go 1.25 or newer to build
* Xcode command line tools, plus Xcode for `xcrun devicectl` (device discovery)
* For a **physical device**: the server must run with admin privileges (see below)

## Quick start

```bash
go run .            # builds, starts on http://127.0.0.1:8722 and opens the browser
```

Then pick a source, press **Start capture**, and watch the log stream in.

```bash
# Simulators
go run .

# Simulators + connected iOS devices (device capture needs root)
sudo go run .
```

Flags:

| Flag | Default | Description |
|------|---------|-------------|
| `-addr` | `127.0.0.1:8722` | listen address |
| `-buffer` | `20000` | how many entries to keep in memory |
| `-open` | `true` | open the browser on start |
| `-web` | – | serve UI assets from a directory instead of the embedded copy (front-end development) |

Build a standalone binary:

```bash
go build -o oslog-live .
./oslog-live
```

## Capturing from a physical device

`log stream` refuses to read a paired device unless it runs as root:

```
log: Must be admin to run 'stream' command
```

So run the server with `sudo` to enable device capture:

```bash
sudo ./oslog-live
```

The app detects this and fails fast with that explanation instead of retrying (simulators work
without elevated privileges). If you prefer not to run the whole server as root, grant
passwordless sudo for the log tool only:

```
# /etc/sudoers.d/oslog  (use visudo)
youruser ALL=(root) NOPASSWD: /usr/bin/log
```

## Filtering

Filters are evaluated **on the server** against the whole in-memory buffer, not just the rows the
browser happens to be showing. Every filter change opens a fresh snapshot, so a search always
searches everything that has been received.

| Control | Behaviour |
|---------|-----------|
| **Subsystem** / **Category** / **Process** | Multi-select pickers populated from the live stream with counts. Selections act as OR inside a picker and AND across pickers. `*` wildcards work (e.g. `com.apple.*`). |
| **Level** | Toggles for `Error`, `Fault`, `Default`, `Info`, `Debug`, `Signpost`, `Activity`, `Loss`, … |
| **Search** | Case-insensitive substring across message, subsystem, category, process and level. `.*` switches to a regular expression, `Aa` makes it case sensitive. Matches are highlighted. |
| **hide entries without a subsystem** | Hides noisy framework output that carries no subsystem. |
| **Predicate** | An NSPredicate appended to the `log stream` command (e.g. `subsystem == "com.example.app"`). It reduces work at the source and applies when a capture starts. |

The address bar always mirrors the current filter, so it can be bookmarked or shared.
`Copy link` copies it.

## Sharing the filtered log

Export uses the same filter as the live view, so what you see is what you get:

| Format | Use |
|--------|-----|
| **JSON** | Envelope with `exportedAt`, target, filter description, count and `entries` |
| **CSV** | `id,timestamp,level,eventType,process,processId,threadId,subsystem,category,message` |
| **NDJSON** | One JSON object per line – easy to `grep`, `jq` or stream |
| **TXT** | Metadata preamble plus aligned log lines, easy to paste into a ticket |
| **Copy loaded** / **Copy link** | Clipboard helpers for the entries currently loaded in the browser, and for the filtered view |

Exports cover the entire server buffer (up to `-buffer` entries), while the browser only keeps the
most recent 3000 rows on screen. The footer reports both numbers, e.g.
*"5,984 match the filter in the server buffer · showing the most recent 3,000"*.

## HTTP API

The UI is a client of this API, which is handy for automation:

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/api/targets` | Available simulators and connected devices |
| `POST` | `/api/stream/start` | Start a capture: `{"kind":"simulator\|device","id":"<udid>","name":"…","level":"debug\|info\|default","predicate":"…"}` |
| `POST` | `/api/stream/stop` | Stop the capture |
| `GET` | `/api/status` | Capture state, counters, rate, errors |
| `GET` | `/api/facets` | Subsystem/category/process/level counts, scoped by the current filter |
| `GET` | `/api/logs` | Buffered entries (`limit`, `after`, plus any filter) |
| `GET` | `/api/stream` | **SSE** feed: `snapshot` (mode `full`/`resume`), `log`, `status`, `warning` events |
| `GET` | `/api/export` | Download filtered entries (`format=json\|csv\|ndjson\|txt`) |
| `POST` | `/api/buffer/clear` | Drop the in-memory buffer |
| `POST` | `/api/simulators/boot` | Boot a shutdown simulator |

Filters use repeatable query parameters everywhere:
`subsystem=`, `category=`, `process=`, `level=`, `q=`, `regex=true`, `case=true`, `hasSubsystem=true`.

Example – follow one subsystem live from the terminal:

```bash
curl -N 'http://127.0.0.1:8722/api/stream?subsystem=com.example.app&level=Error'
```

The SSE feed resumes without duplicates: each frame carries an `id`, and a client that reconnects
with `Last-Event-ID` receives only what it missed.

## How it works

* **Capture** – `xcrun simctl spawn <udid> log stream --style ndjson …` for simulators, or
  `log stream --device-udid <udid> --style ndjson …` for devices. Stdout is parsed as ndjson;
  non-JSON notices such as `Filtering the log data using …` are ignored.
* **Buffer** – a fixed-capacity ring buffer with monotonically increasing IDs. IDs let a browser
  reconnect and continue exactly where it left off even after older entries were evicted.
* **Fan-out** – each browser subscription carries its own filter, so a viewer never receives the
  unfiltered firehose. Entries are coalesced into batches; a viewer that cannot keep up drops
  entries and is told how many, rather than stalling the capture.
* **Resilience** – if the stream dies unexpectedly it is restarted with backoff (up to 5 times).
  Conditions that cannot be retried (not booted, not connected, missing admin rights) stop
  immediately with an actionable message.
* **Shutdown** – the stream runs in its own process group, so stopping it also tears down the
  `log` process spawned inside the simulator. No stray processes are left behind.

## Development

```bash
go test ./...              # unit + HTTP/SSE integration tests
go test -race ./...
go build ./...
```

Front-end iteration without rebuilding the binary:

```bash
go run . -web ./web -open=false
```

## Notes and limitations

* macOS only. On other platforms the app exits at startup with an explanation.
* One capture at a time. Starting a new capture stops the previous one and clears the buffer, so
  counters and facets always describe the current session.
* `-addr` defaults to loopback. The API sends no CORS headers for mutating calls and requires
  `Content-Type: application/json`, so a random web page cannot start or stop your capture. Only
  bind to a non-loopback address on a trusted network.
* The buffer is in memory only; restarting loses history. Export what you need to keep.
* Signposts, activities and dropped-message (`lossEvent`) records are shown as separate levels.
  Signpost name/type/scope are included in the JSON details and exports.
