package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type config struct {
	interval        time.Duration
	output          string
	paths           []string
	count           int
	top             int
	append          bool
	web             bool
	dual            bool
	port            int
	discordWebhook  string
	alertCooldown   time.Duration
}

type cpuSample struct {
	idle  uint64
	total uint64
}

type diskUsage struct {
	Path        string  `json:"path"`
	UsedBytes   uint64  `json:"used_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	TotalBytes  uint64  `json:"total_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

type memoryUsage struct {
	UsedBytes   uint64  `json:"used_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	TotalBytes  uint64  `json:"total_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

type processSnapshot struct {
	name       string
	totalTicks uint64
	rssBytes   uint64
}

type processMetric struct {
	PID        int     `json:"pid"`
	Name       string  `json:"name"`
	CPUPercent float64 `json:"cpu_percent"`
	RSSBytes   uint64  `json:"rss_bytes"`
	RSSPercent float64 `json:"rss_percent"`
}

type metricPoint struct {
	Timestamp  time.Time       `json:"timestamp"`
	CPUPercent float64         `json:"cpu_percent"`
	Memory     memoryUsage     `json:"memory"`
	Disks      []diskUsage     `json:"disks"`
	Processes  []processMetric `json:"processes,omitempty"`
}

type metricStore struct {
	mu        sync.RWMutex
	points    []metricPoint
	listeners map[chan metricPoint]struct{}
}

func newMetricStore() *metricStore {
	return &metricStore{listeners: make(map[chan metricPoint]struct{})}
}

func (s *metricStore) add(p metricPoint) {
	s.mu.Lock()
	s.points = append(s.points, p)
	for ch := range s.listeners {
		select {
		case ch <- p:
		default:
		}
	}
	s.mu.Unlock()
}

func (s *metricStore) all() []metricPoint {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]metricPoint, len(s.points))
	copy(out, s.points)
	return out
}

func (s *metricStore) subscribe() chan metricPoint {
	ch := make(chan metricPoint, 8)
	s.mu.Lock()
	s.listeners[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}

func (s *metricStore) unsubscribe(ch chan metricPoint) {
	s.mu.Lock()
	delete(s.listeners, ch)
	s.mu.Unlock()
	close(ch)
}

var store = newMetricStore()

func main() {
	cfg, err := parseFlags()
	if err != nil {
		exitf("%v\n", err)
	}

	if cfg.discordWebhook != "" && cfg.top < 10 {
		cfg.top = 10
	}

	if cfg.web {
		points, err := readMetricsFile(cfg.output)
		if err != nil && !os.IsNotExist(err) {
			exitf("read metrics: %v\n", err)
		}
		for _, p := range points {
			store.add(p)
		}
		serveWeb(cfg)
		return
	}

	if cfg.dual {
		points, err := readMetricsFile(cfg.output)
		if err != nil && !os.IsNotExist(err) {
			exitf("read metrics: %v\n", err)
		}
		for _, p := range points {
			store.add(p)
		}
		go serveWeb(cfg)
	}

	file, err := openOutput(cfg.output, cfg.append)
	if err != nil {
		exitf("open output: %v\n", err)
	}
	defer file.Close()

	writer := bufio.NewWriter(file)
	defer writer.Flush()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	prevCPU, err := readCPU()
	if err != nil {
		exitf("read cpu: %v\n", err)
	}
	prevProcesses, err := readProcesses()
	if err != nil {
		exitf("read processes: %v\n", err)
	}

	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()

	written := 0
	for {
		select {
		case <-ctx.Done():
			return
		case tickTime := <-ticker.C:
			point, nextCPU, nextProcesses, err := collectMetrics(tickTime, prevCPU, prevProcesses, cfg.paths, cfg.top)
			if err != nil {
				exitf("collect metrics: %v\n", err)
			}
			prevCPU = nextCPU
			prevProcesses = nextProcesses

			if err := writeMetric(writer, point); err != nil {
				exitf("write metric: %v\n", err)
			}
			store.add(point)
			go checkAndAlert(cfg, point)

			written++
			if cfg.count > 0 && written >= cfg.count {
				return
			}
		}
	}
}

func parseFlags() (config, error) {
	var cfg config
	var paths string

	flag.DurationVar(&cfg.interval, "interval", 2*time.Second, "sample interval")
	flag.StringVar(&cfg.output, "output", "metrics.jsonl", "output file path")
	flag.StringVar(&paths, "paths", "/", "comma-separated disk paths to inspect")
	flag.IntVar(&cfg.count, "count", 0, "number of samples to record (0 runs until interrupted)")
	flag.IntVar(&cfg.top, "top", 5, "number of processes to include in each sample")
	flag.BoolVar(&cfg.append, "append", false, "append to the output file instead of truncating it")
	flag.BoolVar(&cfg.web, "web", false, "start a web server to display metrics")
	flag.BoolVar(&cfg.dual, "dual", false, "collect metrics and serve the dashboard")
	flag.IntVar(&cfg.port, "port", 65432, "web server port")
	flag.StringVar(&cfg.discordWebhook, "discord-webhook", "", "Discord webhook URL for CPU alerts")
	flag.DurationVar(&cfg.alertCooldown, "alert-cooldown", 5*time.Minute, "minimum time between Discord alerts")
	flag.Parse()

	if cfg.interval <= 0 {
		return cfg, errors.New("interval must be greater than zero")
	}
	if cfg.output == "" {
		return cfg, errors.New("output path is required")
	}
	if cfg.count < 0 {
		return cfg, errors.New("count must be zero or greater")
	}
	if cfg.top < 0 {
		return cfg, errors.New("top must be zero or greater")
	}

	for _, part := range strings.Split(paths, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			cfg.paths = append(cfg.paths, part)
		}
	}
	if len(cfg.paths) == 0 {
		return cfg, errors.New("at least one disk path is required")
	}

	return cfg, nil
}

