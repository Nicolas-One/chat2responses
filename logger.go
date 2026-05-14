package main

import (
	"io"
	"log"
	"os"
	"sync"
)

var (
	logFile    *os.File
	logMu      sync.Mutex
	loggingOn  bool
)

// initLogging 初始化日志输出。
// 当 enabled=true 时，日志同时写入文件 chat2responses.log 和控制台（调试模式）。
// 当 enabled=false 时，仅调试模式下输出到控制台，否则静默。
func initLogging(enabled bool) {
	logMu.Lock()
	defer logMu.Unlock()

	// 关闭已有日志文件
	if logFile != nil {
		logFile.Close()
		logFile = nil
	}

	loggingOn = enabled

	if enabled {
		f, err := os.OpenFile("chat2responses.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			logFile = f
			if debugMode {
				log.SetOutput(io.MultiWriter(f, os.Stderr))
			} else {
				log.SetOutput(f)
			}
			log.Printf("[日志] 日志记录已开启，写入 chat2responses.log")
			return
		}
		log.Printf("[日志] 无法创建日志文件: %v", err)
	}

	// 日志关闭时：调试模式输出到控制台，否则静默
	if debugMode {
		log.SetOutput(os.Stderr)
	} else {
		log.SetOutput(io.Discard)
	}
}
