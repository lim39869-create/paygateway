// Package main Cloudflare Tunnel 管理工具
//
// 启动 trycloudflare 隧道到本地端口，输出回调地址。
// 用法:
//
//	go run ./cmd/tunnel/                    # 启动隧道到 8080
//	go run ./cmd/tunnel/ --port 8080        # 指定端口
//	go run ./cmd/tunnel/ --daemon           # 后台运行，域名写入文件
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"paygateway/internal/tunnel"
)

func main() {
	port := flag.Int("port", 8080, "本地服务端口")
	daemon := flag.Bool("daemon", false, "后台运行，域名写入 tunnel_domain.txt")
	flag.Parse()

	fmt.Println("╔" + repeat("═", 58) + "╗")
	fmt.Println("║  Cloudflare Tunnel 隧道工具                        ║")
	fmt.Println("╚" + repeat("═", 58) + "╝")
	fmt.Printf("\n[启动] 隧道 -> localhost:%d\n", *port)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tun, err := tunnel.New(ctx, &tunnel.Config{
		LocalPort:    *port,
		StartTimeout: 30 * time.Second,
	})
	if err != nil {
		fmt.Printf("\n[!] 隧道启动失败: %v\n", err)
		fmt.Println("\n请确保已安装 cloudflared:")
		fmt.Println("  curl -L https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64 -o /usr/local/bin/cloudflared && chmod +x /usr/local/bin/cloudflared")
		os.Exit(1)
	}

	fmt.Printf("\n  🎉 隧道就绪!\n")
	fmt.Printf("  公网地址: %s\n", tun.URL())
	fmt.Printf("  回调地址: %s/alipay/notify\n", tun.URL())
	fmt.Printf("  本地端口: localhost:%d\n", *port)

	if *daemon {
		domain := tun.URL()
		if domain != "" {
			_ = os.WriteFile("tunnel_domain.txt", []byte(domain+"\n"), 0644)
			_ = os.WriteFile("tunnel_callback.txt", []byte(domain+"/alipay/notify\n"), 0644)
			fmt.Printf("\n[✓] 域名已保存到 tunnel_domain.txt\n")
			fmt.Printf("[✓] 回调地址已保存到 tunnel_callback.txt\n")
		}
		fmt.Println("\n[完成] 隧道已在后台运行")
		return
	}

	// 前台运行：启动健康检查
	go tun.HealthLoop(context.Background(), 30*time.Second)

	fmt.Println("\n[提示] 隧道会一直运行直到 Ctrl+C")
	fmt.Println("按 Ctrl+C 停止隧道")

	// 等待信号
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	fmt.Println("\n\n[停止] 正在关闭隧道...")
	tun.Stop()
	fmt.Println("[完成]")
}

func repeat(s string, n int) string {
	r := make([]byte, n)
	for i := range r {
		r[i] = s[0]
	}
	return string(r)
}
