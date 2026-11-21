# Tasks (v0.2 design)

Status: proposed. Nothing here ships in v0.1. When it is built, the API parts move into
`docs/DESIGN.md` in the same PR.

v0.1 lends spare capacity to CI. v0.2 lends the same capacity to agents for other work: fetch a
page every hour and post what changed, render a site in a headless browser and save the table,
turn a folder of `.mov` files into `.mp4`, convert a Markdown report to PDF and attach it to a
GitHub release. An agent says what it wants in one call. Tentmaker picks a device in the pool,
runs it in the same sandbox CI uses, and sends the result where the agent asked, with
credentials the agent never sees.

## Goals

1. **One call.** `tentmaker task run convert --in talk.mov --to mp4 --send files:nas` or one
   MCP `task_run` call does the whole thing. No image to build, no YAML to write.
2. **CI keeps its share.** The everyday budget stays reserved for CI. Tasks get what CI leaves.
3. **Same fence as CI.** A task runs in the job sandbox of its device: own user, own network
   namespace, memory and CPU limits, a fresh directory, nothing left behind.
4. **Secrets stay put.** Sinks (Slack, GitHub, S3 and the rest) run on the device, outside the
   sandbox, with credentials from the OS secret store. A task names a sink; it never holds a
   token, and no output it writes can change where a sink sends.
5. **Reliable.** Every task has a deadline, an idempotency key and a retry policy. A sink
   delivers at least once with a dedupe key, so a retried send doesn't post twice where the
   target supports it.

Not goals for v0.2: sites behind a sign-in, email as a sink, GPU work, tasks longer than 6
hours, and a general workflow engine. A task is a short pipeline, not a DAG scheduler.

## Shape of a task

A task is a list of steps. Each step reads `/in`, writes `/out`, and the next step's `/in` is the
last step's `/out`. After the last step the device collects `/out` and hands it to the sinks.

```json
{
  "key": "weekly-prices-2026-48",
  "steps": [
    {"recipe": "browse", "url": "https://example.com/prices", "select": "table#prices", "as": "html"},
    {"recipe": "transform", "from": "html-table", "to": "csv"},
    {"recipe": "convert", "to": "xlsx"}
  ],
  "send": ["slack:team", "files:nas/prices/"],
  "when": {"every": "1h", "only_on_change": true},
  "limits": {"memory": "2GiB", "cpus": 1, "deadline": "10m"}
}
```

`key` makes a submit idempotent: the same key within 7 days returns the existing task. `when` is
optional; without it the task runs once.

## Recipes

A recipe is a named step with checked arguments, so an agent fills in fields instead of writing
shell. Each one maps to fixed argv in the task image; arguments never reach a shell.

| Recipe | Does | Tools in the image |
| --- | --- | --- |
| `fetch` | HTTP GET or POST of public URLs, with retries, a per-host rate limit and robots.txt honored by default | Go stdlib (a small `tm-fetch` helper) |
| `browse` | render a page in headless Chromium; save HTML, text, a CSS-selected part, a screenshot or a PDF | Chromium headless shell |
| `extract` | pull structured data: CSS selectors, JSON paths, HTML tables, PDF text and tables, OCR of images and scans | `tm-extract`, poppler, tesseract |
| `transform` | reshape data: JSON, CSV, TSV, NDJSON, HTML table, Markdown table; filter, sort, dedupe, join, template | `tm-transform` (Go), jq |
| `convert` | file conversion by extension pair: video and audio, images, documents, spreadsheets, archives | ffmpeg, libvips, pandoc, LibreOffice headless, ghostscript, 7z |
| `media` | common media jobs: trim, transcode to a preset, loudness normalize, thumbnails, contact sheet, GIF | ffmpeg |
| `doc` | merge, split, compress, watermark and page-range PDFs; Markdown or HTML to PDF with a stylesheet | qpdf, ghostscript, Chromium print |
| `archive` | zip, unzip, tar, checksum and manifest a folder | Go stdlib, 7z |
| `diff` | compare `/in` with the last successful run of the same task and write only what changed; empty output skips the sinks | `tm-diff` |
| `run` | a command in a named image the device allows, for work no recipe covers | podman |

`convert` picks the tool from the input and output types and refuses a pair it can't do, before
anything runs. `run` is off unless the device lists the image under `tasks.allow_images`; it is
the escape hatch, not the main road.

The task image (`ghcr.io/baney75/tentmaker-tasks`) is one Linux OCI image with every tool above,
pinned by digest per release and checked against the release's signed manifest, like the
binary. A device pulls it once and reuses it.

## Where a task runs

- **Linux:** rootless podman in the job's network namespace, exactly like a CI container job.
- **macOS:** inside a Tart Linux guest, the same VM path CI uses for Linux jobs on a Mac.
- **Windows:** not in v0.2. Windows workers advertise no task platform, so tasks never land
  there. WSL support is a later question.

Network: a task may reach the public internet, since scraping needs it, and nothing else. The
nftables rules (and Softnet on macOS) drop private, link-local, CGNAT and tailnet ranges and this
device's own addresses, so a task can't reach the LAN, the tailnet or the peer and control APIs.
DNS answers that point into those ranges are dropped too.

## Budget

Tasks and CI share the device's slots, and CI comes first.

- A worker fills free slots with CI offers before task offers on every tick.
- Under the everyday profile a task takes a slot only when no broker in the pool reports a
  queued CI job this device could run. Under boost both fill freely, CI first.
