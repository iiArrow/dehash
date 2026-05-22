package api

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"

	"github.com/valyala/fasthttp"

	"dehash/internal/model"
	"dehash/internal/store"
)

const maxBulkWorkers = 64

type Handler struct {
	store *store.Store
	sem   chan struct{}
}

func New(s *store.Store) *Handler {
	return &Handler{
		store: s,
		sem:   make(chan struct{}, maxBulkWorkers),
	}
}

// HandleLookup handles POST /lookup
//
// Request:  {"hash": "<hex>", "details": true|false}
// Response: {"status":"found"|"not_found"} or {"status":"found","details":{...}}
func (h *Handler) HandleLookup(ctx *fasthttp.RequestCtx) {
	var req struct {
		Hash    string `json:"hash"`
		Details bool   `json:"details"`
	}
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		badRequest(ctx, "invalid JSON")
		return
	}
	if req.Hash == "" {
		badRequest(ctx, "hash is required")
		return
	}

	ctx.SetContentType("application/json")

	details, err := h.store.Lookup(req.Hash)
	if err != nil {
		if err == store.ErrNotFound {
			ctx.SetBodyString(`{"status":"not_found"}`)
			return
		}
		if err == store.ErrInvalidHash {
			badRequest(ctx, err.Error())
			return
		}
		writeJSON(ctx, 500, map[string]string{"error": err.Error()})
		return
	}

	if !req.Details {
		ctx.SetBodyString(`{"status":"found"}`)
		return
	}
	writeJSON(ctx, 200, map[string]any{"status": "found", "details": details})
}

// HandleBulk handles POST /bulk
//
// Request:  {"hashes": ["<hex>", ...], "details": true|false}
// Response: {"not_found": [...], "found_count": N}
//
//	or with details: {"not_found": [...], "found": {"<hash>": {...}}, "found_count": N}
func (h *Handler) HandleBulk(ctx *fasthttp.RequestCtx) {
	var req struct {
		Hashes  []string `json:"hashes"`
		Details bool     `json:"details"`
	}
	if err := json.Unmarshal(ctx.PostBody(), &req); err != nil {
		badRequest(ctx, "invalid JSON")
		return
	}
	if len(req.Hashes) == 0 {
		badRequest(ctx, "hashes array is required and must not be empty")
		return
	}

	ctx.SetContentType("application/json")
	writeJSON(ctx, 200, h.processBulk(req.Hashes, req.Details))
}

// HandleFile handles POST /file
//
// Body:  plain text, one hash per line (no JSON wrapping)
// Query: ?details=true|false
//
// Designed for large hash lists (~100k lines). Same response format as /bulk.
// Example: curl -X POST http://localhost:8080/file?details=false --data-binary @hashes.txt
func (h *Handler) HandleFile(ctx *fasthttp.RequestCtx) {
	details := string(ctx.QueryArgs().Peek("details")) == "true"

	hashes := parseHashLines(ctx.PostBody())
	if len(hashes) == 0 {
		badRequest(ctx, "request body is empty or contains no valid hashes")
		return
	}

	ctx.SetContentType("application/json")
	writeJSON(ctx, 200, h.processBulk(hashes, details))
}

// processBulk is shared by HandleBulk and HandleFile.
func (h *Handler) processBulk(hashes []string, details bool) map[string]any {
	type result struct {
		d     *model.FileDetails
		found bool
	}
	results := make([]result, len(hashes))

	var wg sync.WaitGroup
	for i, hashStr := range hashes {
		wg.Add(1)
		h.sem <- struct{}{}
		go func(idx int, hs string) {
			defer func() {
				<-h.sem
				wg.Done()
			}()
			d, err := h.store.Lookup(hs)
			if err == nil {
				results[idx] = result{d: d, found: true}
			}
		}(i, hashStr)
	}
	wg.Wait()

	notFound := make([]string, 0)
	foundCount := 0

	if details {
		foundMap := make(map[string]*model.FileDetails)
		for i, r := range results {
			if r.found {
				foundCount++
				foundMap[hashes[i]] = r.d
			} else {
				notFound = append(notFound, hashes[i])
			}
		}
		return map[string]any{
			"not_found":   notFound,
			"found":       foundMap,
			"found_count": foundCount,
		}
	}

	for i, r := range results {
		if r.found {
			foundCount++
		} else {
			notFound = append(notFound, hashes[i])
		}
	}
	return map[string]any{
		"not_found":   notFound,
		"found_count": foundCount,
	}
}

// parseHashLines splits a plain-text body into individual hash strings.
func parseHashLines(body []byte) []string {
	lines := bytes.Split(body, []byte("\n"))
	hashes := make([]string, 0, len(lines))
	for _, line := range lines {
		h := strings.TrimSpace(string(line))
		if h != "" {
			hashes = append(hashes, h)
		}
	}
	return hashes
}

func badRequest(ctx *fasthttp.RequestCtx, msg string) {
	writeJSON(ctx, 400, map[string]string{"error": msg})
}

func writeJSON(ctx *fasthttp.RequestCtx, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		ctx.SetStatusCode(500)
		ctx.SetBodyString(`{"error":"internal error"}`)
		return
	}
	ctx.SetContentType("application/json")
	ctx.SetStatusCode(status)
	ctx.SetBody(b)
}
