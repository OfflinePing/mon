# mon

Tiny Linux CLI that records CPU, memory, disk usage, and the busiest process names as newline-delimited JSON (`.jsonl`).

## Build

```bash
go build -o mon .
```

## Run

```bash
./mon -interval 2s -output metrics.jsonl
```

A few useful flags:

- `-interval 1s` sample every second
- `-count 300` stop after 300 samples
- `-paths /,/home` record multiple disk mount points
- `-top 10` include the 10 busiest processes per sample
- `-append` keep adding to an existing log file

## Output format

Each line is a standalone JSON object, which makes it easy to feed into `jq`, Python, pandas, SQLite, Grafana importers, or a quick plotting script later.

```json
{"timestamp":"2026-04-08T17:00:00Z","cpu_percent":7.42,"memory":{"used_bytes":4200000000,"free_bytes":12300000000,"total_bytes":16500000000,"used_percent":25.45},"disks":[{"path":"/","used_bytes":1234567890,"free_bytes":9876543210,"total_bytes":11111111100,"used_percent":11.11}],"processes":[{"pid":1234,"name":"postgres","cpu_percent":2.10,"rss_bytes":180224000,"rss_percent":1.09}]}
```
