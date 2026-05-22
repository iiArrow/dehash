package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
)

func main() {
	addr     := flag.String("addr", "http://localhost:8080", "Server address")
	hashFile := flag.String("hashes", "hashes_amcache.txt", "Hash file (one per line)")
	workers  := flag.Int("workers", 500, "Goroutine count — crank up to find ceiling")
	duration := flag.Duration("duration", 30*time.Second, "How long to run")
	flag.Parse()

	hashes := loadHashes(*hashFile)
	if len(hashes) == 0 {
		fmt.Fprintln(os.Stderr, "no hashes loaded")
		os.Exit(1)
	}

	client := &fasthttp.Client{
		MaxConnsPerHost:     *workers + 100,
		ReadTimeout:         10 * time.Second,
		WriteTimeout:        10 * time.Second,
		MaxIdleConnDuration: 60 * time.Second,
	}

	url := *addr + "/lookup"

	var (
		totalReqs   atomic.Int64
		totalErrors atomic.Int64
	)

	stop := make(chan struct{})

	// Spawn all workers — no throttle, no rate limit.
	for i := 0; i < *workers; i++ {
		go func(seed uint64) {
			rng := rand.New(rand.NewPCG(seed, seed^0xdeadbeef))

			req  := fasthttp.AcquireRequest()
			resp := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseRequest(req)
			defer fasthttp.ReleaseResponse(resp)

			req.SetRequestURI(url)
			req.Header.SetMethod("POST")
			req.Header.SetContentType("application/json")

			for {
				select {
				case <-stop:
					return
				default:
				}

				hash := hashes[rng.IntN(len(hashes))]
				body, _ := json.Marshal(map[string]any{"hash": hash, "details": false})
				req.SetBody(body)
				resp.Reset()

				if err := client.Do(req, resp); err != nil {
					totalErrors.Add(1)
				} else {
					totalReqs.Add(1)
				}
			}
		}(uint64(time.Now().UnixNano()) + uint64(i))
	}

	// ── live display ──────────────────────────────────────────────────────────
	fmt.Println()
	fmt.Println("  dehash — BLITZ TEST")
	fmt.Printf("  %d goroutines  |  %s  |  no limits\n", *workers, *duration)
	fmt.Println("  ─────────────────────────────────────────────────────────────")
	fmt.Println()

	ticker  := time.NewTicker(time.Second)
	start   := time.Now()
	end     := start.Add(*duration)

	var (
		prevTotal int64
		peakRPS   int64
		history   []int64 // per-second samples for sparkline
	)

	for t := range ticker.C {
		current  := totalReqs.Load()
		errors   := totalErrors.Load()
		elapsed  := t.Sub(start)
		rps      := current - prevTotal
		prevTotal = current
		history   = append(history, rps)

		if rps > peakRPS {
			peakRPS = rps
		}

		remaining := end.Sub(t)
		if remaining < 0 {
			remaining = 0
		}

		bar := progressBar(elapsed.Seconds(), duration.Seconds(), 36)

		fmt.Printf("\033[A\033[A\033[A") // move up 3 lines
		fmt.Printf("  %s  %2.0fs / %.0fs\n", bar, elapsed.Seconds(), duration.Seconds())
		fmt.Printf("  Current : %s req/s    Peak : %s req/s\n",
			pad(formatNum(rps), 10), pad(formatNum(peakRPS), 10))
		fmt.Printf("  Total   : %s requests   Errors : %d\n",
			pad(formatNum(current), 12), errors)

		if t.After(end) || t.Equal(end) {
			break
		}
	}

	ticker.Stop()
	close(stop)

	// Allow in-flight requests to finish.
	time.Sleep(200 * time.Millisecond)

	final   := totalReqs.Load()
	errors  := totalErrors.Load()
	elapsed := time.Since(start)
	avgRPS  := float64(final) / elapsed.Seconds()

	fmt.Println()
	fmt.Println("  ═════════════════════════════════════════════════════════════")
	fmt.Println("  RESULTS")
	fmt.Println("  ─────────────────────────────────────────────────────────────")
	fmt.Printf("  Duration      : %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("  Total requests: %s\n", formatNum(final))
	fmt.Printf("  Avg req/s     : %s\n", formatNum(int64(avgRPS)))
	fmt.Printf("  Peak req/s    : %s\n", formatNum(peakRPS))
	fmt.Printf("  Errors        : %d  (%.2f%%)\n", errors, errorRate(errors, final))
	fmt.Println("  ─────────────────────────────────────────────────────────────")
	fmt.Printf("  Sparkline     : %s\n", sparkline(history))
	fmt.Println()
}

// ── helpers ───────────────────────────────────────────────────────────────────

func loadHashes(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if h := strings.TrimSpace(sc.Text()); h != "" {
			out = append(out, h)
		}
	}
	return out
}

func progressBar(elapsed, total float64, width int) string {
	filled := int(elapsed / total * float64(width))
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

func formatNum(n int64) string {
	s := fmt.Sprintf("%d", n)
	// Insert commas every 3 digits from the right.
	out := []byte(s)
	result := make([]byte, 0, len(out)+len(out)/3)
	for i, c := range out {
		if i > 0 && (len(out)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, c)
	}
	return string(result)
}

func pad(s string, n int) string {
	for len(s) < n {
		s += " "
	}
	return s
}

func errorRate(errors, total int64) float64 {
	if total+errors == 0 {
		return 0
	}
	return float64(errors) / float64(total+errors) * 100
}

// sparkline renders a mini bar chart of per-second req/s history.
func sparkline(samples []int64) string {
	if len(samples) == 0 {
		return ""
	}
	bars := []rune("▁▂▃▄▅▆▇█")
	var max int64
	for _, v := range samples {
		if v > max {
			max = v
		}
	}
	if max == 0 {
		return strings.Repeat("▁", len(samples))
	}
	out := make([]rune, len(samples))
	for i, v := range samples {
		idx := int(float64(v) / float64(max) * float64(len(bars)-1))
		out[i] = bars[idx]
	}
	return string(out)
}
