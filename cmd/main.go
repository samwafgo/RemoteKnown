package main

import (
	"RemoteKnown/internal/detector"
	"RemoteKnown/internal/notifier"
	"RemoteKnown/internal/server"
	"RemoteKnown/internal/storage"
	"io"
	"log"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("RemoteKnown 守护进程启动...")
	/*go func() {
		// 开启一个用于调试的端口，比如 6060
		log.Println(http.ListenAndServe("0.0.0.0:6060", nil))
	}()*/
	// 获取用户配置目录 (AppData/Roaming) 以确保持久化存储
	userConfigDir, err := os.UserConfigDir()
	if err != nil {
		log.Printf("无法获取用户配置目录, 回退到当前目录: %v", err)
		userConfigDir = "."
	}

	// 确保目录存在
	appDataDir := filepath.Join(userConfigDir, "RemoteKnown")
	if err := os.MkdirAll(appDataDir, 0755); err != nil {
		log.Printf("无法创建应用数据目录: %v", err)
		appDataDir = "."
	}

	// 设置日志文件
	logFile := filepath.Join(appDataDir, "RemoteKnown.log")
	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Printf("无法打开日志文件 %s: %v，将只输出到控制台", logFile, err)
	} else {
		defer file.Close()
		// 同时输出到文件和控制台
		multiWriter := io.MultiWriter(os.Stdout, file)
		log.SetOutput(multiWriter)
		log.Printf("日志文件: %s", logFile)
	}

	dbPath := filepath.Join(appDataDir, "RemoteKnown.db")
	log.Printf("数据库路径: %s", dbPath)

	storage, err := storage.NewStorage(dbPath)
	if err != nil {
		log.Fatalf("初始化存储失败: %v", err)
	}
	defer storage.Close()

	notifier := notifier.NewNotifier(storage)
	detector := detector.NewDetector(storage, notifier)

	srv := server.NewServer(detector, storage, notifier)

	go func() {
		if err := srv.Start(); err != nil {
			log.Fatalf("服务器启动失败: %v", err)
		}
	}()

	log.Printf("RemoteKnown 服务已启动，监听端口 %d", server.DefaultPort)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("正在关闭 RemoteKnown 守护进程...")
	srv.Stop()
	log.Println("RemoteKnown 已退出")
}
