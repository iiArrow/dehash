package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
)

// ── config ────────────────────────────────────────────────────────────────────

type config struct {
	addr        string
	hashFile    string
	concurrency int
	bulkConc    int
	batchSize   int
	duration    time.Duration
	warmup      time.Duration
}

// ── entry point ───────────────────────────────────────────────────────────────

func main() {
	cfg := config{}
	flag.StringVar(&cfg.addr, "addr", "http://localhost:8080", "Server base URL")
	flag.StringVar(&cfg.hashFile, "hashes", "hashes_amcache.txt", "Hash file (one per line)")
	flag.IntVar(&cfg.concurrency, "concurrency", 50, "Workers for single-hash scenarios")
	flag.IntVar(&cfg.bulkConc, "bulk-concurrency", 10, "Workers for bulk scenarios")
	flag.IntVar(&cfg.batchSize, "batch", 1000, "Hashes per bulk request")
	flag.DurationVar(&cfg.duration, "duration", 10*time.Second, "Measurement window per scenario")
	flag.DurationVar(&cfg.warmup, "warmup", 2*time.Second, "Warmup period (requests sent but not counted)")
	flag.Parse()

	hashes := loadHashes(cfg.hashFile)
	if len(hashes) == 0 {
		fmt.Fprintf(os.Stderr, "no hashes loaded from %s\n", cfg.hashFile)
		os.Exit(1)
	}

	client := &fasthttp.Client{
		MaxConnsPerHost:     cfg.concurrency + cfg.bulkConc + 20,
		ReadTimeout:         30 * time.Second,
		WriteTimeout:        30 * time.Second,
		MaxIdleConnDuration: 60 * time.Second,
	}

	printHeader(cfg, len(hashes))

	scenarios := []scenario{
		{
			label:   "Single  details=false",
			details: false,
			run: func() []int64 {
				return runSingle(client, cfg, hashes, false)
			},
		},
		{
			label:   "Single  details=true ",
			details: true,
			run: func() []int64 {
				return runSingle(client, cfg, hashes, true)
			},
		},
		{
			label:   fmt.Sprintf("Bulk %-5d details=false", cfg.batchSize),
			details: false,
			isBulk:  true,
			run: func() []int64 {
				return runBulk(client, cfg, hashes, false)
			},
		},
		{
			label:   fmt.Sprintf("Bulk %-5d details=true ", cfg.batchSize),
			details: true,
			isBulk:  true,
			run: func() []int64 {
				return runBulk(client, cfg, hashes, true)
			},
		},
	}

	var summaries []summary
	for i, s := range scenarios {
		if i > 0 {
			// Brief pause between scenarios so connections drain.
			time.Sleep(500 * time.Millisecond)
		}
		sum := runScenario(s, cfg, client)
		summaries = append(summaries, sum)
	}

	printSummaryTable(summaries, cfg.batchSize)
}

// ── scenario runner ───────────────────────────────────────────────────────────

type scenario struct {
	label   string
	details bool
	isBulk  bool
	run     func() []int64 // returns latency samples in microseconds
}

type summary struct {
	label                    string
	isBulk                   bool
	total, errors            int64
	reqPerSec, hashesPerSec  float64
	p50, p95, p99            time.Duration
}

func runScenario(s scenario, cfg config, _ *fasthttp.Client) summary {
	fmt.Printf("  ┌─ %s\n", s.label)
	fmt.Printf("  │  running warmup (%s)...\n", cfg.warmup)

	latencies := s.run() // warmup is handled inside

	dur := cfg.duration.Seconds()
	total := int64(len(latencies))
	reqPerSec := float64(total) / dur

	hashesPerSec := 0.0
	if s.isBulk {
		hashesPerSec = reqPerSec * float64(cfg.batchSize)
	}

	p50, p95, p99 := percentiles(latencies)

	fmt.Printf("  │  Requests    : %d  |  %.0f req/s\n", total, reqPerSec)
	if s.isBulk {
		fmt.Printf("  │  Hashes/s    : %.0f\n", hashesPerSec)
	}
	fmt.Printf("  │  Latency p50 : %s\n", p50.Round(time.Microsecond))
	fmt.Printf("  │  Latency p95 : %s\n", p95.Round(time.Microsecond))
	fmt.Printf("  │  Latency p99 : %s\n", p99.Round(time.Microsecond))
	fmt.Printf("  └─────────────────────────────────────────\n\n")

	return summary{
		label:       s.label,
		isBulk:      s.isBulk,
		total:       total,
		reqPerSec:   reqPerSec,
		hashesPerSec: hashesPerSec,
		p50:         p50,
		p95:         p95,
		p99:         p99,
	}
}

// ── single hash test ──────────────────────────────────────────────────────────

