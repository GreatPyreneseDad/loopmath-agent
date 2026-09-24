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
	"strings"
	"syscall"
	"time"

	"github.com/GreatPyreneseDad/loopmath-agent/internal/api"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/config"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/cost"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/findings"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/loop"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/mcp"
	"github.com/GreatPyreneseDad/loopmath-agent/internal/otlp"
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
		// read-only filesystem (distroless, Fly) — keep running with the in-memory ring + sink
		log.Printf("findings file %q unavailable (%v); continuing without it", cfg.FindingsFile, err)
		emit, _ = findings.NewEmitter("", cfg.SinkURL, cfg.SinkToken, cfg.SinkFlush, 5000)
	}
	defer emit.Close()

	engine := loop.New(cfg, price, emit)
	px, err := proxy.New(cfg, engine)
	if err != nil {
		log.Fatalf("proxy: %v", err)
	}
	rcv := otlp.New(engine).WithToken(cfg.ProxyToken)
	adm := api.New(engine, emit, price, px, rcv, cfg.AdminToken)

	// MCP over HTTP: the same tools as `loopmath-agent mcp`, served by this process.
	// Thin client over our own admin API (loopback), so behavior is identical.
	selfAdmin := "http://127.0.0.1" + portOf(cfg.AdminAddr)
	if cfg.Single {
		selfAdmin = "http://127.0.0.1" + portOf(cfg.ProxyAddr) + "/_loopmath"
	}
	os.Setenv("LOOPMATH_ADMIN_TOKEN", cfg.AdminToken)
	mcpSrv := mcp.New(selfAdmin, cfg.ProxyAddr, cfg.AdminAddr, "", loop.Version)
	if cfg.PublicURL != "" {
		mcpSrv.Hosted(cfg.PublicURL, cfg.ProxyToken)
	}
	mcpHTTP := mcp.HTTPHandler{S: mcpSrv, Token: cfg.AdminToken}

	var front http.Handler = px
	if cfg.Single {
		// One port for PaaS: /_loopmath/* → admin, /v1/traces → otlp, everything else → proxy
		mux := http.NewServeMux()
		mux.Handle("/_loopmath/mcp", mcpHTTP)
		mux.Handle("/_loopmath/mcp/", mcpHTTP)
		mux.Handle("/_loopmath/", http.StripPrefix("/_loopmath", adm))
		mux.Handle("/v1/traces", rcv)
		mux.Handle("/", px)
		front = mux
		if cfg.ProxyToken == "" || cfg.AdminToken == "" {
			log.Printf("WARNING: -single without -proxy-token/-admin-token exposes the proxy and cost data to anyone who can reach %s", cfg.ProxyAddr)
		}
	}
	proxySrv := &http.Server{Addr: cfg.ProxyAddr, Handler: front, ReadHeaderTimeout: 30 * time.Second}
	adminMux := http.NewServeMux()
	adminMux.Handle("/mcp", mcpHTTP)
	adminMux.Handle("/mcp/", mcpHTTP)
	adminMux.Handle("/", adm)
	adminSrv := &http.Server{Addr: cfg.AdminAddr, Handler: adminMux, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		log.Printf("loopmath-agent %s: proxy on %s (anthropic→%s, openai→%s, gemini→%s) billing=%s", loop.Version, cfg.ProxyAddr, cfg.AnthropicUpstream, cfg.OpenAIUpstream, cfg.GeminiUpstream, cfg.Billing)
		if cfg.ProxyToken != "" {
			log.Printf("proxy token required (X-Loopmath-Token or /t/<token>/)")
		}
		if err := proxySrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	if cfg.Single {
		log.Printf("single-port mode: admin at %s/_loopmath/ (v1/findings, v1/loops, metrics), MCP at %s/_loopmath/mcp, otlp at %s/v1/traces", cfg.ProxyAddr, cfg.ProxyAddr, cfg.ProxyAddr)
	} else {
		go func() {
			log.Printf("admin on %s: /v1/findings /v1/loops /metrics, MCP over HTTP at /mcp", cfg.AdminAddr)
			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal(err)
			}
		}()
	}
	var otlpSrv *http.Server
	if cfg.OTLPAddr != "" && !cfg.Single {
		otlpSrv = &http.Server{Addr: cfg.OTLPAddr, Handler: rcv, ReadHeaderTimeout: 30 * time.Second}
		go func() {
			log.Printf("otlp receiver on %s: POST /v1/traces (GenAI spans; protobuf or JSON, gzip ok)", cfg.OTLPAddr)
			if err := otlpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal(err)
			}
		}()
	}
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
	if otlpSrv != nil {
		otlpSrv.Shutdown(ctx)
	}
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

// portOf(":8080") == ":8080"; portOf("0.0.0.0:8080") == ":8080"
func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i:]
	}
	return addr
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
