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
	"flag"
	"fmt"
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
	"github.com/GreatPyreneseDad/loopmath-agent/internal/mcp"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/proxy"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "mcp":
			runMCP(os.Args[2:])
			return
		case "version", "-version", "--version":
			fmt.Println("loopmath-agent", loop.Version)
			return
		}
	}
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

// runMCP serves the stdio MCP server:  loopmath-agent mcp [-admin-url ...]
func runMCP(args []string) {
	fs := flag.NewFlagSet("loopmath-agent mcp", flag.ExitOnError)
	adminURL := fs.String("admin-url", envOr("LOOPMATH_ADMIN_URL", "http://127.0.0.1:8788"), "admin API of the running agent")
	proxyAddr := fs.String("proxy", envOr("LOOPMATH_PROXY_ADDR", ":8787"), "proxy address used when starting the agent")
	adminAddr := fs.String("admin", envOr("LOOPMATH_ADMIN_ADDR", "127.0.0.1:8788"), "admin address used when starting the agent")
	fs.Parse(args)
	self, _ := os.Executable()
	srv := mcp.New(*adminURL, *proxyAddr, *adminAddr, self, loop.Version)
	if err := srv.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
