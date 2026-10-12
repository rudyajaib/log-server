# OSLog Live

A local web app that streams Apple unified logging (**OSLog**) from a booted **iOS simulator**
into your browser in real time. Filter by subsystem, category, process, level and free-text
search, then export exactly what you see.

Written in Go with no third-party dependencies, and the UI is embedded in the binary, so it
ships as a single executable.

```text
xcrun simctl spawn <udid> log stream --style ndjson  ──▶  log-server (Go)  ──SSE──▶  web UI
```

## Requirements

* macOS (the app drives `log stream` and `xcrun simctl`)
* Go 1.25 or newer to build
* Xcode command line tools

## Quick start

```bash
go run .            # builds, serves http://127.0.0.1:8722 and opens the browser
```

Pick a source, press **Start capture**, and watch the log stream in.

Build a standalone binary:

```bash
go build -o oslog-live .
./oslog-live
```

| Flag | Default | Description |
|------|---------|-------------|
| `-addr` | `127.0.0.1:8722` | listen address |
| `-buffer` | `20000` | how many entries to keep in memory |
| `-history` | `~/Library/Logs/oslog-live/session.ndjson` | file that receives every captured entry so you can read back beyond the RAM buffer (`""` disables it) |
| `-history-max-mb` | `256` | stop recording history at this size; the UI warns and offers to clear it |
| `-open` | `true` | open the browser on start |
| `-web` | – | serve UI assets from a directory instead of the embedded copy (front-end development) |

## Physical devices are not available right now

`log stream` only reads a connected iPhone/iPad when it runs as root, so device capture needs
`sudo`. **`sudo` is blocked on our JumpCloud-managed Macs**, which means device capture cannot
be used on a real device for now — the app will report that admin privileges are required.

Use a **simulator** instead. It produces the same OSLog output and needs no elevated privileges,
so nothing else in this guide changes.

## Filtering

Filters run **on the server** against the whole in-memory buffer (not just the visible rows), and
every filter change opens a fresh snapshot.

| Control | Behaviour |
|---------|-----------|
| **Subsystem** / **Category** / **Process** | Multi-select pickers with every value seen during the capture and its session total. Selections are OR within a picker and AND across pickers. `*` wildcards work (e.g. `com.apple.*`). |
| **Level** | Toggles for `Error`, `Fault`, `Default`, `Info`, `Debug`, `Signpost`, `Activity`, `Loss`, … |
| **Search** | Case-insensitive substring across message, subsystem, category, process and level. `.*` switches to a regular expression, `Aa` makes it case sensitive. Matches are highlighted. |
| **hide entries without a subsystem** | Hides noisy framework output that carries no subsystem. |
| **Predicate** | An extra NSPredicate for `log stream` (e.g. `subsystem BEGINSWITH "com.example"`), combined with the capture settings using AND. |

The value list is scoped to the **capture**, not the buffer: a subsystem is still listed (with a
stale timestamp) after its entries leave the buffer, so pick it and use **Load older** to read
back through the history file. Each picker is capped at the 1,000 most frequent values.

Rendering stays responsive on busy streams: every received entry is kept, but only the newest
800 rows live in the DOM at once (the footer shows `loaded · rendered`). Scrolling to the oldest
row extends the window and then pages into the history file.

## Capture settings

**Start capture** opens a dialog that decides what is ever recorded:

| Setting | Effect |
|---------|--------|
| **Only these subsystems** | One per line. When set, only those subsystems are captured. Empty means everything. |
| **Ignore everything from `com.apple`** | Drops the system namespace (system noise). |
| **Ignore entries without a subsystem** | Drops entries carrying no subsystem (framework chatter). |

An explicit subsystem list is decisive: a listed `com.apple.*` subsystem is still captured even
with the com.apple exclusion ticked. Where possible the settings become part of the `log stream`
predicate, and they are enforced again as entries arrive, so excluded entries never reach the
buffer, the value list or the history file.

**Capture level matters.** `log stream --level` drops quieter tiers at the source, so an entry
filtered there cannot be recovered later. `Error` and `Fault` always come through; if your app
logs with `Logger.debug`/`info`, pick **Info** or **Debug** before starting.

Scoping the capture is the most effective way to keep the history file small — an unscoped
capture can add tens of MB per minute. The footer shows what is being recorded
(`· observing 2 subsystems`, `· history 39,225 entries (23 MB)`).

