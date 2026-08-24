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

	listeners, err := listen(cfg.ListenAddr)
	if err != nil {
		log.Error("listen failed", "addr", cfg.ListenAddr, "error", err)
		os.Exit(1)
	}
	for _, l := range listeners {
		defer l.Close()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	serveErr := make(chan error, len(listeners))
	for _, l := range listeners {
		l := l
		go func() {
			log.Info("service started", "addr", l.Addr().String(), "network", l.Addr().Network(), "timeout", cfg.CommandTimeout.String())
			if err := server.Serve(l); err != nil && err != http.ErrServerClosed {
				serveErr <- err
			}
		}()
	}
	if err := <-serveErr; err != nil {
		log.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// listen 打开 TCP 或 Unix Domain Socket 监听器。
//   - addr 以 "unix:" 为前缀时，使用 Unix Domain Socket，消除本地回源 TCP 握手与内核协议栈开销。
//   - addr 为 "127.0.0.1:PORT" 时，额外监听 IPv6 loopback "::1:PORT"。
//     cloudflared 用 "localhost" 回源时会依次尝试 IPv4/IPv6，双栈监听可避免 IPv6 探测失败导致的
//     "connection refused"，保证无论走哪种协议都能稳定回源。
func listen(addr string) ([]net.Listener, error) {
	if strings.HasPrefix(addr, "unix:") {
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
		return []net.Listener{listener}, nil
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	// 仅当显式绑定 IPv4 loopback 时，补充监听 IPv6 loopback，消除 localhost 回源的 IPv6 探测失败。
	if host == "127.0.0.1" {
		ipv6, err := net.Listen("tcp", net.JoinHostPort("::1", port))
		if err != nil {
			// IPv6 不可用时忽略，仍以 IPv4 运行。
			ipv4, err := net.Listen("tcp", addr)
			if err != nil {
				return nil, err
			}
			return []net.Listener{ipv4}, nil
		}
		ipv4, err := net.Listen("tcp", addr)
		if err != nil {
			_ = ipv6.Close()
			return nil, err
		}
		return []net.Listener{ipv4, ipv6}, nil
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return []net.Listener{listener}, nil
}
