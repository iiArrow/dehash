package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/valyala/fasthttp"

	"dehash/internal/api"
	"dehash/internal/store"
)

func main() {
	dbPath := flag.String("db", "data/nsrl.pebble", "Path to Pebble DB directory")
	addr := flag.String("addr", ":8080", "Listen address (e.g. :8080)")
	flag.Parse()

	log.Printf("Opening Pebble (read-only): %s", *dbPath)
	s, err := store.Open(*dbPath, true)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer s.Close()

	h := api.New(s)

	srv := &fasthttp.Server{
		Handler:            router(h),
		Name:               "dehash",
		MaxRequestBodySize: 32 * 1024 * 1024, // 32 MB — supports ~400k hashes per bulk request
	}

	// Graceful shutdown on SIGTERM / SIGINT.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-quit
		log.Println("Shutting down...")
		if err := srv.Shutdown(); err != nil {
			log.Printf("shutdown error: %v", err)
		}
	}()

	log.Printf("dehash listening on %s", *addr)
	if err := srv.ListenAndServe(*addr); err != nil {
		log.Printf("server stopped: %v", err)
	}
}

func router(h *api.Handler) fasthttp.RequestHandler {
	return func(ctx *fasthttp.RequestCtx) {
		if !ctx.IsPost() && string(ctx.Path()) != "/health" {
			ctx.SetStatusCode(fasthttp.StatusMethodNotAllowed)
			return
		}
		switch string(ctx.Path()) {
		case "/lookup":
			h.HandleLookup(ctx)
		case "/bulk":
			h.HandleBulk(ctx)
		case "/file":
			h.HandleFile(ctx)
		case "/health":
			ctx.SetContentType("application/json")
			ctx.SetBodyString(`{"status":"ok"}`)
		default:
			ctx.SetStatusCode(fasthttp.StatusNotFound)
			ctx.SetBodyString(`{"error":"not found"}`)
		}
	}
}
