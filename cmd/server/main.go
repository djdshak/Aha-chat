package main

import (
	"context"
	"log"
	"net/http"
	"time"
)

func main() {
	addr := getenv("LISTEN_ADDR", "127.0.0.1:8080")

	// 1) 核心组件
	hub := newHub()
	go hub.run()

	// 2) 路由
	mux := http.NewServeMux()
	registerRoutes(mux, hub)

	// 3) HTTP Server
	srv := &http.Server{
		Addr:              addr,
		Handler:           loggingMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// 4) 启动监听（后台 goroutine）
	go func() {
		log.Println("listening on", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("ListenAndServe:", err)
		}
	}()

	// 5) 等待退出信号
	waitForSignal()
	log.Println("shutting down...")

	// 6) 优雅关闭
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = srv.Shutdown(ctx)

	log.Println("bye")
}