func openOutput(path string, appendMode bool) (*os.File, error) {
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	flags := os.O_CREATE | os.O_WRONLY
	if appendMode {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}

	return os.OpenFile(path, flags, 0o644)
}

func collectMetrics(now time.Time, prevCPU cpuSample, prevProcesses map[int]processSnapshot, paths []string, top int) (metricPoint, cpuSample, map[int]processSnapshot, error) {
	currentCPU, err := readCPU()
	if err != nil {
		return metricPoint{}, cpuSample{}, nil, err
	}

	memory, err := readMemory()
	if err != nil {
		return metricPoint{}, cpuSample{}, nil, err
	}

	currentProcesses, err := readProcesses()
	if err != nil {
		return metricPoint{}, cpuSample{}, nil, err
	}

	disks := make([]diskUsage, 0, len(paths))
	for _, path := range paths {
		disk, err := readDisk(path)
		if err != nil {
			return metricPoint{}, cpuSample{}, nil, err
		}
		disks = append(disks, disk)
	}

	return metricPoint{
		Timestamp:  now.UTC(),
		CPUPercent: cpuPercent(prevCPU, currentCPU),
		Memory:     memory,
		Disks:      disks,
		Processes:  topProcesses(prevCPU, currentCPU, prevProcesses, currentProcesses, memory.TotalBytes, top),
	}, currentCPU, currentProcesses, nil
}

func writeMetric(writer *bufio.Writer, point metricPoint) error {
	line, err := json.Marshal(point)
	if err != nil {
		return err
	}
	if _, err := writer.Write(line); err != nil {
		return err
	}
	if err := writer.WriteByte('\n'); err != nil {
		return err
	}
	return writer.Flush()
}

var lastAlertTime time.Time

func checkAndAlert(cfg config, point metricPoint) {
	if cfg.discordWebhook == "" || point.CPUPercent < 80.0 {
		return
	}
	if time.Since(lastAlertTime) < cfg.alertCooldown {
		return
	}
	lastAlertTime = time.Now()

	top := point.Processes
	if len(top) > 10 {
		top = top[:10]
	}

	var procs strings.Builder
	for i, p := range top {
		fmt.Fprintf(&procs, "%d. **%s** (PID %d) — CPU %.1f%%, RSS %.1f MB\n",
			i+1, p.Name, p.PID, p.CPUPercent, float64(p.RSSBytes)/(1024*1024))
	}

	var disks strings.Builder
	for _, d := range point.Disks {
		fmt.Fprintf(&disks, "• `%s` — %.1f%% used (%.1f / %.1f GB)\n",
			d.Path, d.UsedPercent,
			float64(d.UsedBytes)/(1024*1024*1024),
			float64(d.TotalBytes)/(1024*1024*1024))
	}

	embed := map[string]interface{}{
		"embeds": []map[string]interface{}{
			{
				"title":       "⚠️ High CPU Alert",
				"color":       0xFF4444,
				"description": fmt.Sprintf("CPU usage is at **%.1f%%**", point.CPUPercent),
				"fields": []map[string]interface{}{
					{
						"name":  "Memory",
						"value": fmt.Sprintf("%.1f%% used (%.1f / %.1f GB)", point.Memory.UsedPercent, float64(point.Memory.UsedBytes)/(1024*1024*1024), float64(point.Memory.TotalBytes)/(1024*1024*1024)),
					},
					{
						"name":  "Disk Usage",
						"value": disks.String(),
					},
					{
						"name":  fmt.Sprintf("Top %d Processes", len(top)),
						"value": procs.String(),
					},
				},
				"timestamp": point.Timestamp.Format(time.RFC3339),
			},
		},
	}

	body, err := json.Marshal(embed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "discord alert marshal: %v\n", err)
		return
	}

	resp, err := http.Post(cfg.discordWebhook, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "discord alert send: %v\n", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "discord alert: status %d\n", resp.StatusCode)
	}
}

