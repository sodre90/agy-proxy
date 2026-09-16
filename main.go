// agy-proxy exposes the Antigravity CLI's model access (your Google AI
// subscription) as an Anthropic Messages API endpoint, so Claude Code can run
// on models such as gemini-3.8-flash.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type logger struct {
	verbose bool
	log     *log.Logger
}

func (l *logger) debugf(format string, v ...any) {
	if l.verbose {
		l.log.Printf("[debug] "+format, v...)
	}
}

func main() {
	addr := flag.String("addr", envOr("AGY_PROXY_ADDR", "127.0.0.1:8788"), "listen address (loopback only unless you know better)")
	defaultModel := flag.String("model", envOr("AGY_PROXY_MODEL", "gemini-3.8-flash-high"), "upstream model used when the request says sonnet/opus/haiku or sends nothing")
	baseURL := flag.String("base-url", envOr("AGY_PROXY_BASE_URL", defaultBaseURL), "upstream v1internal base URL")
	proxyToken := flag.String("token", envOr("AGY_PROXY_TOKEN", ""), "optional auth token Claude Code must present; empty accepts anything")
	stateDir := flag.String("state-dir", envOr("AGY_PROXY_STATE", defaultStateDir()), "directory for cached credentials")
	check := flag.Bool("check", false, "verify credentials and the model catalog, then exit")
	verbose := flag.Bool("verbose", false, "log request details (never tokens)")
	flag.Parse()

	l := &logger{verbose: *verbose, log: log.New(os.Stderr, "agy-proxy ", log.LstdFlags|log.Lmicroseconds)}

	tokens, err := newTokenManager(*stateDir, l)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agy-proxy: %v\n", err)
		os.Exit(1)
	}
	agy := newAgyClient(*baseURL, tokens, l)
	cats := newCatalog(agy)

	if *check {
		runSelfCheck(agy, cats)
		return
	}

	srv := &proxyServer{
		agy:          agy,
		catalog:      cats,
		sigs:         newSignatureStore(*stateDir),
		defaultModel: *defaultModel,
		authToken:    *proxyToken,
		logger:       l,
	}
	httpServer := &http.Server{Addr: *addr, Handler: srv.handler()}
	l.log.Printf("listening on http://%s (anthropic /v1/messages), default model %s", *addr, *defaultModel)
	if err := httpServer.ListenAndServe(); err != nil {
		fmt.Fprintf(os.Stderr, "agy-proxy: %v\n", err)
		os.Exit(1)
	}
}

func runSelfCheck(agy *agyClient, cats *catalog) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := cats.Models(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "agy-proxy: credential or catalog check failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("agy-proxy: token refresh OK, catalog reachable at %s\n", agy.baseURL)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".agy-proxy"
	}
	return filepath.Join(home, ".config", "agy-proxy")
}
