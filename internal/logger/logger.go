// Package logger 日志系统：slog + 文件轮转 + 异步写库 + trace_id
package logger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Options 日志配置
type Options struct {
	Level      string // debug / info / warn / error
	LogFile    string // 日志文件路径（空则不写文件）；若含 "{date}" 占位符则按天分文件，如 "logs/paygateway-{date}.log"
	MaxSizeMB  int    // 单个文件最大 MB
	MaxBackups int    // 保留文件数
	MaxAgeDays int    // 保留天数
	EnableDB   bool   // 是否异步写库（管理界面查询）
	DBWriter   DBWriter
}

const datePlaceholder = "{date}"

// 按天分文件状态：当前日期 + 对应文件，跨天时切换
var (
	dayMu     sync.Mutex
	curDay    string
	dayLogger *lumberjack.Logger
)

// DBWriter 日志落库接口（由 store 包实现，避免循环依赖）
type DBWriter interface {
	InsertLog(ctx context.Context, level, scope, message, traceID, fields string) error
}

// LogEntry 结构化日志条目（异步队列用）
type LogEntry struct {
	Level   string
	Scope   string
	Message string
	TraceID string
	Fields  string
	Time    time.Time
}

var (
	mu        sync.Mutex
	logger    *slog.Logger
	dbWriter  DBWriter
	logCh     chan LogEntry
	closeCh   chan struct{}
	once      sync.Once
	closeOnce sync.Once // Close 只允许执行一次（防双 close panic）
	enabled   bool
)

// Init 初始化全局日志
func Init(opts Options) {
	once.Do(func() {
		enabled = true
		level := parseLevel(opts.Level)

		var writers []io.Writer
		writers = append(writers, os.Stdout)

		if opts.LogFile != "" {
			writers = append(writers, newFileWriter(opts))
		}

		multi := io.MultiWriter(writers...)
		logger = slog.New(slog.NewJSONHandler(multi, &slog.HandlerOptions{
			Level: level,
		}))

		dbWriter = opts.DBWriter
		if opts.EnableDB && dbWriter != nil {
			logCh = make(chan LogEntry, 2048)
			closeCh = make(chan struct{})
			go dbWriterLoop()
		}
	})
}

// SetDBWriter 设置日志落库 writer（Init 之后调用；EnableDB 时生效）
func SetDBWriter(w DBWriter) {
	if !enabled {
		return
	}
	dbWriter = w
	if logCh == nil && w != nil {
		logCh = make(chan LogEntry, 2048)
		closeCh = make(chan struct{})
		go dbWriterLoop()
	}
}

// Close 关闭日志（优雅退出时调用，flush 队列）
// 只关闭 closeCh 通知 writer 排空队列后退出；不关闭 logCh ——
// 避免后续任何 logger 调用向已关闭 channel 发送导致 panic（向关闭 channel 发送会 panic，非阻塞 select 也拦不住）
func Close() {
	if !enabled || logCh == nil {
		return
	}
	closeOnce.Do(func() {
		close(closeCh)
	})
}

// newFileWriter 构造文件写入器。LogFile 含 "{date}" 时按天分文件，否则单文件+大小轮转。
func newFileWriter(opts Options) io.Writer {
	if !strings.Contains(opts.LogFile, datePlaceholder) {
		return &lumberjack.Logger{
			Filename:   opts.LogFile,
			MaxSize:    opts.MaxSizeMB,
			MaxBackups: opts.MaxBackups,
			MaxAge:     opts.MaxAgeDays,
			Compress:   true,
		}
	}
	return &dayFileWriter{opts: opts}
}

// dayFileWriter 按天切换文件的 io.Writer。每次 Write 检查当前日期，跨天时重建 lumberjack 指向新文件。
type dayFileWriter struct {
	opts Options
}

func (w *dayFileWriter) Write(p []byte) (int, error) {
	day := time.Now().Format("2006-01-02")
	dayMu.Lock()
	defer dayMu.Unlock()
	if dayLogger == nil || curDay != day {
		path := strings.ReplaceAll(w.opts.LogFile, datePlaceholder, day)
		dayLogger = &lumberjack.Logger{
			Filename:   path,
			MaxSize:    w.opts.MaxSizeMB,
			MaxBackups: w.opts.MaxBackups,
			MaxAge:     w.opts.MaxAgeDays,
			Compress:   true,
		}
		curDay = day
	}
	return dayLogger.Write(p)
}

// parseLevel 解析日志级别
func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// WithTrace 返回带 trace_id 的 logger（gin 中间件使用）
func WithTrace(traceID string) *slog.Logger {
	if !enabled || logger == nil {
		return slog.Default()
	}
	return logger.With("trace_id", traceID)
}

// Debug / Info / Warn / Error 全局快捷方法
func Debug(scope, msg string, args ...any) {
	if !enabled || logger == nil {
		return
	}
	entry := logEntry("debug", scope, msg, "", args)
	logger.Debug(msg, append([]any{"scope", scope}, args...)...)
	pushDB(entry)
}

func Info(scope, msg string, args ...any) {
	if !enabled || logger == nil {
		return
	}
	logger.Info(msg, append([]any{"scope", scope}, args...)...)
	pushDB(logEntry("info", scope, msg, "", args))
}

func Warn(scope, msg string, args ...any) {
	if !enabled || logger == nil {
		return
	}
	logger.Warn(msg, append([]any{"scope", scope}, args...)...)
	pushDB(logEntry("warn", scope, msg, "", args))
}

func Error(scope, msg string, args ...any) {
	if !enabled || logger == nil {
		return
	}
	logger.Error(msg, append([]any{"scope", scope}, args...)...)
	pushDB(logEntry("error", scope, msg, "", args))
}

// logEntry 构造结构化条目（trace_id 从 slog 参数中提取）
func logEntry(level, scope, msg, traceID string, args []any) LogEntry {
	fields := make(map[string]any)
	for i := 0; i+1 < len(args); i += 2 {
		if k, ok := args[i].(string); ok {
			fields[k] = args[i+1]
		}
	}
	if tid, ok := fields["trace_id"]; ok {
		traceID = fmt.Sprint(tid)
		delete(fields, "trace_id")
	}
	fieldsJSON := ""
	if len(fields) > 0 {
		b, _ := jsonMarshal(fields)
		fieldsJSON = string(b)
	}
	return LogEntry{
		Level:   level,
		Scope:   scope,
		Message: msg,
		TraceID: traceID,
		Fields:  fieldsJSON,
		Time:    time.Now(),
	}
}

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

// dbWriterLoop 异步批量写库：攒 50 条或 1s flush
func dbWriterLoop() {
	batch := make([]LogEntry, 0, 50)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 || dbWriter == nil {
			return
		}
		for _, e := range batch {
			if err := dbWriter.InsertLog(context.Background(), e.Level, e.Scope, e.Message, e.TraceID, e.Fields); err != nil {
				// 写库失败不影响主流程，降级打印
				fmt.Fprintf(os.Stderr, "[logger] db write failed: %v\n", err)
			}
		}
		batch = batch[:0]
	}

	for {
		select {
		case e, ok := <-logCh:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= 50 {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-closeCh:
			// 收到关闭信号：排空剩余日志后退出（logCh 永不关闭，避免发送方 panic）
			flush()
			for {
				select {
				case e := <-logCh:
					batch = append(batch, e)
					if len(batch) >= 50 {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func pushDB(e LogEntry) {
	if logCh == nil {
		return
	}
	select {
	case logCh <- e:
	default:
		// 队列满，丢弃（保主流程）
	}
}
