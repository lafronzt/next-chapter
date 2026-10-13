package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/lafronzt/next-chapter/internal/app"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	addr := env("LISTEN_ADDR", ":8000")
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Println("Next Chapter " + app.Version + " (Go)")
			return nil
		case "healthcheck":
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return err
			}
			client := http.Client{Timeout: 3 * time.Second}
			resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return fmt.Errorf("healthcheck: %s", resp.Status)
			}
			return nil
		default:
			return fmt.Errorf("usage: next-chapter [version|healthcheck]")
		}
	}
	demo := os.Getenv("LOCAL_DEMO") == "1"
	if demo {
		addr = env("LISTEN_ADDR", "127.0.0.1:8000")
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("LOCAL_DEMO requires a loopback LISTEN_ADDR")
		}
	}
	hops, err := strconv.Atoi(env("TRUSTED_PROXY_HOPS", "0"))
	if err != nil || hops < 0 || hops > 10 {
		return fmt.Errorf("TRUSTED_PROXY_HOPS must be a whole number from 0 to 10")
	}
	a, err := app.New(app.Config{Database: env("DATABASE_PATH", "data/next-chapter.sqlite3"), Password: os.Getenv("APP_PASSWORD"), Secret: os.Getenv("APP_SECRET_KEY"), Demo: demo, SecureCookie: os.Getenv("COOKIE_SECURE") == "1", TrustedProxyHops: hops})
	if err != nil {
		return err
	}
	defer a.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pollingDone := make(chan struct{})
	if env("START_POLLER", "1") == "1" {
		go func() { defer close(pollingDone); a.Poll(ctx) }()
	} else {
		close(pollingDone)
	}
	server := &http.Server{Addr: addr, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		timeout, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.Shutdown(timeout); err != nil {
			log.Print(err)
			server.Close()
		}
	}()
	log.Printf("Next Chapter listening on %s", addr)
	err = server.ListenAndServe()
	stop()
	<-shutdownDone
	<-pollingDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