func readCPU() (cpuSample, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuSample{}, err
	}

	line := strings.SplitN(string(data), "\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) < 8 || fields[0] != "cpu" {
		return cpuSample{}, fmt.Errorf("unexpected /proc/stat format")
	}

	values := make([]uint64, 0, len(fields)-1)
	for _, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return cpuSample{}, err
		}
		values = append(values, value)
	}

	idle := values[3]
	if len(values) > 4 {
		idle += values[4]
	}

	var total uint64
	for _, value := range values {
		total += value
	}

	return cpuSample{idle: idle, total: total}, nil
}

func cpuPercent(prev, current cpuSample) float64 {
	totalDelta := current.total - prev.total
	idleDelta := current.idle - prev.idle
	if totalDelta == 0 {
		return 0
	}
	return (float64(totalDelta-idleDelta) / float64(totalDelta)) * 100
}

func readMemory() (memoryUsage, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return memoryUsage{}, err
	}

	values := make(map[string]uint64)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return memoryUsage{}, err
		}
		values[strings.TrimSuffix(fields[0], ":")] = value * 1024
	}

	total := values["MemTotal"]
	available := values["MemAvailable"]
	if total == 0 {
		return memoryUsage{}, fmt.Errorf("MemTotal missing from /proc/meminfo")
	}

	used := total - available
	return memoryUsage{
		UsedBytes:   used,
		FreeBytes:   available,
		TotalBytes:  total,
		UsedPercent: (float64(used) / float64(total)) * 100,
	}, nil
}

func readDisk(path string) (diskUsage, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return diskUsage{}, fmt.Errorf("statfs %s: %w", path, err)
	}

	total := stats.Blocks * uint64(stats.Bsize)
	free := stats.Bavail * uint64(stats.Bsize)
	used := total - free

	usedPercent := 0.0
	if total > 0 {
		usedPercent = (float64(used) / float64(total)) * 100
	}

	return diskUsage{
		Path:        path,
		UsedBytes:   used,
		FreeBytes:   free,
		TotalBytes:  total,
		UsedPercent: usedPercent,
	}, nil
}

func readProcesses() (map[int]processSnapshot, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}

	pageSize := uint64(os.Getpagesize())
	processes := make(map[int]processSnapshot)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		process, err := readProcess(pid, pageSize)
		if err != nil {
			if os.IsNotExist(err) || os.IsPermission(err) {
				continue
			}
			return nil, err
		}
		processes[pid] = process
	}

	return processes, nil
}

func readProcess(pid int, pageSize uint64) (processSnapshot, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return processSnapshot{}, err
	}

	line := strings.TrimSpace(string(data))
	openIndex := strings.Index(line, "(")
	closeIndex := strings.LastIndex(line, ")")
	if openIndex == -1 || closeIndex == -1 || closeIndex <= openIndex {
		return processSnapshot{}, fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}

	name := line[openIndex+1 : closeIndex]
	fields := strings.Fields(strings.TrimSpace(line[closeIndex+1:]))
	if len(fields) < 22 {
		return processSnapshot{}, fmt.Errorf("unexpected /proc/%d/stat field count", pid)
	}

	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return processSnapshot{}, err
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return processSnapshot{}, err
	}
	rssPages, err := strconv.ParseInt(fields[21], 10, 64)
	if err != nil {
		return processSnapshot{}, err
	}
	if rssPages < 0 {
		rssPages = 0
	}

	return processSnapshot{
		name:       name,
		totalTicks: utime + stime,
		rssBytes:   uint64(rssPages) * pageSize,
	}, nil
}

func topProcesses(prevCPU, currentCPU cpuSample, prevProcesses, currentProcesses map[int]processSnapshot, totalMemory uint64, top int) []processMetric {
	if top == 0 {
		return nil
	}

	totalDelta := currentCPU.total - prevCPU.total
	processes := make([]processMetric, 0, len(currentProcesses))
	for pid, current := range currentProcesses {
		prev, ok := prevProcesses[pid]
		if !ok {
			continue
		}
		if current.totalTicks < prev.totalTicks {
			continue
		}

		processDelta := current.totalTicks - prev.totalTicks
		cpuPercent := 0.0
		if totalDelta > 0 {
			cpuPercent = (float64(processDelta) / float64(totalDelta)) * 100
		}

		rssPercent := 0.0
		if totalMemory > 0 {
			rssPercent = (float64(current.rssBytes) / float64(totalMemory)) * 100
		}

		processes = append(processes, processMetric{
			PID:        pid,
			Name:       current.name,
			CPUPercent: cpuPercent,
			RSSBytes:   current.rssBytes,
			RSSPercent: rssPercent,
		})
	}

	sort.Slice(processes, func(i, j int) bool {
		if processes[i].CPUPercent != processes[j].CPUPercent {
			return processes[i].CPUPercent > processes[j].CPUPercent
		}
		if processes[i].RSSBytes != processes[j].RSSBytes {
			return processes[i].RSSBytes > processes[j].RSSBytes
		}
		return processes[i].PID < processes[j].PID
	})

	if len(processes) > top {
		processes = processes[:top]
	}

	return processes
}

