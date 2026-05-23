#!/usr/bin/env bash
# dehash startup script — run with: ./start.sh
set -euo pipefail

# ── colours ──────────────────────────────────────────────────────────────────
CYAN='\033[0;36m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
RED='\033[0;31m'; BOLD='\033[1m'; RESET='\033[0m'

info()   { echo -e "${CYAN}→${RESET}  $*"; }
ok()     { echo -e "${GREEN}✓${RESET}  $*"; }
warn()   { echo -e "${YELLOW}!${RESET}  $*"; }
die()    { echo -e "${RED}✗${RESET}  $*" >&2; exit 1; }
banner() { echo -e "\n${BOLD}$*${RESET}"; echo "────────────────────────────────────────────"; }

show_usage() {
    local addr="$1"
    echo "  Quick test:"
    echo ""
    echo -e "  ${CYAN}# Single hash lookup${RESET}"
    echo "  curl -s -X POST http://localhost${addr}/lookup \\"
    echo "    -H 'Content-Type: application/json' \\"
    echo "    -d '{\"hash\":\"<your-hash-here>\",\"details\":true}'"
    echo ""
    echo -e "  ${CYAN}# Bulk lookup — returns only the NOT-found (not-legitimate) hashes${RESET}"
    echo "  curl -s -X POST http://localhost${addr}/bulk \\"
    echo "    -H 'Content-Type: application/json' \\"
    echo "    -d '{\"hashes\":[\"hash1\",\"hash2\",\"hash3\"]}'"
    echo ""
    echo "  Health check:  curl http://localhost${addr}/health"
    echo ""
    echo "  Press Ctrl+C to stop."
}

# ── always run from project root ─────────────────────────────────────────────
cd "$(dirname "$0")"

clear
echo -e "${BOLD}"
echo "  ██████╗ ███████╗██╗  ██╗ █████╗ ███████╗██╗  ██╗"
echo "  ██╔══██╗██╔════╝██║  ██║██╔══██╗██╔════╝██║  ██║"
echo "  ██║  ██║█████╗  ███████║███████║███████╗███████║"
echo "  ██║  ██║██╔══╝  ██╔══██║██╔══██║╚════██║██╔══██║"
echo "  ██████╔╝███████╗██║  ██║██║  ██║███████║██║  ██║"
echo "  ╚═════╝ ╚══════╝╚═╝  ╚═╝╚═╝  ╚═╝╚══════╝╚═╝  ╚═╝"
echo -e "${RESET}"
echo "  NSRL Hash Lookup Service"
echo ""

# ── build binaries if missing ────────────────────────────────────────────────
_need_build=false
[[ ! -f bin/etl || ! -f bin/server ]] && _need_build=true
# Rebuild if any Go source is newer than the compiled binaries.
if ! $_need_build && find . -name '*.go' -newer bin/etl -print -quit 2>/dev/null | grep -q .; then
    _need_build=true
fi
if $_need_build; then
    banner "Building binaries"
    command -v go &>/dev/null || die "Go is not installed. Get it from https://go.dev/dl/"
    mkdir -p bin
    info "Running go mod tidy..."
    go mod tidy
    info "Building bin/etl and bin/server..."
    go build -ldflags="-s -w" -o bin/etl    ./cmd/etl
    go build -ldflags="-s -w" -o bin/server ./cmd/server
    ok "Build complete."
fi

# ── main menu ────────────────────────────────────────────────────────────────
echo ""
echo "  What would you like to do?"
echo ""

HAS_DB=false
[[ -d data/nsrl.pebble ]] && HAS_DB=true

echo -e "  ${GREEN}1)${RESET} Import an NSRL file  →  then start the server"
if $HAS_DB; then
    echo -e "  ${GREEN}2)${RESET} Start the server        (Pebble DB already exists ✓)"
    echo -e "  ${GREEN}3)${RESET} Run stress test         (server must already be running)"
    echo -e "  ${GREEN}4)${RESET} Run blitz test          (max throughput, 30s, no limits)"
else
    echo     "     2)  Start the server        (no Pebble DB found yet)"
    echo     "     3)  Run stress test         (no Pebble DB found yet)"
    echo     "     4)  Run blitz test          (no Pebble DB found yet)"
fi
echo ""
read -rp "  Choice [1/2/3/4]: " CHOICE

