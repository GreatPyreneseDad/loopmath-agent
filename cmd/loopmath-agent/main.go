// loopmath-agent: a customer-held proxy that sits beside your model gateway
// and emits loop-cost findings. Prompts stay inside. Findings leave.
//
//	loopmath-agent                       # :8787 proxy, 127.0.0.1:8788 admin
//	export ANTHROPIC_BASE_URL=http://localhost:8787
//	export OPENAI_BASE_URL=http://localhost:8787/openai
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/GreatPyreneseDad/loopmath-agent/internal/api"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/config"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/cost"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/findings"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/loop"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/proxy"
)

func main() {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	price, err := cost.Load(cfg.PriceFile)
	if err != nil {
		log.Fatalf("prices: %v", err)
	}
	emit, err := findings.NewEmitter(cfg.FindingsFile, cfg.SinkURL, cfg.SinkToken, cfg.SinkFlush, 5000)
	if err != nil {
		log.Fatalf("findings: %v", err)
	}
	defer emit.Close()

	engine := loop.New(cfg, price, emit)
	px, err := proxy.New(cfg, engine)
	if err != nil {
		log.Fatalf("proxy: %v", err)
	}
	adm := api.New(engine, emit, price, px)

	proxySrv := &http.Server{Addr: cfg.ProxyAddr, Handler: px, ReadHeaderTimeout: 30 * time.Second}
	adminSrv := &http.Server{Addr: cfg.AdminAddr, Handler: adm, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		log.Printf("loopmath-agent %s: proxy on %s (anthropic→%s, openai→%s)", loop.Version, cfg.ProxyAddr, cfg.AnthropicUpstream, cfg.OpenAIUpstream)
		if err := proxySrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	go func() {
		log.Printf("admin on %s: /v1/findings /v1/loops /metrics", cfg.AdminAddr)
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	if cfg.SinkURL != "" {
		log.Printf("findings sink: %s (findings only; no prompt text leaves this host)", cfg.SinkURL)
	}
	if cfg.FindingsFile != "" {
		log.Printf("findings file: %s", cfg.FindingsFile)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	proxySrv.Shutdown(ctx)
	adminSrv.Shutdown(ctx)
}
