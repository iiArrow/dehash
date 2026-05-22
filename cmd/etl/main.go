package main

import (
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/bloom"
	"github.com/vmihailenco/msgpack/v5"
	_ "modernc.org/sqlite"

	"dehash/internal/model"
	"dehash/internal/store"
)

// ── tuning constants ──────────────────────────────────────────────────────────
const (
	rowChannelBuffer = 20_000 // rows buffered between SQLite reader and workers
	kvChannelBuffer  = 10_000 // kv batches buffered between workers and Pebble writer
	commitEvery      = 500_000
)

// ── data types flowing through the pipeline ───────────────────────────────────

type rawRow struct {
	sha256, sha1, md5, crc32 string
	fileName                 string
	fileSize                 int64
	path                     string
	packageID                int64
}

type pkgInfo struct {
	appName, appVersion, appType string
	manufacturer, osName, osVersion, language string
}

type kvPair struct{ key, val []byte }

// ─────────────────────────────────────────────────────────────────────────────

func main() {
	sqlitePath := flag.String("db", "", "Path to NSRL SQLite database (required)")
	pebblePath := flag.String("out", "data/nsrl.pebble", "Output Pebble DB directory")
	workers    := flag.Int("workers", runtime.NumCPU(), "Serialization worker count")
	flag.Parse()

	if *sqlitePath == "" {
		log.Fatal("--db is required")
	}

	log.Printf("Opening SQLite: %s", *sqlitePath)
	sqlDB, err := sql.Open("sqlite", *sqlitePath)
	if err != nil {
		log.Fatalf("open sqlite: %v", err)
	}
	defer sqlDB.Close()
	sqlDB.SetMaxOpenConns(1)

	applyPragmas(sqlDB)

	// ── Phase 1: preload package map into RAM ─────────────────────────────────
	// There are far fewer packages than METADATA rows, so this is cheap.
	// Avoids executing a multi-table JOIN for every single METADATA row.
	log.Println("Phase 1: loading package details into memory...")
	start := time.Now()
	pkgMap, err := loadPackageMap(sqlDB)
	if err != nil {
		log.Fatalf("load package map: %v", err)
	}
	log.Printf("  %d packages loaded in %s", len(pkgMap), time.Since(start).Round(time.Millisecond))

	// ── Open Pebble with write-optimised settings ─────────────────────────────
	log.Printf("Opening Pebble: %s", *pebblePath)
	pb, err := openPebble(*pebblePath)
	if err != nil {
		log.Fatalf("open pebble: %v", err)
	}
	defer pb.Close()

	// ── Phase 2: stream METADATA through parallel pipeline ────────────────────
	log.Printf("Phase 2: streaming METADATA with %d workers...", *workers)
	start = time.Now()

	rowCh := make(chan rawRow, rowChannelBuffer)
	kvCh  := make(chan []kvPair, kvChannelBuffer)

	// Producer: single goroutine reads from SQLite
	go func() {
		defer close(rowCh)
		if err := streamRows(sqlDB, rowCh); err != nil {
			log.Printf("stream error: %v", err)
		}
	}()

	// Workers: parallel msgpack serialisation
	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for row := range rowCh {
				if pairs := serializeRow(row, pkgMap); len(pairs) > 0 {
					kvCh <- pairs
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(kvCh)
	}()

	// Consumer: single goroutine owns the Pebble batch
	count, skipped := writeToPebble(pb, kvCh)

	elapsed := time.Since(start)
	fmt.Printf("\n\nDone.\n")
	fmt.Printf("  Records imported : %d\n", count)
	fmt.Printf("  Records skipped  : %d\n", skipped)
	fmt.Printf("  Total time       : %s\n", elapsed.Round(time.Second))
	fmt.Printf("  Average rate     : %.0f records/s\n", float64(count)/elapsed.Seconds())
	fmt.Printf("  Pebble DB path   : %s\n", *pebblePath)
}

// ── Phase 1: package map ──────────────────────────────────────────────────────

const pkgQuery = `
SELECT
    po.package_id,
    COALESCE(a.name, ''),
    COALESCE(
        NULLIF(a.version,          ''),
        NULLIF(a.build,            ''),
        NULLIF(a.latest_copyright, ''),
        NULLIF(a.other,            ''),
        ''
    ),
    COALESCE(at_agg.description, ''),
    COALESCE(mfg_agg.name,       ''),
    COALESCE(os_agg.name,        ''),
    COALESCE(os_agg.version,     ''),
    COALESCE(lang_agg.name,      '')
FROM PACKAGE_OBJECT po
JOIN APPLICATION a ON po.package_id = a.package_id
LEFT JOIN (
    SELECT aat.application_id, MIN(at.description) AS description
    FROM APPLICATION_APPLICATION_TYPE aat
    JOIN APPLICATION_TYPE at ON aat.application_type_id = at.application_type_id
    GROUP BY aat.application_id
) at_agg ON a.application_id = at_agg.application_id
LEFT JOIN (
    SELECT ma.application_id, MIN(mfg.name) AS name
    FROM MANUFACTURER_APPLICATION ma
    JOIN MANUFACTURER mfg ON ma.manufacturer_id = mfg.manufacturer_id
    GROUP BY ma.application_id
) mfg_agg ON a.application_id = mfg_agg.application_id
LEFT JOIN (
    SELECT osa.application_id, MIN(os.name) AS name, MIN(os.version) AS version
    FROM OPERATING_SYSTEM_APPLICATION osa
    JOIN OPERATING_SYSTEM os ON osa.operating_system_id = os.operating_system_id
    GROUP BY osa.application_id
) os_agg ON a.application_id = os_agg.application_id
LEFT JOIN (
    SELECT al.application_id, MIN(l.name) AS name
    FROM APPLICATION_LANGUAGE al
    JOIN LANGUAGE l ON al.language_id = l.language_id
    GROUP BY al.application_id
) lang_agg ON a.application_id = lang_agg.application_id
GROUP BY po.package_id
`

func loadPackageMap(db *sql.DB) (map[int64]*pkgInfo, error) {
	rows, err := db.Query(pkgQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := make(map[int64]*pkgInfo, 500_000)
	for rows.Next() {
		var id int64
		p := &pkgInfo{}
		if err := rows.Scan(&id, &p.appName, &p.appVersion, &p.appType,
			&p.manufacturer, &p.osName, &p.osVersion, &p.language); err != nil {
			continue
		}
		m[id] = p
	}
	return m, rows.Err()
}

// ── Phase 2: stream METADATA rows ────────────────────────────────────────────

// Simple query — no subqueries, just one JOIN on an indexed column.
const metaQuery = `
SELECT
    md.sha256,
    md.sha1,
    md.md5,
    md.crc32,
    CASE WHEN md.extension = '' OR md.extension IS NULL
         THEN md.file_name
         ELSE md.file_name || '.' || md.extension
    END,
    md.bytes,
    COALESCE(md.path, ''),
    COALESCE(po.package_id, 0)
FROM METADATA md
LEFT JOIN PACKAGE_OBJECT po ON md.object_id = po.object_id
`

func streamRows(db *sql.DB, out chan<- rawRow) error {
	rows, err := db.Query(metaQuery)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var r rawRow
		if err := rows.Scan(
			&r.sha256, &r.sha1, &r.md5, &r.crc32,
			&r.fileName, &r.fileSize, &r.path, &r.packageID,
		); err != nil {
			continue
		}
		out <- r
	}
	return rows.Err()
}

// ── Serialisation worker ──────────────────────────────────────────────────────

func serializeRow(r rawRow, pkgMap map[int64]*pkgInfo) []kvPair {
	d := model.FileDetails{
		SHA256:   strings.ToUpper(r.sha256),
		SHA1:     strings.ToUpper(r.sha1),
		MD5:      strings.ToUpper(r.md5),
		CRC32:    strings.ToUpper(r.crc32),
		FileName: r.fileName,
		FileSize: r.fileSize,
		Path:     r.path,
	}
	if p, ok := pkgMap[r.packageID]; ok && p != nil {
		d.AppName      = p.appName
		d.AppVersion   = p.appVersion
		d.AppType      = p.appType
		d.Manufacturer = p.manufacturer
		d.OSName       = p.osName
		d.OSVersion    = p.osVersion
		d.Language     = p.language
	}

	val, err := msgpack.Marshal(&d)
	if err != nil {
		return nil
	}

	pairs := make([]kvPair, 0, 4)
	for _, h := range []struct {
		prefix byte
		hexStr string
	}{
		{store.PrefixSHA256, r.sha256},
		{store.PrefixSHA1, r.sha1},
		{store.PrefixMD5, r.md5},
		{store.PrefixCRC32, r.crc32},
	} {
		if h.hexStr == "" {
			continue
		}
		raw, err := hex.DecodeString(h.hexStr)
		if err != nil {
			continue
		}
		pairs = append(pairs, kvPair{
			key: store.MakeKey(h.prefix, raw),
			val: val,
		})
	}
	return pairs
}

// ── Pebble batch writer ───────────────────────────────────────────────────────

func writeToPebble(pb *pebble.DB, kvCh <-chan []kvPair) (count, skipped int64) {
	batch := pb.NewBatch()
	var ops atomic.Int64
	start := time.Now()

	// Progress ticker
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				n := ops.Load()
				elapsed := time.Since(start).Seconds()
				fmt.Printf("\r  %d records | %.0f rec/s    ", n, float64(n)/elapsed)
			case <-done:
				return
			}
		}
	}()

	for pairs := range kvCh {
		for _, kv := range pairs {
			if err := batch.Set(kv.key, kv.val, nil); err != nil {
				atomic.AddInt64(&skipped, 1)
				continue
			}
		}
		ops.Add(1)
		count++

		if count%commitEvery == 0 {
			if err := batch.Commit(pebble.NoSync); err != nil {
				log.Fatalf("batch commit: %v", err)
			}
			batch = pb.NewBatch()
		}
	}

	if err := batch.Commit(pebble.Sync); err != nil {
		log.Fatalf("final commit: %v", err)
	}
	close(done)
	return count, skipped
}

