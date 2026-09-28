package logger

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

var (
	// Log is the global logger instance
	Log *logrus.Logger
	// logWriter rotates the file at the local calendar-day boundary.
	logWriter *dailyFileWriter
)

var (
	telegramTokenPattern = regexp.MustCompile(`(?i)(/bot)[0-9]+:[A-Za-z0-9_-]+`)
	signaturePattern     = regexp.MustCompile(`(?i)([?&]signature=)[^&\s"']+`)
)

func sanitizeLogMessage(message string) string {
	message = telegramTokenPattern.ReplaceAllString(message, `${1}<redacted>`)
	return signaturePattern.ReplaceAllString(message, `${1}<redacted>`)
}

type dailyFileWriter struct {
	mu          sync.Mutex
	dir         string
	currentDate string
	file        *os.File
	now         func() time.Time
}

func newDailyFileWriter(dir string) (*dailyFileWriter, error) {
	w := &dailyFileWriter{dir: dir, now: time.Now}
	if err := w.rotateLocked(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *dailyFileWriter) rotateLocked() error {
	date := w.now().Format("2006-01-02")
	if w.file != nil && date == w.currentDate {
		return nil
	}
	path := filepath.Join(w.dir, fmt.Sprintf("nofx_%s.log", date))
	next, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	previous := w.file
	w.file = next
	w.currentDate = date
	if previous != nil {
		_ = previous.Close()
	}
	return nil
}

func (w *dailyFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.rotateLocked(); err != nil && w.file == nil {
		return 0, err
	}
	return w.file.Write(p)
}

func (w *dailyFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// compactFormatter is a custom formatter for cleaner log output
type compactFormatter struct {
	logrus.TextFormatter
}

func (f *compactFormatter) Format(entry *logrus.Entry) ([]byte, error) {
	level := strings.ToUpper(entry.Level.String())[0:4]
	timestamp := entry.Time.Format("01-02 15:04:05")

	// Skip frames to find actual caller (skip logrus + our wrapper functions)
	caller := ""
	for i := 3; i < 10; i++ {
		_, file, line, ok := runtime.Caller(i)
		if !ok {
			break
		}
		// Skip logrus internal and our logger.go
		if !strings.Contains(file, "logrus") && !strings.HasSuffix(file, "logger/logger.go") {
			// Get package name from path (e.g., "nofx/manager/trader_manager.go" -> "manager")
			dir := filepath.Dir(file)
			pkg := filepath.Base(dir)
			caller = fmt.Sprintf("%s/%s:%d", pkg, filepath.Base(file), line)
			break
		}
	}

	msg := fmt.Sprintf("%s [%s] %s %s\n", timestamp, level, caller, sanitizeLogMessage(entry.Message))
	return []byte(msg), nil
}

func init() {
	// Auto-initialize default logger to ensure it works before Init is called
	Log = logrus.New()
	Log.SetLevel(logrus.InfoLevel)
	Log.SetFormatter(&compactFormatter{})
	Log.SetOutput(os.Stdout)
}

// ============================================================================
// Initialization functions
// ============================================================================

// Init initializes the global logger
// If config is nil, uses default configuration (console output, info level)
func Init(cfg *Config) error {
	if logWriter != nil {
		_ = logWriter.Close()
		logWriter = nil
	}
	Log = logrus.New()

	// Use default values if no config provided
	if cfg == nil {
		cfg = &Config{Level: "info"}
	}

	// Set default values
	cfg.SetDefaults()

	// Set log level
	level, err := logrus.ParseLevel(cfg.Level)
	if err != nil {
		level = logrus.InfoLevel
	}
	Log.SetLevel(level)

	// Set compact formatter
	Log.SetFormatter(&compactFormatter{})

	// Setup log file output (write to both stdout and file)
	logDir := "data"
	if err := os.MkdirAll(logDir, 0755); err == nil {
		w, err := newDailyFileWriter(logDir)
		if err == nil {
			logWriter = w
			// Write to both stdout and file
			Log.SetOutput(io.MultiWriter(os.Stdout, w))
		} else {
			Log.SetOutput(os.Stdout)
		}
	} else {
		Log.SetOutput(os.Stdout)
	}

	Log.SetReportCaller(true)

	return nil
}

// InitWithSimpleConfig initializes logger with simplified config
// Suitable for scenarios that only need basic functionality
func InitWithSimpleConfig(level string) error {
	return Init(&Config{Level: level})
}

// Shutdown gracefully shuts down the logger
func Shutdown() {
	if logWriter != nil {
		_ = logWriter.Close()
		logWriter = nil
	}
}

// ============================================================================
// Logging functions
// ============================================================================

// WithFields creates logger entry with fields
func WithFields(fields logrus.Fields) *logrus.Entry {
	return Log.WithFields(fields)
}

// WithField creates logger entry with a single field
func WithField(key string, value interface{}) *logrus.Entry {
	return Log.WithField(key, value)
}

// add debug, info, warn
func Debug(args ...interface{}) {
	Log.Debug(args...)
}

func Info(args ...interface{}) {
	Log.Info(args...)
}

func Warn(args ...interface{}) {
	Log.Warn(args...)
}

func Debugf(format string, args ...interface{}) {
	Log.Debugf(format, args...)
}

func Infof(format string, args ...interface{}) {
	Log.Infof(format, args...)
}

func Warnf(format string, args ...interface{}) {
	Log.Warnf(format, args...)
}

func Error(args ...interface{}) {
	Log.Error(args...)
}

func Errorf(format string, args ...interface{}) {
	Log.Errorf(format, args...)
}

func Fatal(args ...interface{}) {
	Log.Fatal(args...)
}

func Fatalf(format string, args ...interface{}) {
	Log.Fatalf(format, args...)
}

func Panic(args ...interface{}) {
	Log.Panic(args...)
}

func Panicf(format string, args ...interface{}) {
	Log.Panicf(format, args...)
}

// ============================================================================
// MCP Logger adapter
// ============================================================================

// MCPLogger adapter that allows MCP package to use the global logger
// Implements mcp.Logger interface
type MCPLogger struct{}

// NewMCPLogger creates MCP log adapter
func NewMCPLogger() *MCPLogger {
	return &MCPLogger{}
}

func (l *MCPLogger) Debugf(format string, args ...any) {
	Log.Debugf(format, args...)
}

func (l *MCPLogger) Infof(format string, args ...any) {
	Log.Infof(format, args...)
}

func (l *MCPLogger) Warnf(format string, args ...any) {
	Log.Warnf(format, args...)
}

func (l *MCPLogger) Errorf(format string, args ...any) {
	Log.Errorf(format, args...)
}
