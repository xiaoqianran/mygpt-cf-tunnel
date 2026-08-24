package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/xiaoqianran/mygpt-cf-tunnel/internal/agent"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := agent.LoadConfig()
	if err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	handler, err := agent.New(cfg, log)
	if err != nil {
		log.Error("initialize service", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := handler.Close(); err != nil {
			log.Error("close audit sink", "error", err)
		}
	}()

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      42 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	listener, err := listen(cfg.ListenAddr)
	if err != nil {
		log.Error("listen failed", "addr", cfg.ListenAddr, "error", err)
		os.Exit(1)
	}
	defer listener.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Info("service started", "addr", cfg.ListenAddr, "network", listenerNetwork(cfg.ListenAddr), "timeout", cfg.CommandTimeout.String())
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// listen 打开 TCP 或 Unix Domain Socket 监听器。当 addr 以 "unix:" 为前缀时，
// 使用 Unix Domain Socket，消除本地回源 TCP 握手与内核协议栈开销，配合 cloudflared 回源。
func listen(addr string) (net.Listener, error) {
	if !strings.HasPrefix(addr, "unix:") {
		return net.Listen("tcp", addr)
	}
	sockPath := strings.TrimPrefix(addr, "unix:")
	// 清理上次可能遗留的 socket 文件，避免 "address already in use"。
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, err
	}
	// 允许 cloudflared（通常以非 root 用户运行）读写该 socket。
	if err := os.Chmod(sockPath, 0o666); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

func listenerNetwork(addr string) string {
	if strings.HasPrefix(addr, "unix:") {
		return "unix"
	}
	return "tcp"
}
