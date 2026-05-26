# dehash

Fast NSRL hash lookup service for Blue Teamers! Check file hashes against the [NIST National Software Reference Library (NSRL)](https://www.nist.gov/itl/ssd/software-quality-group/national-software-reference-library-nsrl) to identify known-good files and surface suspicious ones.

---

## What it does

Given a set of file hashes collected from a machine (e.g. from Windows Amcache, Prefetch, or any forensic artifact), dehash tells you which ones are **not** in the NSRL — meaning they are unknown/suspicious and worth investigating.

```
your hashes  ──►  dehash  ──►  not_found  =  suspicious / not legitimate
                          ──►  found      =  known-good (in NSRL)
```

Supports **SHA256**, **SHA1**, **MD5**, and **CRC32** — auto-detected by hash length. Mixed types in the same request are fine.

---

## How it works

```
┌─────────────────────────────────────────────────────────────────┐
│  ETL (one-time, run after each NSRL release)                    │
│                                                                 │
│  NSRL SQLite DB  ──►  Go ETL pipeline  ──►  Pebble (embedded)  │
│           )              two-phase:             lookup store      │
│                         1. load package map   ~40–60 GB         │
│                         2. stream METADATA                      │
└─────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────┐
│  API Server (always running)                                    │
│                                                                 │
│  POST /lookup  ──►  single hash  ──►  found / not_found        │
│  POST /bulk    ──►  JSON array   ──►  not_found list            │
│  POST /file    ──►  plain text   ──►  not_found list            │
│  GET  /health                                                   │
└─────────────────────────────────────────────────────────────────┘
```

Lookups hit [Pebble](https://github.com/cockroachdb/pebble) (embedded LSM-tree with Bloom filters). No external database, no network round-trips.

---

## Requirements

| Requirement | Notes |
|---|---|
| Go 1.22+ | [https://go.dev/dl](https://go.dev/dl) |
| sqlite3 CLI | Only needed to import `.sql` text dumps — not needed at runtime |
| NSRL RDS | Download from [https://www.nist.gov/itl/ssd/software-quality-group/national-software-reference-library-nsrl/nsrl-download/current-rds](https://www.nist.gov/itl/ssd/software-quality-group/national-software-reference-library-nsrl/nsrl-download/current-rds) |

Install sqlite3:
```bash
sudo pacman -S sqlite    # Arch / CachyOS
sudo apt install sqlite3 # Debian / Ubuntu
brew install sqlite      # macOS
```

---

## Quick Start

```bash
git clone <repo-url>
cd dehash
./start.sh
```

The interactive script handles everything: building, importing, and starting the server.

---

## Manual Setup

### Step 1 — Build

```bash
make build
# Produces: bin/etl  bin/server  bin/stress  bin/blitz
```

### Step 2 — Import the NSRL database (ETL)

The NSRL ships as a SQLite database. If you have a **binary `.db` file** (the full modern set), skip directly to the ETL step. If you have a **SQL text dump** (like the delta files), convert it first.

**Convert SQL text dump → SQLite binary:**
```bash
sqlite3 data/nsrl_import.db < RDS_2026.03.1_modern.schema.sql
sqlite3 data/nsrl_import.db < RDS_2026.03.1_modern_delta.sql
```

**Run the ETL:**
```bash
./bin/etl --db data/nsrl_import.db --out data/nsrl.pebble
```

Options:
```
--db       Path to NSRL SQLite .db file  (required)
--out      Output path for Pebble DB     (default: data/nsrl.pebble)
--workers  Serialization worker count    (default: number of CPUs)
```

The ETL runs in two phases:
1. Loads all package metadata (app name, manufacturer, OS, etc.) into a Go map — one-time cost, avoids per-row SQL joins on the hot path.
2. Streams METADATA rows through a parallel worker pipeline into Pebble.

Expected time:
- Delta file: minutes to ~1 hour
- Full 142 GB modern set: ~30–60 minutes on NVMe SSD

### Step 3 — Start the server

```bash
./bin/server --db data/nsrl.pebble --addr :8080
```

Options:
```
--db     Path to Pebble DB directory  (default: data/nsrl.pebble)
--addr   Listen address               (default: :8080)
```

---

## API Reference

### `POST /lookup` — single hash

**Request:**
```json
{
  "hash": "34b2d30ced220c984e53868c46cc638b690617e4",
  "details": false
}
```

**Response (not found):**
```json
{ "status": "not_found" }
```

**Response (found, details=false):**
```json
{ "status": "found" }
```

**Response (found, details=true):**
```json
{
  "status": "found",
  "details": {
    "sha256":       "ABC123...",
    "sha1":         "34B2D3...",
    "md5":          "...",
    "crc32":        "...",
    "file_name":    "chrome.exe",
    "file_size":    12345,
    "path":         "Google Chrome/",
    "app_name":     "Google Chrome",
    "app_version":  "118.0.5993.71",
    "app_type":     "Web Browser",
    "manufacturer": "Google",
    "os_name":      "Windows 10",
    "os_version":   "10.0",
    "language":     "English"
  }
}
```

---

### `POST /bulk` — JSON array of hashes

Best for up to ~10k hashes. For larger lists use `/file`.

**Request:**
```json
{
  "hashes": ["hash1", "hash2", "hash3"],
  "details": false
}
```

**Response:**
```json
{
  "not_found":   ["hash2"],
  "found_count": 2
}
```

With `"details": true`, a `"found"` map is also returned:
```json
{
  "not_found":   ["hash2"],
  "found_count": 2,
  "found": {
    "hash1": { ...details... },
    "hash3": { ...details... }
  }
}
```

---

### `POST /file` — plain text file (large hash lists)

Designed for files with ~100k+ hashes. Send the file content directly as plain text — one hash per line, no JSON encoding.

**Request body:** plain text, `Content-Type: text/plain`
**Query params:** `?details=true|false`

```bash
curl -X POST "http://localhost:8080/file?details=false" \
     -H "Content-Type: text/plain" \
     --data-binary @hashes_amcache.txt
```

**Response:** same format as `/bulk`

```json
{
  "not_found":   ["hash2", "hash5"],
  "found_count": 99998
}
```

---

### `GET /health`

```json
{ "status": "ok" }
```

---

## Client Examples

### curl

```bash
# Single hash
curl -s -X POST http://localhost:8080/lookup \
  -H "Content-Type: application/json" \
  -d '{"hash":"34b2d30ced220c984e53868c46cc638b690617e4","details":true}'

# Bulk (JSON array)
curl -s -X POST http://localhost:8080/bulk \
  -H "Content-Type: application/json" \
  -d '{"hashes":["hash1","hash2","hash3"],"details":false}'

# Large file — pipe directly, no JSON encoding needed
curl -s -X POST "http://localhost:8080/file?details=false" \
  -H "Content-Type: text/plain" \
  --data-binary @hashes_amcache.txt
```

---

### Python

```python
import requests

BASE = "http://localhost:8080"

# ── single hash ───────────────────────────────────────────────────
resp = requests.post(f"{BASE}/lookup", json={
    "hash": "34b2d30ced220c984e53868c46cc638b690617e4",
    "details": True,
})
data = resp.json()

if data["status"] == "found":
    d = data["details"]
    print(f"Known good: {d['file_name']} — {d['app_name']} {d['app_version']}")
else:
    print("Not found — suspicious")

# ── bulk (JSON array) ─────────────────────────────────────────────
hashes = ["hash1", "hash2", "hash3"]
resp = requests.post(f"{BASE}/bulk", json={"hashes": hashes, "details": False})
result = resp.json()
print(f"Suspicious: {result['not_found']}")

# ── large file ────────────────────────────────────────────────────
with open("hashes_amcache.txt", "rb") as f:
    resp = requests.post(
        f"{BASE}/file",
        params={"details": "false"},
        data=f,
        headers={"Content-Type": "text/plain"},
    )

result = resp.json()
print(f"Total suspicious hashes: {len(result['not_found'])}")
for h in result["not_found"]:
    print(f"  SUSPICIOUS: {h}")
```

---

### Go

See [`examples/client/main.go`](examples/client/main.go) for a full working example.

```bash
# Run the demo
go run examples/client/main.go

# Look up a single hash
go run examples/client/main.go --hash 34b2d30ced220c984e53868c46cc638b690617e4 --details

# Check a whole file
go run examples/client/main.go --file hashes.txt
```

---

## Terminal UI (TUI)

An interactive terminal client built with [Bubble Tea](https://github.com/charmbracelet/bubbletea). Lets you query the server without writing any code — supports all three request types with a live results viewer.

```bash
# Build
make build

# Run
./bin/tui
```

**Features:**
- Configure server URL and port from the UI
- Switch between `lookup`, `bulk`, and `file` modes
- Built-in file picker for selecting hash list files
- Toggle `details` on/off
- Syntax-highlighted JSON output with a scrollable viewport
- Saves last response to a file

**Keyboard shortcuts:**

| Key | Action |
|---|---|
| `Tab` / `↑↓` | Navigate fields |
| `←` `→` | Change radio options (request type, details) |
| `Space` | Toggle / open file picker |
| `Enter` | Send request |
| `Ctrl+C` | Quit |

---

## Performance Testing

### Structured stress test (4 scenarios with percentile latency)

```bash
./bin/stress

# Options
./bin/stress --concurrency 100 --batch 5000 --duration 30s
```

Runs four scenarios back-to-back:
- Single hash, `details=false`
- Single hash, `details=true`
- Bulk N hashes, `details=false`
- Bulk N hashes, `details=true`

### Blitz test (max throughput, no limits)

```bash
./bin/blitz

# Options
./bin/blitz --workers 500 --duration 30s
```

Spawns 500 goroutines hammering `/lookup` with no throttle for 30 seconds. Shows live req/s, peak req/s, and a sparkline. Increase `--workers` until throughput plateaus — that's your ceiling.

---

## Project Structure

```
dehash/
├── cmd/
│   ├── etl/        — NSRL SQLite → Pebble import pipeline
│   ├── server/     — HTTP API server
│   ├── tui/        — Bubble Tea interactive terminal client
│   ├── stress/     — structured stress test (4 scenarios + percentiles)
│   └── blitz/      — max-throughput blitz test (live counter + sparkline)
├── internal/
│   ├── model/      — FileDetails struct (JSON + MessagePack tags)
│   ├── store/      — Pebble wrapper, hash-type detection, key encoding
│   └── api/        — fasthttp handlers (/lookup, /bulk, /file)
├── examples/
│   └── client/     — Go client usage example
├── start.sh        — interactive setup script
└── Makefile
```

---

## Hash Type Detection

Hash type is auto-detected by the byte length of the decoded hex string:

| Hex chars | Bytes | Type   |
|-----------|-------|--------|
| 8         | 4     | CRC32  |
| 32        | 16    | MD5    |
| 40        | 20    | SHA1   |
| 64        | 32    | SHA256 |

Input is normalised (trimmed, lowercased) before lookup. Mixed types in the same bulk/file request are fully supported.

> **CRC32 note:** 4-byte keyspace has a non-trivial collision rate. CRC32 hits should be treated as informational — corroborate with SHA1 or SHA256.

---

## NSRL Updates

NIST publishes NSRL updates quarterly. To update:

```bash
# 1. Download the new release from NIST
# 2. Import it (apply schema + data SQL, or use the binary .db directly)
sqlite3 data/nsrl_new.db < new_schema.sql
sqlite3 data/nsrl_new.db < new_data.sql

# 3. Run ETL into a new Pebble path
./bin/etl --db data/nsrl_new.db --out data/nsrl_new.pebble

# 4. Restart the server pointing at the new DB (zero downtime: old server
#    keeps running until you send SIGTERM)
./bin/server --db data/nsrl_new.pebble --addr :8080
```