The address bar always mirrors the current filter, so a view can be bookmarked or shared
(**Copy link**).

## Sharing the filtered log

Export uses the same filter as the live view, so what you see is what you get.

| Format | Use |
|--------|-----|
| **JSON** | Envelope with `exportedAt`, target, filter description, count and `entries` |
| **CSV** | `id,timestamp,level,eventType,process,processId,threadId,subsystem,category,message` |
| **NDJSON** | One JSON object per line – easy to `grep`, `jq` or stream |
| **TXT** | Metadata preamble plus aligned log lines, easy to paste into a ticket |
| **Copy loaded** / **Copy link** | Clipboard helpers for the entries currently loaded, and for the filtered view |

Exports cover the entire server buffer (up to `-buffer` entries); the browser keeps the most
recent 3,000 rows on screen. The footer reports both counts.

## HTTP API

The UI is a client of this API, which is handy for automation:

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/api/targets` | Available simulators (and connected devices) |
| `POST` | `/api/stream/start` | Start a capture: `{"kind":"simulator","id":"<udid>","name":"…","level":"debug\|info\|default","predicate":"…","subsystems":["…"],"ignoreApple":true,"ignoreNoSubsystem":true}` |
| `POST` | `/api/stream/stop` | Stop the capture |
| `GET` | `/api/status` | Capture state, counters, rate, errors |
| `GET` | `/api/facets` | Subsystem/category/process/level counts, scoped by the current filter |
| `GET` | `/api/logs` | Buffered entries (`limit`, `after`, `before`); `before` pages into the history file |
| `GET` | `/api/stream` | **SSE** feed: `snapshot`, `log`, `status`, `warning` events |
| `GET` | `/api/export` | Download filtered entries (`format=json\|csv\|ndjson\|txt`) |
| `POST` | `/api/buffer/clear` | Drop the in-memory buffer |
| `POST` | `/api/history/clear` | Empty the history file and resume recording |
| `POST` | `/api/simulators/boot` | Boot a shutdown simulator |

Filters use repeatable query parameters everywhere:
`subsystem=`, `category=`, `process=`, `level=`, `q=`, `regex=true`, `case=true`, `hasSubsystem=true`.

```bash
# Follow one subsystem live from the terminal
curl -N 'http://127.0.0.1:8722/api/stream?subsystem=com.example.app&level=Error'
```

The SSE feed resumes without duplicates: each frame carries an `id`, and a client that reconnects
with `Last-Event-ID` receives only what it missed.

## How it works

* **Capture** – `xcrun simctl spawn <udid> log stream --style ndjson …`. Stdout is parsed as
  ndjson; non-JSON notices are ignored.
* **Buffer** – a fixed-capacity ring buffer with monotonically increasing IDs, so a browser can
  reconnect and continue exactly where it left off even after older entries were evicted.
* **Catalog** – a per-dimension index of every value seen since the capture started (with a
  session total and last-seen time), which is what the pickers read.
* **History** – every entry is appended to a newline-delimited JSON file. Backwards paging reads
  that file from the end, so a session can be explored far past the RAM buffer.
* **Fan-out** – each browser subscription carries its own filter, so a viewer never receives the
  unfiltered firehose. A viewer that cannot keep up drops entries and is told how many.
* **Resilience** – if the stream dies unexpectedly it is restarted with backoff (up to 5 times).
  Conditions that cannot be retried (not booted, missing admin rights) stop with a clear message.

## Development

```bash
go test ./...              # unit + HTTP/SSE integration tests
go test -race ./...
go build ./...

go run . -web ./web -open=false   # front-end iteration without rebuilding the binary
```

## Notes and limitations

* macOS only. On other platforms the app exits at startup with an explanation.
* One capture at a time. Starting a new capture stops the previous one and clears the buffer.
* The buffer is in memory only; restarting loses it. The history file keeps the session on disk,
  but starting a new capture truncates it and it stops recording at `-history-max-mb`.
* Entries emitted **before** a capture starts are not imported; the history file covers the
  session, not the device's persistent store.
* `print()` / `debugPrint()` write to stdout and never reach OSLog. Only `os_log`, `Logger`,
  `NSLog` and `os_signpost` appear here.
* Signposts, activities and dropped-message (`lossEvent`) records are shown as separate levels.
* `-addr` defaults to loopback, and the API requires `Content-Type: application/json` for
  mutating calls. Only bind to a non-loopback address on a trusted network.
