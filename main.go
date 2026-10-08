// Command log-server streams Apple unified logging (OSLog) output from a booted
// iOS simulator or a connected iOS device into a local web application, where the
// log can be filtered by subsystem and category, searched live and exported.
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/rudy/log-server/internal/history"
	"github.com/rudy/log-server/internal/server"
)

//go:embed all:web
var embeddedWeb embed.FS

func main() {
	addr := flag.String("addr", "127.0.0.1:8722", "address the web app listens on")
	bufferSize := flag.Int("buffer", 20000, "number of log entries kept in memory")
	openBrowser := flag.Bool("open", true, "open the web app in the default browser")
	webDir := flag.String("web", "", "serve web assets from this directory instead of the embedded copy (development)")
	historyPath := flag.String("history", defaultHistoryPath(),
		"file that receives every captured entry so it can be scrolled back beyond the in-memory buffer (\"\" disables it)")
	historyMaxMB := flag.Int("history-max-mb", 256, "stop recording history once the file reaches this size (0 = no limit)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `%s - live OSLog viewer for iOS simulators and devices

Usage:
  %s [flags]

Flags:
`, os.Args[0], os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintf(flag.CommandLine.Output(), `
Examples:
  %s                       # simulator capture (no elevated privileges needed)
  sudo %s                  # also allows capture from a connected iOS device
`, os.Args[0], os.Args[0])
	}
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags)
	if runtime.GOOS != "darwin" {
		logger.Fatal("this tool requires macOS: it drives the `log stream` and `xcrun simctl` commands")
	}

	webFS, err := webAssets(*webDir)
	if err != nil {
		logger.Fatalf("loading web assets: %v", err)
	}

	var store *history.Store
	if *historyPath != "" {
		opened, err := history.Open(*historyPath, int64(*historyMaxMB)<<20)
		if err != nil {
			logger.Fatalf("opening history file: %v", err)
		}
		store = opened
		defer store.Close()
		logger.Printf("capture history: %s (limit %d MB)", *historyPath, *historyMaxMB)
	}

	srv := server.New(server.Options{
		WebFS:      webFS,
		BufferSize: *bufferSize,
		Logger:     logger,
		History:    store,
	})
	defer srv.Close()

	url := fmt.Sprintf("http://%s", displayAddr(*addr))
	httpServer := &http.Server{
		Addr:    *addr,
		Handler: srv.Handler(),
		// No WriteTimeout: the SSE feed is a long lived response.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	logger.Printf("OSLog live viewer listening on %s (buffer=%d entries)", url, *bufferSize)
	if os.Geteuid() != 0 {
		logger.Printf("note: capture from a connected iOS device needs admin privileges - run with sudo to enable it")
	}

	if *openBrowser {
		go func() {
			time.Sleep(300 * time.Millisecond)
			if err := openURL(url); err != nil {
				logger.Printf("could not open the browser automatically: %v", err)
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		logger.Fatalf("server error: %v", err)
	case <-ctx.Done():
		logger.Printf("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Close()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Printf("shutdown: %v", err)
	}
}

// defaultHistoryPath keeps recorded entries next to the user's other logs.
func defaultHistoryPath() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "Library", "Logs", "oslog-live", "session.ndjson")
}

// webAssets returns the filesystem backing the UI: a directory when -web is set
// (handy while iterating on the front end) or the copy embedded in the binary.
func webAssets(dir string) (fs.FS, error) {
	if dir == "" {
		return fs.Sub(embeddedWeb, "web")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	return os.DirFS(dir), nil
}

// displayAddr makes wildcard listen addresses clickable in the terminal.
func displayAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "localhost"
	}
	return net.JoinHostPort(host, port)
}

func openURL(url string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", url).Start()
	}
	return fmt.Errorf("automatic browser opening is not supported on %s", runtime.GOOS)
}