// ── helpers ───────────────────────────────────────────────────────────────────

func applyPragmas(db *sql.DB) {
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA cache_size=-2097152",    // 2 GB page cache
		"PRAGMA temp_store=MEMORY",
		"PRAGMA mmap_size=17179869184",  // 16 GB mmap
		"PRAGMA synchronous=OFF",
		"PRAGMA query_only=ON",
		"PRAGMA threads=4",
	} {
		if _, err := db.Exec(p); err != nil {
			log.Printf("pragma warning: %v", err)
		}
	}
}

func openPebble(path string) (*pebble.DB, error) {
	opts := &pebble.Options{
		// Large write buffer — reduces compaction pressure during bulk import.
		MemTableSize:                256 << 20,
		MemTableStopWritesThreshold: 8,
		// Skip WAL — ETL is idempotent; crash = re-run.
		DisableWAL: true,
		// Allow aggressive background compaction while writing.
		MaxConcurrentCompactions: func() int { return runtime.NumCPU() / 2 },
		// 512 MB block cache for hot blocks.
		Cache: pebble.NewCache(512 << 20),
		Levels: []pebble.LevelOptions{
			{
				BlockSize:    32 * 1024,
				FilterPolicy: bloom.FilterPolicy(10),
				FilterType:   pebble.TableFilter,
				Compression:  pebble.SnappyCompression,
			},
		},
	}
	return pebble.Open(path, opts)
}