# ══════════════════════════════════════════════════════════ OPTION 1 ═════════
if [[ "$CHOICE" == "1" ]]; then

    banner "Step 1 — Select your NSRL file"
    echo ""
    echo "  Accepted formats:"
    echo "    • SQL text dump  (e.g. RDS_2026.03.1_modern_delta.sql)"
    echo "    • Binary .db     (e.g. NSRLFile.db — the full 142 GB database)"
    echo ""
    read -rp "  Path to NSRL file: " INPUT_FILE
    INPUT_FILE="${INPUT_FILE/#\~/$HOME}"          # expand ~
    [[ -f "$INPUT_FILE" ]] || die "File not found: $INPUT_FILE"

    # Detect file type by magic bytes.
    if head -c 16 "$INPUT_FILE" 2>/dev/null | grep -q "SQLite format 3"; then
        ok "Detected binary SQLite database — ready for ETL."
        DB_FILE="$INPUT_FILE"
    else
        ok "Detected SQL text dump — will convert to a SQLite database."
        echo ""

        command -v sqlite3 &>/dev/null || \
            die "sqlite3 is required to import a SQL text dump.\n\n  Install:\n    sudo pacman -S sqlite    # Arch / CachyOS\n    sudo apt install sqlite3 # Debian / Ubuntu"

        # Auto-detect schema file next to the data file.
        SQL_DIR="$(dirname "$INPUT_FILE")"
        SCHEMA_FILE="$(find "$SQL_DIR" -maxdepth 1 -name "*.schema.sql" 2>/dev/null | head -1 || true)"

        if [[ -n "$SCHEMA_FILE" ]]; then
            ok "Found schema: $(basename "$SCHEMA_FILE")"
        else
            warn "No *.schema.sql found next to the data file."
            read -rp "  Path to schema file (leave blank to skip): " SCHEMA_FILE
            SCHEMA_FILE="${SCHEMA_FILE/#\~/$HOME}"
            [[ -z "$SCHEMA_FILE" || -f "$SCHEMA_FILE" ]] || die "Schema file not found: $SCHEMA_FILE"
        fi

        mkdir -p data
        DB_FILE="data/nsrl_import.db"
        rm -f "$DB_FILE"

        if [[ -n "$SCHEMA_FILE" && -f "$SCHEMA_FILE" ]]; then
            info "Applying schema..."
            sqlite3 "$DB_FILE" < "$SCHEMA_FILE"
            ok "Schema applied."
        fi

        info "Importing data ($(wc -l < "$INPUT_FILE" | xargs) lines — please wait)..."
        sqlite3 "$DB_FILE" < "$INPUT_FILE"
        ok "SQLite database created: $DB_FILE"
    fi

    banner "Step 2 — ETL: building Pebble lookup database"
    echo ""
    warn "Time estimate:"
    echo "     • Delta / small file  →  minutes to ~1 hour"
    echo "     • Full 142 GB DB      →  several hours"
    echo ""
    read -rp "  Pebble output path [default: data/nsrl.pebble]: " PEBBLE_PATH
    PEBBLE_PATH="${PEBBLE_PATH:-data/nsrl.pebble}"
    mkdir -p "$(dirname "$PEBBLE_PATH")"

    echo ""
    ./bin/etl --db "$DB_FILE" --out "$PEBBLE_PATH"
    ok "ETL complete — Pebble DB: $PEBBLE_PATH"

    echo ""
    read -rp "  Listen address [default: :8080]: " ADDR
    ADDR="${ADDR:-:8080}"

    banner "Step 3 — Starting server"
    ok "dehash running on  http://0.0.0.0${ADDR}"
    echo ""
    show_usage "$ADDR"
    exec ./bin/server --db "$PEBBLE_PATH" --addr "$ADDR"

# ══════════════════════════════════════════════════════════ OPTION 2 ═════════
elif [[ "$CHOICE" == "2" ]]; then

    $HAS_DB || die "No Pebble DB found at data/nsrl.pebble\nRun option 1 first to import an NSRL file."

    echo ""
    read -rp "  Listen address [default: :8080]: " ADDR
    ADDR="${ADDR:-:8080}"

    banner "Starting server"
    ok "dehash running on  http://0.0.0.0${ADDR}"
    echo ""
    show_usage "$ADDR"
    exec ./bin/server --db data/nsrl.pebble --addr "$ADDR"

# ══════════════════════════════════════════════════════════ OPTION 3 ═════════
elif [[ "$CHOICE" == "3" ]]; then

    $HAS_DB || die "No Pebble DB found — run option 1 first."

    echo ""
    read -rp "  Server address [default: http://localhost:8080]: " ADDR
    ADDR="${ADDR:-http://localhost:8080}"

    read -rp "  Hash file [default: hashes_amcache.txt]: " HASH_FILE
    HASH_FILE="${HASH_FILE:-hashes_amcache.txt}"
    [[ -f "$HASH_FILE" ]] || die "Hash file not found: $HASH_FILE"

    read -rp "  Concurrency (single requests) [default: 50]: " CONC
    CONC="${CONC:-50}"

    read -rp "  Batch size (hashes per bulk request) [default: 1000]: " BATCH
    BATCH="${BATCH:-1000}"

    banner "Stress test"
    exec ./bin/stress \
        --addr "$ADDR" \
        --hashes "$HASH_FILE" \
        --concurrency "$CONC" \
        --batch "$BATCH"

# ══════════════════════════════════════════════════════════ OPTION 4 ═════════
elif [[ "$CHOICE" == "4" ]]; then

    $HAS_DB || die "No Pebble DB found — run option 1 first."

    echo ""
    read -rp "  Server address [default: http://localhost:8080]: " ADDR
    ADDR="${ADDR:-http://localhost:8080}"

    read -rp "  Hash file [default: hashes_amcache.txt]: " HASH_FILE
    HASH_FILE="${HASH_FILE:-hashes_amcache.txt}"
    [[ -f "$HASH_FILE" ]] || die "Hash file not found: $HASH_FILE"

    read -rp "  Goroutines [default: 500]: " WORKERS
    WORKERS="${WORKERS:-500}"

    read -rp "  Duration  [default: 30s]: " DUR
    DUR="${DUR:-30s}"

    banner "Blitz test — max throughput"
    exec ./bin/blitz \
        --addr "$ADDR" \
        --hashes "$HASH_FILE" \
        --workers "$WORKERS" \
        --duration "$DUR"

else
    die "Invalid choice '$CHOICE' — please re-run and enter 1, 2, 3, or 4."
fi
