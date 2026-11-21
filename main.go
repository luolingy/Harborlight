package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"harborlight/internal/auth"
	"harborlight/internal/config"
	"harborlight/internal/dockerx"
	"harborlight/internal/server"
	"harborlight/internal/store"
)

func main() {
	cfg := config.Load()

	// 数据目录
	for _, dir := range []string{"data", cfg.DataDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("创建数据目录 %s 失败: %v", dir, err)
		}
	}

	// 元数据库：SQLite（默认）/ PostgreSQL
	gdb, err := store.Open(cfg.DBURL)
	if err != nil {
		log.Fatalf("初始化元数据库失败: %v", err)
	}
	dbw := store.Wrap(gdb)

	// Redis（可选）：会话存储
	var rdb *redis.Client
	if cfg.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			log.Fatalf("HL_REDIS_URL 格式错误: %v", err)
		}
		rdb = redis.NewClient(opt)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := rdb.Ping(ctx).Err(); err != nil {
			log.Fatalf("Redis 连接失败: %v", err)
		}
		log.Printf("✓ Redis 会话存储已启用: %s", cfg.RedisURL)
	}

	// Docker
	dk, err := dockerx.New(cfg.DataDir)
	if err != nil {
		log.Fatalf("连接 Docker Engine 失败: %v", err)
	}
	defer dk.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := dk.Ping(ctx); err != nil {
		log.Printf("⚠ Docker Engine 暂不可达（面板仍可启动，实例操作将报错）: %v", err)
	} else {
		log.Printf("✓ Docker Engine 已连接")
	}

	// 认证
	authSvc := auth.New(cfg, rdb)
	switch {
	case authSvc.OAuthEnabled():
		log.Printf("✓ OAuth 登录已启用 (provider=%s, redirect=%s)", cfg.Provider, cfg.OAuthRedirectURL())
	case cfg.DemoLogin:
		log.Printf("✓ 演示登录已启用 (HL_DEMO_LOGIN=true)")
	default:
		log.Printf("⚠ 未配置 OAuth 且未启用演示登录，将无法登录")
	}

	srv, err := server.New(cfg, authSvc, dk, dbw)
	if err != nil {
		log.Fatalf("初始化服务失败: %v", err)
	}

	log.Printf("✓ Harborlight 已启动: http://localhost%s  (数据库: %s)", cfg.Addr, cfg.DBSummary())
	if err := http.ListenAndServe(cfg.Addr, srv.Router()); err != nil {
		log.Fatal(err)
	}
}
