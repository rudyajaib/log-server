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
| `-history` | `~/Library/Logs/oslog-live/session.ndjson` | file that receives every captured entry, so you can read back further than the RAM buffer (`""` disables it) |
| `-history-max-mb` | `256` | stop recording history at this size; the UI warns and offers to clear it |
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
| **Subsystem** / **Category** / **Process** | Multi-select pickers showing every value seen during the capture with its session total. Selections act as OR inside a picker and AND across pickers. `*` wildcards work (e.g. `com.apple.*`). |
| **Level** | Toggles for `Error`, `Fault`, `Default`, `Info`, `Debug`, `Signpost`, `Activity`, `Loss`, … |
| **Search** | Case-insensitive substring across message, subsystem, category, process and level. `.*` switches to a regular expression, `Aa` makes it case sensitive. Matches are highlighted. |
| **hide entries without a subsystem** | Hides noisy framework output that carries no subsystem. |
| **Predicate** | An extra NSPredicate for the `log stream` command (e.g. `subsystem BEGINSWITH "com.example"`), combined with the capture settings using AND. |

Changing a filter never empties the view: the rows on screen stay put until the new
snapshot arrives, at which point they are swapped in one step.

### The value list is scoped to the capture, not to the buffer

The pickers list **every value seen since the capture started**, each with its session total
and how long ago it was last logged (`2m ago`, highlighted once it stops). Values are never
removed while a capture runs, so a subsystem that logs once every fifteen minutes stays
selectable instead of appearing and then vanishing as the buffer window slides past it.

That matters because the buffer is a window measured in seconds. Measured on a live
simulator with a 3,000 entry buffer: 29,553 entries had been evicted and the window covered
about 1.8 seconds. Six subsystems were still listed whose entries had *all* left the buffer;
a buffer-scoped list showed 24 values where the session list showed 30.

Selecting a value that is no longer in the buffer is not a dead end — "Load older", or
scrolling to the oldest row, pages backwards through the history file to find it.

Each picker is capped at the 1,000 most frequent values (the popover says so when a cap is
hit, e.g. `showing 1000 of 1234 values seen`); the search box inside the picker filters the
list, and the catalog itself is capped per dimension so a long session cannot grow it without
limit.

### Rendering at high rates

The loaded entries and the rendered rows are deliberately separate. Every entry the
browser receives is kept, but only the newest 800 rows live in the DOM at once (`5,000 loaded
· 800 rendered` in the footer). Scrolling to the oldest rendered row extends the window from
memory, and paging continues into the history file, so nothing is lost. Appends are batched
into one animation frame instead of one per network message, which keeps frame times flat as
the rate climbs — worth knowing if you were seeing sluggishness on a busy stream.

### Capture settings

Pressing **Start capture** opens a dialog with three settings, because they decide what is ever
recorded — the difference between a manageable history and hundreds of megabytes is made here,
before anything is captured. The footer shows the current setting and reopens the dialog
(`· observing 2 subsystems — edit`).

| Setting | Effect |
|---------|--------|
| **Only these subsystems** | One per line. When set, only those subsystems are captured. Empty means everything. |
| **Ignore everything from `com.apple`** | Drops the system namespace, which is noise when investigating your own app. |
| **Ignore entries without a subsystem** | Drops entries carrying no subsystem at all — usually framework chatter. |

Where possible the settings become part of the `log stream` predicate so the filtering happens
at the source, and they are enforced *again* as entries arrive, so an excluded entry never
reaches the buffer, the value list or the history file even if the predicate is ignored.

Two rules are worth knowing:

* **An explicit subsystem list is decisive.** A listed `com.apple.*` subsystem is still
  captured even with the com.apple exclusion ticked, and the predicate is emitted as the list
  alone. Adding both would produce a predicate that contradicts itself and captures nothing —
  the dialog says so when you tick both.
* **"Ignore entries without a subsystem" is applied on arrival, not at the source.** NSPredicate
  cannot express it: `subsystem != ""` does not exclude a missing subsystem (measured: 9,926
  events kept where 7,667 was correct) and `subsystem.length` is not a valid field. It still
  keeps such entries out of the buffer, the list and the file.

### Keeping the capture small

Unscoped, the history file grows quickly: 656 bytes per entry measured on a live app, so a
firehose of ~1,250 entries/s adds about **47 MB per minute** and fills the default 256 MB cap
in a few minutes. Two ways to bring that down:

1. **Scope the capture.** Put the subsystems you care about in the capture settings. Filtering
   at the source reduced this capture from ~800 entries/s to ~72/s, and scoping to a sparse
   subsystem such as a websocket client reduces it to a handful of entries per minute — a few
   KB per minute, which the history file will hold for weeks. Ticking both exclusions on this
   workload removed every remaining event.
2. **Raise the level only as far as you need.** `Info` and `Debug` are an order of magnitude
   more verbose than `Default`; see the table below.