func serveWeb(cfg config) {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(dashboardHTML))
	})

	mux.HandleFunc("/api/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(store.all())
	})

	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher.Flush()

		ch := store.subscribe()
		defer store.unsubscribe(ch)

		for {
			select {
			case <-r.Context().Done():
				return
			case point := <-ch:
				data, err := json.Marshal(point)
				if err != nil {
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
		}
	})

	addr := fmt.Sprintf(":%d", cfg.port)
	fmt.Fprintf(os.Stderr, "serving dashboard at http://localhost%s\n", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		exitf("web server: %v\n", err)
	}
}

func readMetricsFile(path string) ([]metricPoint, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var points []metricPoint
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var point metricPoint
		if err := json.Unmarshal(line, &point); err != nil {
			continue
		}
		points = append(points, point)
	}
	return points, scanner.Err()
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>mon</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link href="https://fonts.googleapis.com/css2?family=Anybody:wght@300;400;600&family=IBM+Plex+Mono:wght@300;400;500&display=swap" rel="stylesheet">
<script src="https://cdn.jsdelivr.net/npm/chart.js@4"></script>
<script src="https://cdn.jsdelivr.net/npm/chartjs-adapter-date-fns@3"></script>
<style>
  :root {
    --bg: #08090b;
    --surface: #0f1114;
    --border: #1a1c21;
    --text: #c8cad0;
    --text-dim: #5a5d66;
    --text-muted: #33363d;
    --accent: #d4a54a;
    --cpu: #4a9eed;
    --mem: #a47de8;
    --disk: #d4a54a;
    --teal: #4ac5a8;
    --rose: #e87d7d;
    --radius: 6px;
    --font: 'Anybody', sans-serif;
    --mono: 'IBM Plex Mono', monospace;
  }
  *, *::before, *::after { margin: 0; padding: 0; box-sizing: border-box; }
  html { font-size: 15px; height: 100%; }
  body {
    font-family: var(--font);
    background: var(--bg);
    color: var(--text);
    height: 100%;
    overflow: hidden;
  }
  body::before {
    content: '';
    position: fixed;
    inset: 0;
    background: radial-gradient(ellipse 80% 50% at 50% -10%, rgba(212,165,74,0.04), transparent);
    pointer-events: none;
    z-index: 0;
  }

  .shell {
    position: relative;
    z-index: 1;
    margin: 0 auto;
    padding: 1.5rem 2rem 1rem;
    height: 100%;
    display: flex;
    flex-direction: column;
    overflow: hidden;
  }

  /* Header */
  header {
    display: flex;
    align-items: baseline;
    gap: 0.75rem;
    margin-bottom: 1rem;
    padding-bottom: 0.75rem;
    border-bottom: 1px solid var(--border);
    flex-shrink: 0;
  }
  header h1 {
    font-family: var(--mono);
    font-size: 1.1rem;
    font-weight: 500;
    color: var(--accent);
    letter-spacing: -0.02em;
  }
  header .dot {
    width: 6px; height: 6px;
    border-radius: 50%;
    background: var(--teal);
    display: inline-block;
    animation: pulse 2.5s ease-in-out infinite;
    margin-left: 0.1rem;
  }
  @keyframes pulse { 0%,100%{ opacity:1; } 50%{ opacity:0.3; } }
  header .meta {
    font-family: var(--mono);
    font-size: 0.72rem;
    color: var(--text-muted);
    margin-left: auto;
    letter-spacing: 0.03em;
  }

  /* Stat row */
  .stats {
    display: grid;
    grid-template-columns: repeat(4, 1fr);
    gap: 1px;
    background: var(--border);
    border-radius: var(--radius);
    overflow: hidden;
    margin-bottom: 1rem;
    flex-shrink: 0;
  }
  .stat {
    background: var(--surface);
    padding: 0.9rem 1.2rem;
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
    opacity: 0;
    animation: fadeUp 0.5s ease forwards;
  }
  .stat:nth-child(1) { animation-delay: 0.05s; }
  .stat:nth-child(2) { animation-delay: 0.1s; }
  .stat:nth-child(3) { animation-delay: 0.15s; }
  .stat:nth-child(4) { animation-delay: 0.2s; }
  @keyframes fadeUp {
    from { opacity: 0; transform: translateY(6px); }
    to   { opacity: 1; transform: translateY(0); }
  }
  .stat-label {
    font-family: var(--mono);
    font-size: 0.65rem;
    font-weight: 400;
    color: var(--text-dim);
    text-transform: uppercase;
    letter-spacing: 0.12em;
  }
  .stat-value {
    font-family: var(--mono);
    font-size: 1.5rem;
    font-weight: 300;
    letter-spacing: -0.03em;
    line-height: 1;
    color: #fff;
  }
  .stat-value .unit {
    font-size: 0.65rem;
    font-weight: 400;
    color: var(--text-dim);
    margin-left: 0.2rem;
    letter-spacing: 0.05em;
  }
  .stat-sub {
    font-family: var(--mono);
    font-size: 0.68rem;
    color: var(--text-muted);
  }
  .stat-bar {
    margin-top: 0.4rem;
    height: 2px;
    background: var(--border);
    border-radius: 1px;
    overflow: hidden;
  }
  .stat-bar-fill {
    height: 100%;
    border-radius: 1px;
    transition: width 0.8s cubic-bezier(0.22, 1, 0.36, 1);
  }

  /* Charts grid */
  .charts {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 1px;
    background: var(--border);
    border-radius: var(--radius);
    overflow: hidden;
    margin-bottom: 1rem;
    flex: 1;
    min-height: 120px;
  }
  .chart-cell {
    background: var(--surface);
    padding: 0.8rem 1.2rem 0.6rem;
    opacity: 0;
    animation: fadeUp 0.5s ease forwards;
    animation-delay: 0.25s;
    display: flex;
    flex-direction: column;
    min-height: 0;
  }
  .chart-cell:nth-child(2) { animation-delay: 0.3s; }
  .chart-cell:nth-child(3) { animation-delay: 0.35s; }
  .chart-cell:nth-child(4) { animation-delay: 0.4s; }
  .chart-cell h2 {
    font-family: var(--mono);
    font-size: 0.65rem;
    font-weight: 400;
    color: var(--text-dim);
    text-transform: uppercase;
    letter-spacing: 0.12em;
    margin-bottom: 0.4rem;
    flex-shrink: 0;
  }
  .chart-full {
    grid-column: 1 / -1;
  }
  canvas { width: 100% !important; flex: 1; min-height: 0; }

  /* Process table */
  .table-wrap {
    background: var(--border);
    border-radius: var(--radius);
    overflow: hidden;
    opacity: 0;
    animation: fadeUp 0.5s ease forwards;
    animation-delay: 0.45s;
    flex: 1;
    min-height: 120px;
    display: flex;
    flex-direction: column;
  }
  .table-inner {
    background: var(--surface);
    overflow-y: auto;
    flex: 1;
    min-height: 0;
    scrollbar-width: thin;
    scrollbar-color: #2a2d35 transparent;
  }
  .table-inner::-webkit-scrollbar { width: 6px; }
  .table-inner::-webkit-scrollbar-track { background: transparent; }
  .table-inner::-webkit-scrollbar-thumb { background: #2a2d35; border-radius: 3px; }
  .table-inner::-webkit-scrollbar-thumb:hover { background: #3a3d46; }
  .table-header {
    padding: 0.8rem 1.4rem 0.5rem;
    flex-shrink: 0;
  }
  .table-header h2 {
    font-family: var(--mono);
    font-size: 0.65rem;
    font-weight: 400;
    color: var(--text-dim);
    text-transform: uppercase;
    letter-spacing: 0.12em;
  }
  table {
    width: 100%;
    border-collapse: collapse;
  }
  th, td {
    text-align: left;
    padding: 0.55rem 1.4rem;
    font-family: var(--mono);
    font-size: 0.78rem;
  }
  th {
    font-weight: 400;
    color: var(--text-muted);
    font-size: 0.68rem;
    text-transform: uppercase;
    letter-spacing: 0.08em;
    border-bottom: 1px solid var(--border);
  }
  td {
    color: var(--text);
    border-bottom: 1px solid #12141a;
  }
  tr:last-child td { border-bottom: none; }
  tr:hover td { background: rgba(255,255,255,0.015); }
  td.name { color: #fff; font-weight: 500; }
  td.num { color: var(--text-dim); font-variant-numeric: tabular-nums; }
  td.cpu-val { color: var(--cpu); }
  td.rss-val { color: var(--mem); }

  /* Bar in table */
  .mini-bar {
    display: inline-block;
    width: 48px;
    height: 3px;
    background: var(--border);
    border-radius: 1.5px;
    overflow: hidden;
    vertical-align: middle;
    margin-left: 0.5rem;
  }
  .mini-bar-fill {
    height: 100%;
    border-radius: 1.5px;
    transition: width 0.6s ease;
  }

  /* Footer */
  .foot {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-top: 0.75rem;
    padding-top: 0.5rem;
    border-top: 1px solid var(--border);
    flex-shrink: 0;
  }
  .foot span {
    font-family: var(--mono);
    font-size: 0.65rem;
    color: var(--text-muted);
    letter-spacing: 0.04em;
  }
  .foot .live {
    display: flex;
    align-items: center;
    gap: 0.4rem;
  }
  .foot .live::before {
    content: '';
    width: 5px; height: 5px;
    border-radius: 50%;
    background: var(--teal);
    animation: pulse 2.5s ease-in-out infinite;
  }

  @media (max-width: 700px) {
    .shell { padding: 1rem 0.75rem; }
    .stats { grid-template-columns: 1fr 1fr; }
    .charts { grid-template-columns: 1fr; }
    .chart-full { grid-column: auto; }
    .stat-value { font-size: 1.2rem; }
  }
</style>
</head>
<body>
<div class="shell">

  <header>
    <h1>mon</h1><span class="dot"></span>
    <span class="meta" id="headerTime"></span>
  </header>

  <div class="stats">
    <div class="stat">
      <span class="stat-label">cpu</span>
      <span class="stat-value" id="statCpu">--<span class="unit">%</span></span>
      <div class="stat-bar"><div class="stat-bar-fill" id="barCpu" style="width:0;background:var(--cpu)"></div></div>
    </div>
    <div class="stat">
      <span class="stat-label">memory</span>
      <span class="stat-value" id="statMem">--<span class="unit">%</span></span>
      <span class="stat-sub" id="statMemSub"></span>
      <div class="stat-bar"><div class="stat-bar-fill" id="barMem" style="width:0;background:var(--mem)"></div></div>
    </div>
    <div class="stat">
      <span class="stat-label">disk /</span>
      <span class="stat-value" id="statDisk">--<span class="unit">%</span></span>
      <span class="stat-sub" id="statDiskSub"></span>
      <div class="stat-bar"><div class="stat-bar-fill" id="barDisk" style="width:0;background:var(--disk)"></div></div>
    </div>
    <div class="stat">
      <span class="stat-label">samples</span>
      <span class="stat-value" id="statSamples">--</span>
      <span class="stat-sub" id="statInterval"></span>
    </div>
  </div>

  <div class="charts">
    <div class="chart-cell"><h2>cpu utilisation</h2><canvas id="cpuChart"></canvas></div>
    <div class="chart-cell"><h2>memory utilisation</h2><canvas id="memChart"></canvas></div>
    <div class="chart-cell"><h2>disk usage</h2><canvas id="diskChart"></canvas></div>
    <div class="chart-cell"><h2>memory absolute</h2><canvas id="memAbsChart"></canvas></div>
  </div>

  <div class="table-wrap">
    <div class="table-inner">
      <div class="table-header"><h2>processes</h2></div>
      <table id="procTable">
        <thead><tr>
          <th style="width:60px">pid</th>
          <th>name</th>
          <th style="width:120px">cpu</th>
          <th style="width:100px">rss</th>
          <th style="width:140px">mem</th>
        </tr></thead>
        <tbody></tbody>
      </table>
    </div>
  </div>

  <div class="foot">
    <span class="live">auto-refresh 5 s</span>
    <span id="footSamples"></span>
  </div>

</div>

<script>
Chart.defaults.font.family = "'IBM Plex Mono', monospace";
Chart.defaults.font.size = 10;

function mkChart(id, label, color, isPercent) {
  var ctx = document.getElementById(id);
  return new Chart(ctx, {
    type: 'line',
    data: { datasets: [{
      label: label,
      borderColor: color,
      backgroundColor: function(context) {
        var chart = context.chart;
        var ctx2 = chart.ctx, area = chart.chartArea;
        if (!area) return color + '11';
        var g = ctx2.createLinearGradient(0, area.top, 0, area.bottom);
        g.addColorStop(0, color + '28');
        g.addColorStop(1, color + '02');
        return g;
      },
      fill: true,
      pointRadius: 0,
      borderWidth: 1.5,
      tension: 0.35,
      data: []
    }]},
    options: {
      responsive: true,
      maintainAspectRatio: false,
      animation: false,
      interaction: { mode: 'index', intersect: false },
      scales: {
        x: {
          type: 'time',
          time: { tooltipFormat: 'HH:mm:ss', displayFormats: { second: 'HH:mm:ss', minute: 'HH:mm' } },
          ticks: { color: '#5a5d66', font: { size: 9 }, maxTicksLimit: 6, maxRotation: 0 },
          grid: { color: '#181a1f', drawTicks: false },
          border: { display: false }
        },
        y: {
          min: 0,
          max: isPercent ? 100 : undefined,
          ticks: { color: '#5a5d66', font: { size: 9 }, maxTicksLimit: 5, padding: 6, callback: isPercent ? function(v) { return v + '%'; } : undefined },
          grid: { color: '#181a1f', drawTicks: false },
          border: { display: false }
        }
      },
      plugins: {
        legend: { display: false },
        tooltip: {
          backgroundColor: '#1a1c22',
          titleColor: '#888',
          bodyColor: '#e0e0e0',
          borderColor: '#2a2d35',
          borderWidth: 1,
          padding: 10,
          displayColors: false,
          titleFont: { size: 10 },
          bodyFont: { size: 12 }
        }
      }
    }
  });
}

var cpuChart = mkChart('cpuChart', 'CPU %', '#4a9eed', true);
var memChart = mkChart('memChart', 'Memory %', '#a47de8', true);
var diskChart = mkChart('diskChart', 'Disk %', '#d4a54a', true);

var memAbsChart = new Chart(document.getElementById('memAbsChart'), {
  type: 'line',
  data: { datasets: [
    {
      label: 'Used',
      borderColor: '#a47de8',
      backgroundColor: function(context) {
        var chart = context.chart;
        var ctx2 = chart.ctx, area = chart.chartArea;
        if (!area) return '#a47de811';
        var g = ctx2.createLinearGradient(0, area.top, 0, area.bottom);
        g.addColorStop(0, '#a47de828');
        g.addColorStop(1, '#a47de802');
        return g;
      },
      fill: true, pointRadius: 0, borderWidth: 1.5, tension: 0.35, data: []
    },
    {
      label: 'Total',
      borderColor: '#252830',
      borderDash: [3,3],
      pointRadius: 0, borderWidth: 1, tension: 0.35, fill: false, data: []
    }
  ]},
  options: {
    responsive: true,
    maintainAspectRatio: false,
    animation: false,
    interaction: { mode: 'index', intersect: false },
    scales: {
      x: {
        type: 'time',
        time: { tooltipFormat: 'HH:mm:ss', displayFormats: { second: 'HH:mm:ss', minute: 'HH:mm' } },
        ticks: { color: '#5a5d66', font: { size: 9 }, maxTicksLimit: 6, maxRotation: 0 },
        grid: { color: '#181a1f', drawTicks: false },
        border: { display: false }
      },
      y: {
        min: 0,
        ticks: {
          color: '#5a5d66', font: { size: 9 }, maxTicksLimit: 5, padding: 6,
          callback: function(v) { return (v/1073741824).toFixed(0) + ' G'; }
        },
        grid: { color: '#181a1f', drawTicks: false },
        border: { display: false }
      }
    },
    plugins: {
      legend: { display: true, labels: { color: '#5a5d66', boxWidth: 8, boxHeight: 2, padding: 16, font: { size: 10 } } },
      tooltip: {
        backgroundColor: '#1a1c22',
        titleColor: '#888',
        bodyColor: '#e0e0e0',
        borderColor: '#2a2d35',
        borderWidth: 1,
        padding: 10,
        displayColors: false,
        callbacks: { label: function(c) { return c.dataset.label + ': ' + (c.raw.y/1073741824).toFixed(2) + ' GB'; } }
      }
    }
  }
});

function fmt(bytes) {
  if (bytes >= 1073741824) return (bytes/1073741824).toFixed(1) + ' GB';
  if (bytes >= 1048576) return (bytes/1048576).toFixed(0) + ' MB';
  return (bytes/1024).toFixed(0) + ' KB';
}

function animateValue(el, val, suffix) {
  el.innerHTML = val + '<span class="unit">' + suffix + '</span>';
}

var allData = [];

async function refresh() {
  try {
    var res = await fetch('/api/metrics');
    var data = await res.json();
    if (!data || !data.length) return;
    allData = data;
    updateDashboard();
  } catch(e) { console.error('refresh failed:', e); }
}

function updateDashboard() {
  var data = allData;
  if (!data || !data.length) return;
  var maxPts = 300;
  var view = data.length > maxPts ? data.slice(data.length - maxPts) : data;
  try {
    var last = data[data.length - 1];

    // Stats
    animateValue(document.getElementById('statCpu'), last.cpu_percent.toFixed(1), '%');
    animateValue(document.getElementById('statMem'), last.memory.used_percent.toFixed(1), '%');
    document.getElementById('statMemSub').textContent = fmt(last.memory.used_bytes) + ' / ' + fmt(last.memory.total_bytes);
    document.getElementById('barCpu').style.width = last.cpu_percent.toFixed(1) + '%';
    document.getElementById('barMem').style.width = last.memory.used_percent.toFixed(1) + '%';

    if (last.disks && last.disks.length) {
      var d = last.disks[0];
      document.getElementById('statDisk').innerHTML = d.used_percent.toFixed(1) + '<span class="unit">%</span>';
      document.getElementById('statDiskSub').textContent = fmt(d.used_bytes) + ' / ' + fmt(d.total_bytes);
      document.getElementById('barDisk').style.width = d.used_percent.toFixed(1) + '%';
      document.querySelector('.stat:nth-child(3) .stat-label').textContent = 'disk ' + d.path;
    }

    document.getElementById('statSamples').innerHTML = data.length.toString();
    if (data.length >= 2) {
      var dt = new Date(data[1].timestamp) - new Date(data[0].timestamp);
      document.getElementById('statInterval').textContent = (dt/1000).toFixed(0) + 's interval';
    }

    var ts = new Date(last.timestamp);
    document.getElementById('headerTime').textContent = ts.toLocaleTimeString();

    // Charts
    cpuChart.data.datasets[0].data = view.map(function(p) { return { x: p.timestamp, y: p.cpu_percent }; });
    memChart.data.datasets[0].data = view.map(function(p) { return { x: p.timestamp, y: p.memory.used_percent }; });
    memAbsChart.data.datasets[0].data = view.map(function(p) { return { x: p.timestamp, y: p.memory.used_bytes }; });
    memAbsChart.data.datasets[1].data = view.map(function(p) { return { x: p.timestamp, y: p.memory.total_bytes }; });

    if (view[0].disks && view[0].disks.length) {
      var colors = ['#d4a54a','#e87d7d','#4ac5a8','#4a9eed'];
      var alphas = ['#d4a54a','#e87d7d','#4ac5a8','#4a9eed'];
      diskChart.data.datasets = view[0].disks.map(function(_, i) {
        var c = colors[i % colors.length];
        return {
          label: view[0].disks[i].path,
          borderColor: c,
          backgroundColor: function(context) {
            var chart = context.chart;
            var ctx2 = chart.ctx, area = chart.chartArea;
            if (!area) return c + '11';
            var g = ctx2.createLinearGradient(0, area.top, 0, area.bottom);
            g.addColorStop(0, c + '28');
            g.addColorStop(1, c + '02');
            return g;
          },
          fill: true, pointRadius: 0, borderWidth: 1.5, tension: 0.35,
          data: view.map(function(p) { return { x: p.timestamp, y: p.disks[i] ? p.disks[i].used_percent : null }; })
        };
      });
    }

    [cpuChart, memChart, diskChart, memAbsChart].forEach(function(c) { c.update(); });

    // Process table
    var tbody = document.querySelector('#procTable tbody');
    tbody.innerHTML = '';
    if (last.processes) {
      var maxCpu = Math.max.apply(null, last.processes.map(function(p) { return p.cpu_percent; })) || 1;
      last.processes.forEach(function(p) {
        var row = document.createElement('tr');
        var cpuW = (p.cpu_percent / maxCpu * 100).toFixed(0);
        var memW = (p.rss_percent * 10).toFixed(0);
        if (parseInt(memW) > 100) memW = '100';
        row.innerHTML =
          '<td class="num">' + p.pid + '</td>' +
          '<td class="name">' + p.name + '</td>' +
          '<td class="cpu-val">' + p.cpu_percent.toFixed(2) + '%' +
            '<span class="mini-bar"><span class="mini-bar-fill" style="width:' + cpuW + '%;background:var(--cpu)"></span></span></td>' +
          '<td class="num">' + fmt(p.rss_bytes) + '</td>' +
          '<td class="rss-val">' + p.rss_percent.toFixed(2) + '%' +
            '<span class="mini-bar"><span class="mini-bar-fill" style="width:' + memW + '%;background:var(--mem)"></span></span></td>';
        tbody.appendChild(row);
      });
    }

    document.getElementById('footSamples').textContent = data.length + ' samples';

  } catch(e) { console.error('update failed:', e); }
}

refresh();

var evtSource = new EventSource('/api/events');
evtSource.onmessage = function(e) {
  try {
    var point = JSON.parse(e.data);
    allData.push(point);
    updateDashboard();
  } catch(err) { console.error('sse parse:', err); }
};
evtSource.onerror = function() {
  // fallback to polling if SSE drops
  setTimeout(refresh, 3000);
};
</script>
</body>
</html>
`

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
	os.Exit(1)
}
