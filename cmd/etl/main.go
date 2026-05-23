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

// ── tuning ────────────────────────────────────────────────────────────────────
const (
	rowChannelBuffer = 20_000
	kvChannelBuffer  = 10_000
	commitEvery      = 500_000
)

// ── schema variants ───────────────────────────────────────────────────────────
// Modern schema  : main table = METADATA, relational package data
// Minimal schema : main table = FILE,     flat PKG/MFG/OS tables
type schemaKind int

const (
	schemaNone    schemaKind = iota
	schemaModern             // METADATA + PACKAGE_OBJECT + APPLICATION + ...
	schemaMinimal            // FILE + PKG + MFG + OS
)

// ── pipeline types ────────────────────────────────────────────────────────────
type rawRow struct {
	sha256, sha1, md5, crc32 string
	fileName                 string
	fileSize                 int64
	path                     string // empty in minimal (FILE has no path column)
	packageID                int64
}

type pkgInfo struct {
	appName, appVersion, appType         string
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

	// ── detect schema ─────────────────────────────────────────────────────────
	schema := detectSchema(sqlDB)
	switch schema {
	case schemaModern:
		log.Println("Schema detected: modern  (METADATA + relational package tables)")
	case schemaMinimal:
		log.Println("Schema detected: minimal (FILE + flat PKG/MFG/OS tables)")
	}

	// ── Phase 1: load package map into RAM ────────────────────────────────────
	log.Println("Phase 1: loading package details into memory...")
	start := time.Now()
	pkgMap, err := loadPackageMap(sqlDB, schema)
	if err != nil {
		log.Fatalf("load package map: %v", err)
	}
	log.Printf("  %d packages loaded in %s", len(pkgMap), time.Since(start).Round(time.Millisecond))

	// ── Open Pebble ───────────────────────────────────────────────────────────
	log.Printf("Opening Pebble: %s", *pebblePath)
	pb, err := openPebble(*pebblePath)
	if err != nil {
		log.Fatalf("open pebble: %v", err)
	}
	defer pb.Close()

	// ── Phase 2: stream rows through parallel pipeline ────────────────────────
	log.Printf("Phase 2: streaming rows with %d workers...", *workers)
	start = time.Now()

	rowCh := make(chan rawRow, rowChannelBuffer)
	kvCh  := make(chan []kvPair, kvChannelBuffer)

	go func() {
		defer close(rowCh)
		if err := streamRows(sqlDB, schema, rowCh); err != nil {
			log.Printf("stream error: %v", err)
		}
	}()

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
	go func() { wg.Wait(); close(kvCh) }()

	count, skipped := writeToPebble(pb, kvCh)

	elapsed := time.Since(start)
	fmt.Printf("\n\nDone.\n")
	fmt.Printf("  Records imported : %d\n", count)
	fmt.Printf("  Records skipped  : %d\n", skipped)
	fmt.Printf("  Total time       : %s\n", elapsed.Round(time.Second))
	fmt.Printf("  Average rate     : %.0f records/s\n", float64(count)/elapsed.Seconds())
	fmt.Printf("  Pebble DB path   : %s\n", *pebblePath)
}

// ── schema detection ──────────────────────────────────────────────────────────

func detectSchema(db *sql.DB) schemaKind {
	switch {
	case tableExists(db, "METADATA"):
		return schemaModern
	case tableExists(db, "FILE"):
		return schemaMinimal
	default:
		// List tables to help diagnose unknown schemas.
		rows, _ := db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
		var tables []string
		if rows != nil {
			for rows.Next() {
				var t string
				rows.Scan(&t)
				tables = append(tables, t)
			}
			rows.Close()
		}
		log.Fatalf("unrecognized NSRL schema — found tables: %v\n"+
			"  Expected: METADATA (modern) or FILE (minimal)", tables)
		return schemaNone
	}
}

func tableExists(db *sql.DB, name string) bool {
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	return n > 0
}

// ── Phase 1: package map ──────────────────────────────────────────────────────

// Modern: join across the full relational schema.
const pkgQueryModern = `
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

// Minimal: PKG is already flat, just join MFG and OS for names.
const pkgQueryMinimal = `
SELECT
    p.package_id,
    COALESCE(p.name,             ''),
    COALESCE(p.version,          ''),
    COALESCE(p.application_type, ''),
    COALESCE(m.name,             ''),
    COALESCE(o.name,             ''),
    COALESCE(o.version,          ''),
    COALESCE(p.language,         '')
FROM PKG p
LEFT JOIN MFG m ON p.manufacturer_id  = m.manufacturer_id
LEFT JOIN OS  o ON p.operating_system_id = o.operating_system_id
GROUP BY p.package_id
`

func loadPackageMap(db *sql.DB, schema schemaKind) (map[int64]*pkgInfo, error) {
	query := pkgQueryModern
	if schema == schemaMinimal {
		query = pkgQueryMinimal
	}

	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	m := make(map[int64]*pkgInfo, 500_000)
	for rows.Next() {
		var id int64
		p := &pkgInfo{}
		if err := rows.Scan(&id,
			&p.appName, &p.appVersion, &p.appType,
			&p.manufacturer, &p.osName, &p.osVersion, &p.language,
		); err != nil {
			continue
		}
		m[id] = p
	}
	return m, rows.Err()
}

// ── Phase 2: stream rows ──────────────────────────────────────────────────────

// Modern: METADATA joined to PACKAGE_OBJECT for package_id.
const metaQueryModern = `
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

// Minimal: FILE is already flat with package_id, file_name includes extension.
const metaQueryMinimal = `
SELECT
    sha256,
    sha1,
    md5,
    crc32,
    file_name,
    file_size,
    package_id
FROM FILE
`

func streamRows(db *sql.DB, schema schemaKind, out chan<- rawRow) error {
	query := metaQueryModern
	if schema == schemaMinimal {
		query = metaQueryMinimal
	}

	rows, err := db.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var r rawRow
		var scanErr error
		if schema == schemaMinimal {
			// FILE has no path column — leave r.path as ""
			scanErr = rows.Scan(
				&r.sha256, &r.sha1, &r.md5, &r.crc32,
				&r.fileName, &r.fileSize, &r.packageID,
			)
		} else {
			scanErr = rows.Scan(
				&r.sha256, &r.sha1, &r.md5, &r.crc32,
				&r.fileName, &r.fileSize, &r.path, &r.packageID,
			)
		}
		if scanErr != nil {
			continue
		}
		out <- r
	}
	return rows.Err()
}

// ── serialisation worker ──────────────────────────────────────────────────────

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
	if p := pkgMap[r.packageID]; p != nil {
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
		{store.PrefixSHA1,   r.sha1},
		{store.PrefixMD5,    r.md5},
		{store.PrefixCRC32,  r.crc32},
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

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				n := ops.Load()
				fmt.Printf("\r  %d records | %.0f rec/s    ", n, float64(n)/time.Since(start).Seconds())
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
		"PRAGMA cache_size=-2097152",
		"PRAGMA temp_store=MEMORY",
		"PRAGMA mmap_size=17179869184",
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
		MemTableSize:                256 << 20,
		MemTableStopWritesThreshold: 8,
		DisableWAL:                  true,
		MaxConcurrentCompactions:    func() int { return runtime.NumCPU() / 2 },
		Cache:                       pebble.NewCache(512 << 20),
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