The footer always shows what is being recorded (`· observing 2 subsystems`, `· history 39,225
entries (23 MB)`), and `-history-max-mb` sets the cap.

### Capture level matters more than you expect

`log stream --level` drops the quieter tiers **at the source**, so an entry filtered there
can never be recovered by the UI. Measured on a live app, all three streams running over
the same 12 seconds:

| `--level` | Default | Info | Debug | Error | Fault |
|-----------|---------|------|-------|-------|-------|
| `default` | 603 | – | – | 17 | 26 |
| `info` | 604 | 568 | – | 17 | 26 |
| `debug` | 602 | 568 | 4151 | 17 | 26 |

`Error` and `Fault` always come through. If your app logs with `Logger.debug`/`info`
(websockets, SDKs), pick **Info** or **Debug** before starting.

### Why a rare subsystem can seem to be missing

The viewer is a live capture, not a copy of the persistent store Xcode reads. Two windows
bound what is on screen, and both shrink as the log rate rises:

| Log rate | 20,000 entry buffer covers | visible tail |
|----------|---------------------------|--------------|
| ~160/s (idle app) | ~125 s | ~125 s |
| ~5,300/s (busy app) | ~4 s | ~4 s |

A subsystem that logs once every fifteen minutes will usually fall outside both windows,
while a subsystem logging every few seconds is nearly always present. Three ways to fix it:

1. **Scope at the source** with *Observe only these subsystems*, e.g. the websocket
   subsystems. Volume collapses, so the buffer covers hours instead of seconds.
2. **Select it anyway.** The value list is session scoped, so the subsystem is still listed
   (with a stale timestamp) even after its entries have left the buffer. Pick it, then press
   *Load older* to read back through the history file.
3. **Raise `-buffer`** if you prefer to keep more in RAM.

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
| `POST` | `/api/stream/start` | Start a capture: `{"kind":"simulator\|device","id":"<udid>","name":"…","level":"debug\|info\|default","predicate":"…","subsystems":["…"],"ignoreApple":true,"ignoreNoSubsystem":true}` |
| `POST` | `/api/stream/stop` | Stop the capture |
| `GET` | `/api/status` | Capture state, counters, rate, errors. Pass the same filter parameters to get an accurate `matched` count for that view. |
| `GET` | `/api/facets` | Subsystem/category/process/level counts, scoped by the current filter |
| `GET` | `/api/logs` | Buffered entries (`limit`, `after`, `before`). Paging backwards with `before` continues into the history file; the response's `source` says where the page came from (`buffer`, `buffer+history`, `history`). |
| `GET` | `/api/stream` | **SSE** feed: `snapshot` (mode `full`/`resume`, with matching facets), `log`, `status`, `warning` events |
| `GET` | `/api/export` | Download filtered entries (`format=json\|csv\|ndjson\|txt`) |
| `POST` | `/api/buffer/clear` | Drop the in-memory buffer |
| `POST` | `/api/history/clear` | Empty the history file and resume recording |
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
* **Catalog** – a small per-dimension index of every value seen since the capture started, with a
  session total and a last-seen time. The pickers read this rather than the buffer, because a
  buffer-shaped value list churns: a rare subsystem is inside the window for well under a second.
* **History** – every entry is also appended to a newline delimited JSON file. Backwards paging
  reads that file from the end in blocks, so reading older entries costs only as much as it
  needs to and a session can be explored far past the RAM buffer. The file is truncated when a
  new capture starts, keeping it aligned with the buffer.
* **Scanning** – the value list needs no buffer scan at all; it is maintained as entries
  arrive. Only the match count walks the buffer, with a lowercase haystack prepared at ingest so
  it costs about a millisecond per 20,000 entries. These scans hold the buffer lock, so their
  cost is ingest latency, which is why they are kept small.
* **Scope** – an optional subsystem allowlist becomes part of the `log stream` predicate and is
  checked again in the ingest path, so out-of-scope entries occupy nothing.
* **Fan-out** – each browser subscription carries its own filter, so a viewer never receives the
  unfiltered firehose. Entries are coalesced into batches; a viewer that cannot keep up drops
  entries and is told how many, rather than stalling the capture.
* **View** – the browser keeps every loaded entry, tracks the live tail while it is scrolled to
  the end, and shows a count of new entries when it is not. Filter changes replace the rows in a
  single DOM swap, so the table is never seen empty.
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
* The buffer is in memory only; restarting loses it. The history file keeps the session on
  disk, but starting a new capture truncates it, and it stops recording at `-history-max-mb`.
* Entries emitted **before** a capture starts are not imported. The history file covers the
  session, not the device's persistent store, so scroll back within the session rather than
  expecting the last hour of history on connect.
* `print()` / `debugPrint()` write to stdout and never reach OSLog. Only `os_log`, `Logger`,
  `NSLog` and `os_signpost` appear here.
* Signposts, activities and dropped-message (`lossEvent`) records are shown as separate levels.
  Signpost name/type/scope are included in the JSON details and exports.