func runSingle(client *fasthttp.Client, cfg config, hashes []string, details bool) []int64 {
	url := cfg.addr + "/lookup"
	deadline := time.Now().Add(cfg.warmup + cfg.duration)
	warmupEnd := time.Now().Add(cfg.warmup)

	perWorker := make([][]int64, cfg.concurrency)
	var wg sync.WaitGroup

	for i := 0; i < cfg.concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			local := make([]int64, 0, 4096)
			rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()+int64(idx)), 0))

			req := fasthttp.AcquireRequest()
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseRequest(req)
			defer fasthttp.ReleaseResponse(resp)

			req.SetRequestURI(url)
			req.Header.SetMethod("POST")
			req.Header.SetContentType("application/json")

			for time.Now().Before(deadline) {
				hash := hashes[rng.IntN(len(hashes))]
				body := buildLookupBody(hash, details)
				req.SetBody(body)
				resp.Reset()

				t0 := time.Now()
				err := client.Do(req, resp)
				us := time.Since(t0).Microseconds()

				if time.Now().After(warmupEnd) && err == nil {
					local = append(local, us)
				}
			}
			perWorker[idx] = local
		}(i)
	}
	wg.Wait()
	return merge(perWorker)
}

// ── bulk test ─────────────────────────────────────────────────────────────────

func runBulk(client *fasthttp.Client, cfg config, hashes []string, details bool) []int64 {
	url := cfg.addr + "/bulk"
	deadline := time.Now().Add(cfg.warmup + cfg.duration)
	warmupEnd := time.Now().Add(cfg.warmup)

	perWorker := make([][]int64, cfg.bulkConc)
	var wg sync.WaitGroup

	for i := 0; i < cfg.bulkConc; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			local := make([]int64, 0, 1024)
			rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()+int64(idx)), 0))

			req := fasthttp.AcquireRequest()
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseRequest(req)
			defer fasthttp.ReleaseResponse(resp)

			req.SetRequestURI(url)
			req.Header.SetMethod("POST")
			req.Header.SetContentType("application/json")

			for time.Now().Before(deadline) {
				batch := pickRandom(hashes, cfg.batchSize, rng)
				body := buildBulkBody(batch, details)
				req.SetBody(body)
				resp.Reset()

				t0 := time.Now()
				err := client.Do(req, resp)
				us := time.Since(t0).Microseconds()

				if time.Now().After(warmupEnd) && err == nil {
					local = append(local, us)
				}
			}
			perWorker[idx] = local
		}(i)
	}
	wg.Wait()
	return merge(perWorker)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func buildLookupBody(hash string, details bool) []byte {
	b, _ := json.Marshal(map[string]any{"hash": hash, "details": details})
	return b
}

func buildBulkBody(hashes []string, details bool) []byte {
	b, _ := json.Marshal(map[string]any{"hashes": hashes, "details": details})
	return b
}

func pickRandom(hashes []string, n int, rng *rand.Rand) []string {
	if n >= len(hashes) {
		return hashes
	}
	out := make([]string, n)
	for i := range out {
		out[i] = hashes[rng.IntN(len(hashes))]
	}
	return out
}

func loadHashes(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var hashes []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		h := strings.TrimSpace(sc.Text())
		if h != "" {
			hashes = append(hashes, h)
		}
	}
	return hashes
}

func merge(slices [][]int64) []int64 {
	total := 0
	for _, s := range slices {
		total += len(s)
	}
	out := make([]int64, 0, total)
	for _, s := range slices {
		out = append(out, s...)
	}
	return out
}

func percentiles(latencies []int64) (p50, p95, p99 time.Duration) {
	if len(latencies) == 0 {
		return 0, 0, 0
	}
	sorted := make([]int64, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	idx := func(pct float64) int64 {
		i := int(float64(len(sorted)-1) * pct / 100.0)
		return sorted[i]
	}
	return time.Duration(idx(50)) * time.Microsecond,
		time.Duration(idx(95)) * time.Microsecond,
		time.Duration(idx(99)) * time.Microsecond
}

// ── output ────────────────────────────────────────────────────────────────────

func printHeader(cfg config, hashCount int) {
	fmt.Println()
	fmt.Println("  dehash — stress test")
	fmt.Println("  ══════════════════════════════════════════════════")
	fmt.Printf("  Server       : %s\n", cfg.addr)
	fmt.Printf("  Hash file    : %s  (%d hashes)\n", cfg.hashFile, hashCount)
	fmt.Printf("  Duration     : %s measurement + %s warmup per scenario\n", cfg.duration, cfg.warmup)
	fmt.Printf("  Single conc  : %d workers\n", cfg.concurrency)
	fmt.Printf("  Bulk conc    : %d workers × %d hashes/req\n", cfg.bulkConc, cfg.batchSize)
	fmt.Println()
}

func printSummaryTable(summaries []summary, batchSize int) {
	// suppress unused warning
	_ = batchSize

	fmt.Println("  ══ Summary ════════════════════════════════════════════════════════════")
	fmt.Printf("  %-30s  %10s  %12s  %8s  %8s  %8s\n",
		"Scenario", "req/s", "hashes/s", "p50", "p95", "p99")
	fmt.Println("  " + strings.Repeat("─", 80))

	for _, s := range summaries {
		hashRate := "—"
		if s.isBulk {
			hashRate = fmt.Sprintf("%12.0f", s.hashesPerSec)
		} else {
			hashRate = fmt.Sprintf("%12.0f", s.reqPerSec)
		}
		fmt.Printf("  %-30s  %10.0f  %s  %8s  %8s  %8s\n",
			s.label,
			s.reqPerSec,
			hashRate,
			s.p50.Round(time.Microsecond),
			s.p95.Round(time.Microsecond),
			s.p99.Round(time.Microsecond),
		)
	}
	fmt.Println("  " + strings.Repeat("═", 82))
	fmt.Println()

}
