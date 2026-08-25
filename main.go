// SPDX-License-Identifier: Apache-2.0
// performance-test：IotaPanel 性能测试插件（宝塔风格一键跑分，纯 Go 零依赖）。
//
// 契约（IotaPanel 插件铁律）：
//   - 监听 $PLUGIN_BIND:$PLUGIN_PORT（默认 127.0.0.1），经面板网关 /p/performance-test/ 访问
//   - 处理 SIGTERM 优雅退出
//   - manifest bind 保持 127.0.0.1
//
// 跑分项目（参考宝塔面板服务器性能测试 / bt_score）：
//   - CPU：整数运算 + 浮点运算 + 圆周率(π 级数) + 百万次混合运算耗时
//   - 内存：大数组读写带宽(MB/s) + 分配/释放速率
//   - 磁盘：顺序读/写、随机读、4K 读/写(IOPS)，在 $PANEL_HOME/data 下进行
//   - 综合评分：CPU + 内存 + 磁盘（宝塔风格总分）
package main

import (
	"embed"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

func main() {
	// 软内存上限 64MiB（跑分时含大数组也不失控）
	debug.SetMemoryLimit(64 << 20)

	port := os.Getenv("PLUGIN_PORT")
	if port == "" {
		port = "19200"
	}
	bind := os.Getenv("PLUGIN_BIND")
	if bind == "" {
		bind = "127.0.0.1"
	}
	pluginName := os.Getenv("PLUGIN_NAME")
	if pluginName == "" {
		pluginName = "performance-test"
	}
	home := os.Getenv("PANEL_HOME")
	if home == "" {
		home = filepath.Join(os.TempDir(), "performance-test-dev")
	}

	app, err := NewApp(home, pluginName)
	if err != nil {
		log.Fatalf("[performance-test] 初始化失败: %v", err)
	}

	admin := &http.Server{
		Addr:              bind + ":" + port,
		Handler:           app.mux(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("[performance-test] 收到退出信号，优雅关闭…")
		app.cancelRun()          // 终止正在进行的跑分
		_ = admin.Close()
		time.Sleep(200 * time.Millisecond)
		log.Printf("[performance-test] 已退出")
		os.Exit(0)
	}()

	go func() {
		if err := admin.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[performance-test] 管理接口启动失败: %v", err)
		}
	}()

	log.Printf("[performance-test] 管理接口 %s:%s | 数据目录 %s", bind, port, app.dataDir)

	select {} // 常驻（keepalive 插件）
}