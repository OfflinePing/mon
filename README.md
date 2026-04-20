# mon

`mon` is a small Linux system monitor written in Go. It samples CPU, memory, disk, and per-process activity, writes newline-delimited JSON (`.jsonl`), and can also serve a live browser dashboard from the same data.

It is designed for simple local monitoring, log-based analysis, and lightweight self-hosted dashboards without external agents or dependencies.

## Features

- Records point-in-time system metrics as JSON Lines for easy scripting and ingestion.
- Samples total CPU usage from `/proc/stat`.
- Samples memory usage from `/proc/meminfo`.
- Samples one or more disk paths via `statfs`.
- Captures the busiest processes with CPU and RSS memory usage.
- Runs continuously or stops after a fixed number of samples.
- Appends to an existing log file or truncates and starts fresh.
- Serves a built-in live dashboard in the browser.
- Replays an existing `.jsonl` file into the dashboard on startup.
- Supports combined collection plus dashboard serving with a single process.
- Sends Discord alerts when CPU usage crosses the built-in threshold.
- Uses only the Go standard library.

## Requirements

- Linux
- Go 1.22+

`mon` reads directly from `/proc` and uses Linux filesystem statistics, so it is not intended for macOS or Windows.

## Installation

Build from source:

```bash
go build -o mon .
```

Run without installing:

```bash
go run .
```

## Quick Start

Collect metrics every 2 seconds and write them to `metrics.jsonl`:

```bash
./mon -interval 2s -output metrics.jsonl
```

Collect 300 samples and stop:

```bash
./mon -interval 1s -count 300
```

Track multiple disk paths and keep the top 10 processes:

```bash
./mon -paths /,/home,/var -top 10
```

Append new samples to an existing file:

```bash
./mon -append -output metrics.jsonl
```

## Dashboard Modes

Serve the dashboard using an existing metrics file:

```bash
./mon -web -output metrics.jsonl -port 65432
```

Collect metrics and serve the dashboard at the same time:

```bash
./mon -dual -output metrics.jsonl -interval 2s -port 65432
```

When the web server is running, open:

```text
http://localhost:65432
```

The dashboard exposes two HTTP endpoints:

- `GET /api/metrics` returns all currently loaded metric points as JSON.
- `GET /api/events` streams new metric points as Server-Sent Events.

## Alerts

`mon` can send Discord webhook alerts when CPU usage reaches or exceeds `80%`.

```bash
./mon -dual \
  -discord-webhook https://discord.com/api/webhooks/... \
  -alert-cooldown 10m
```

Alert payloads include:

- Current CPU usage
- Current memory usage
- Disk usage for the configured paths
- Up to the top 10 busiest processes

If a Discord webhook is configured and `-top` is lower than `10`, `mon` automatically raises it to `10` so alert messages include enough process detail.

## CLI Reference

```text
-alert-cooldown duration
	minimum time between Discord alerts (default 5m0s)
-append
	append to the output file instead of truncating it
-count int
	number of samples to record (0 runs until interrupted)
-discord-webhook string
	Discord webhook URL for CPU alerts
-dual
	collect metrics and serve the dashboard
-interval duration
	sample interval (default 2s)
-output string
	output file path (default "metrics.jsonl")
-paths string
	comma-separated disk paths to inspect (default "/")
-port int
	web server port (default 65432)
-top int
	number of processes to include in each sample (default 5)
-web
	start a web server to display metrics
```

## Output Format

Each line in the output file is a standalone JSON object. This makes the data easy to process with `jq`, Python, pandas, SQLite, shell pipelines, or custom importers.

Example:

```json
{
	"timestamp": "2026-04-08T17:00:00Z",
	"cpu_percent": 7.42,
	"memory": {
		"used_bytes": 4200000000,
		"free_bytes": 12300000000,
		"total_bytes": 16500000000,
		"used_percent": 25.45
	},
	"disks": [
		{
			"path": "/",
			"used_bytes": 1234567890,
			"free_bytes": 9876543210,
			"total_bytes": 11111111100,
			"used_percent": 11.11
		}
	],
	"processes": [
		{
			"pid": 1234,
			"name": "postgres",
			"cpu_percent": 2.1,
			"rss_bytes": 180224000,
			"rss_percent": 1.09
		}
	]
}
```

Schema summary:

- `timestamp`: sample time in UTC RFC3339 format
- `cpu_percent`: overall CPU usage for the sample window
- `memory`: total, used, free, and usage percentage for system memory
- `disks`: per-path disk usage entries
- `processes`: top processes ranked by CPU, with RSS usage included

## How It Works

- CPU usage is derived from deltas in `/proc/stat`.
- Memory usage is derived from `/proc/meminfo` using `MemTotal` and `MemAvailable`.
- Process activity is derived from `/proc/<pid>/stat`.
- Disk usage is derived from `statfs` for each configured path.

Because metrics are based on deltas between samples, process CPU percentages become meaningful after the first interval.

## Project Layout

```text
.
├── main.go        # CLI, collectors, alerts, and dashboard server
├── go.mod         # module definition
├── metrics.jsonl  # example output file
└── README.md
```

## Development

Run the formatter and build locally:

```bash
gofmt -w main.go
go build ./...
```

Useful manual checks:

```bash
go run . -count 5 -interval 1s
go run . -web -output metrics.jsonl
go run . -dual -count 10
```

## Limitations

- Linux only
- No historical retention management beyond writing to the chosen output file
- No authentication on the built-in dashboard server
- No configurable alert threshold yet; CPU alerts are currently fixed at `80%`

## Contributing

Issues and pull requests are welcome.

When contributing:

- Keep changes small and focused.
- Preserve the zero-dependency standard-library approach unless there is a strong reason to change it.
- Update this README when flags, output format, or runtime behavior change.

## License

This repository does not currently include a license file. Add one before publishing broadly or accepting external contributions under open source terms.
