// dehash client example — demonstrates all three endpoints.
//
// Usage:
//
//	go run examples/client/main.go --file hashes_amcache.txt
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultAddr = "http://localhost:8080"

func main() {
	addr     := flag.String("addr", defaultAddr, "dehash server address")
	hashFile := flag.String("file", "", "Text file with hashes (one per line) — uses /file endpoint")
	single   := flag.String("hash", "", "Single hash to look up — uses /lookup endpoint")
	details  := flag.Bool("details", false, "Request full file details")
	flag.Parse()

	client := &http.Client{Timeout: 30 * time.Second}

	switch {
	case *single != "":
		lookupSingle(client, *addr, *single, *details)

	case *hashFile != "":
		lookupFile(client, *addr, *hashFile, *details)

	default:
		demoAll(client, *addr)
	}
}

// ── single hash lookup ────────────────────────────────────────────────────────

func lookupSingle(client *http.Client, addr, hash string, details bool) {
	body, _ := json.Marshal(map[string]any{"hash": hash, "details": details})

	resp, err := client.Post(addr+"/lookup", "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "request failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)

	status := result["status"]
	fmt.Printf("Hash   : %s\n", hash)
	fmt.Printf("Status : %s\n", status)

	if status == "found" && details {
		if d, ok := result["details"].(map[string]any); ok {
			fmt.Printf("File   : %s  (%v bytes)\n", d["file_name"], d["file_size"])
			fmt.Printf("App    : %s %s\n", d["app_name"], d["app_version"])
			fmt.Printf("Vendor : %s\n", d["manufacturer"])
			fmt.Printf("OS     : %s %s\n", d["os_name"], d["os_version"])
			fmt.Printf("Type   : %s\n", d["app_type"])
		}
	}
}

// ── file-based lookup (large hash lists) ─────────────────────────────────────

func lookupFile(client *http.Client, addr, path string, details bool) {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open file: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	// Count lines for reporting.
	total := countLines(path)
	fmt.Printf("Sending %d hashes from %s...\n", total, path)

	// Rewind and POST the raw file content to /file.
	// The server reads one hash per line — no JSON encoding needed.
	f.Seek(0, 0)
	url := fmt.Sprintf("%s/file?details=%v", addr, details)

	start := time.Now()
	resp, err := client.Post(url, "text/plain", f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "request failed: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	var result struct {
		NotFound   []string        `json:"not_found"`
		Found      map[string]any  `json:"found"`
		FoundCount int             `json:"found_count"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	elapsed := time.Since(start)

	fmt.Printf("Done in %s\n\n", elapsed.Round(time.Millisecond))
	fmt.Printf("  Found (legitimate)     : %d\n", result.FoundCount)
	fmt.Printf("  Not found (suspicious) : %d\n", len(result.NotFound))

	if len(result.NotFound) > 0 {
		fmt.Printf("\nSuspicious hashes:\n")
		for _, h := range result.NotFound {
			fmt.Printf("  %s\n", h)
		}
	}
}

// ── full demo (no flags) ──────────────────────────────────────────────────────

func demoAll(client *http.Client, addr string) {
	fmt.Println("dehash client demo")
	fmt.Println("══════════════════")
	fmt.Println()

	// 1. Health check
	fmt.Println("1. Health check")
	resp, _ := client.Get(addr + "/health")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Printf("   %s\n\n", string(body))

	// 2. Single lookup — not found
	fmt.Println("2. Single lookup (not found)")
	lookupSingle(client, addr, "0000000000000000000000000000000000000000", false)
	fmt.Println()

	// 3. Bulk lookup
	fmt.Println("3. Bulk lookup")
	hashes := []string{
		"0000000000000000000000000000000000000000",
		"1111111111111111111111111111111111111111",
		"2222222222222222222222222222222222222222",
	}
	b, _ := json.Marshal(map[string]any{"hashes": hashes, "details": false})
	resp, _ = client.Post(addr+"/bulk", "application/json", bytes.NewReader(b))
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Printf("   Response: %s\n\n", string(body))

	// 4. File-based lookup using inline content
	fmt.Println("4. File-based lookup (/file endpoint)")
	fileBody := strings.Join(hashes, "\n")
	resp, _ = client.Post(addr+"/file", "text/plain", strings.NewReader(fileBody))
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Printf("   Response: %s\n\n", string(body))
}

// ── helpers ───────────────────────────────────────────────────────────────────

func countLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	return n
}