- A running task is never evicted to make room for CI. Its deadline (default 30 minutes, at
  most 6 hours) bounds how long it can hold a slot. Safety guards drain and evict tasks the same
  way they do CI.
- `tasks.max_slots` caps how many slots tasks may hold at once (default: half, rounded up).

## Who brokers a task

The device that accepts the submit is the task's broker. It keeps the queue in
`tasks/state.json` (written with `config.WriteAtomic`) and hands tasks out through the same
`core.WorkSource` path checks mode uses: a `WorkOffer` with a task body, inputs streamed over
`GET /v1/archive/<ref>`, and the finished `/out` returned with the next `WorkRequest`. Inputs and
outputs are capped (default 2 GiB each) and content-addressed, so a retry doesn't resend what the
broker already has.

A scheduled task (`when.every`) lives on its broker. If that device is gone, the task waits for
it; v0.2 does not move schedules between devices.

## Sinks

Sinks are set up once per device, by the owner, and named:

```toml
[sinks.team]
kind = "slack"           # incoming webhook; the URL is in the secret store as sink/team

[sinks.alerts]
kind = "ntfy"
topic = "baney-ci"

[sinks.repo]
kind = "github"
repo = "baney75/reports"
action = "release-asset" # or issue, comment, file (a commit on a branch, opened as a PR)

[sinks.nas]
kind = "files"
device = "abbey1"        # a pool device; or kind = "s3" with endpoint and bucket
path = "/srv/drop"
```

| Kind | Sends |
| --- | --- |
| `webhook` | POST of the result as JSON or the file itself, to a URL kept in the secret store |
| `slack`, `discord` | a message with the text output, files attached where the service allows |
| `ntfy` | a push notice, with a link when the output is a file |
| `github` | issue, comment, release asset, or a file committed on a branch and opened as a PR |
| `files` | a folder on a pool device, over the mesh, under the folder the sink names and nowhere else |
| `s3` | an S3-compatible bucket and prefix |

Rules:

- The sink runs on the broker device after the task ends. The task's sandbox never sees the
  sink's credentials or URL.
- A task can name only sinks the submitter may use: the local owner may use all of them; a pool
  peer only those listed in `tasks.peer_sinks`.
- Output decides only the content. Paths are cleaned and must stay under the sink's folder,
  names are sanitized, and a `github` sink's repo and branch come from config, never from output.
- Each send has a dedupe key (task key plus run number) passed as the target's idempotency
  header or recorded in state, retries with backoff for 1 hour, then marks the run
  `send_failed` and keeps the output for 7 days so `tentmaker task resend` can try again.

## Interfaces

CLI:

```
tentmaker task run <recipe> [flags] [--send <sink>...] [--every <dur>] [--json]
tentmaker task submit <task.json>
tentmaker task list | status <id> | result <id> [--out <dir>] | cancel <id> | resend <id>
tentmaker sink add <name> --kind <kind> [...]   # reads the secret from stdin, never argv
tentmaker sink list | test <name> | remove <name>
tentmaker recipes [--json]                       # every recipe with its fields and types
```

MCP tools (same stdio server as today): `task_run`, `task_submit`, `task_status`,
`task_result`, `task_cancel`, `task_list`, `sink_list`, `recipes`. `task_run` returns the task id
at once; `task_status` with `wait: <seconds>` blocks up to 60 s so an agent doesn't poll in a
tight loop. Results under 1 MiB come back inline; larger ones return a path on the agent's own
device after `task_result --out`.

Control API: `POST /v1/tasks`, `GET /v1/tasks`, `GET /v1/tasks/<id>`,
`GET /v1/tasks/<id>/out`, `DELETE /v1/tasks/<id>`, all behind the control token. Peer API:
`POST /v1/tasks` from a pool peer, with the same WhoIs and pool checks as `/v1/work`.

## Failure behavior

| Failure | What happens |
| --- | --- |
| step exits non-zero | the run fails with the step's last 4 KiB of stderr, secrets scrubbed; retries per `retry` (default 2, backoff) |
| deadline passes | the sandbox is stopped and cleaned, the run is `timeout` |
| device drains or evicts | the run goes back to the queue and starts over on any device (steps are rerun, not resumed) |
| broker restarts | queue and schedules come back from `tasks/state.json`; a run whose worker is silent 2 minutes is requeued |
| image digest doesn't match the release manifest | no task runs on that device, `doctor` reports it |
| a sink is down | retries for 1 hour, then `send_failed`; output kept 7 days |

## Build plan

1. `internal/task`: spec types, recipe registry with checked fields, step runner that maps
   recipes to argv. Unit tests with a fake executor.
2. Task image and its manifest in the release pipeline; `doctor` checks the digest.
3. Network fence for tasks: public only. An abuse test like the CI one proves the LAN, tailnet
   and control API are unreachable from a task.
4. Broker queue and the worker's CI-first slot order. Property tests: a task never takes an
   everyday slot while CI is queued; a task is never evicted for CI.
5. Sinks, one package each, behind a small `Sink` interface with fakes; path and dedupe tests.
6. CLI, MCP tools and control API; `recipes --json` as the single source the MCP schema is
   built from.
7. Docs and a demo: watch a page hourly and post changes to ntfy; convert a folder of videos to
   a pool device.

Open questions for review: whether `browse` should honor robots.txt by default (proposed: yes,
with a per-task override the owner must allow in config), and whether Windows should get the
recipes that don't need Linux (`fetch`, `transform`, `archive`) natively in v0.2.
