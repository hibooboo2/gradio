// Package log provides a colorized slog.Handler for terminal output.
//
// It was adapted from the nmc-engineer log handler and made self-contained so
// the gradio module does not need to depend on that module or its transitive
// dependencies.
package log

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ANSI terminal color codes
const (
	ColorReset    = "\033[0m"
	ColorRed      = "\033[31m"
	ColorGreen    = "\033[32m"
	ColorYellow   = "\033[33m"
	ColorBlue     = "\033[34m"
	ColorCyan     = "\033[36m"
	ColorGray     = "\033[90m"
	ColorOrange   = "\033[38;5;208m"
	ColorPurple   = "\033[38;5;127m"
	ColorUnknown  = "\033[38;5;144m"
	ColorNil      = "\033[0;38;5;45m"
	Strikethrough = "\033[9m"
)

type ColorHandler struct {
	w  io.Writer
	mu *sync.Mutex
	l  slog.Level
}

func NewColorHandler(w io.Writer, level slog.Level) *ColorHandler {
	return &ColorHandler{
		w:  w,
		mu: &sync.Mutex{},
		l:  level,
	}
}

// Enabled implements slog.Handler.
func (h *ColorHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.l.Level()
}

// WithAttrs and WithGroup can be implemented to support structured sub-loggers
func (h *ColorHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *ColorHandler) WithGroup(name string) slog.Handler       { return h }

var bufPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

const CtxRequestStartKey = "request_start_time"

func NewContextWithStart(ctx context.Context) context.Context {
	return context.WithValue(ctx, CtxRequestStartKey, time.Now())
}

// Handle formats and colorizes the record
func (h *ColorHandler) Handle(ctx context.Context, r slog.Record) error {
	if ShouldntLog(ctx) {
		return nil
	}

	buff := bufPool.Get().(*bytes.Buffer)

	frame, _ := runtime.CallersFrames([]uintptr{r.PC}).Next()
	frame.File = ShortenFile(frame.File)
	// 1. Colorize the Time
	startTime := ctx.Value(CtxRequestStartKey)
	if startTime != nil {
		start, ok := startTime.(time.Time)
		if ok {
			r.Add("since_start", time.Since(start))
		}
	}

	programName := ShortenFile(os.Args[0])
	if strings.Contains(os.Args[0], "go-build") {
		programName = "demo_main"
	}

	timeStr := r.Time.Format("15:04:05.000")
	fmt.Fprintf(buff, "%s[%s]%s%s[%s%s%s:%d%s%s]%s%s%s%s ", ColorCyan, programName, ColorReset, ColorGreen, ColorReset, ColorYellow, frame.File, frame.Line, ColorReset, ColorGreen, ColorReset, ColorGray, timeStr, ColorReset)

	// 2. Colorize the Level
	var levelColor string
	switch r.Level {
	case slog.LevelDebug:
		levelColor = ColorBlue
	case slog.LevelInfo:
		levelColor = ColorGreen
	case slog.LevelWarn:
		levelColor = ColorYellow
	case slog.LevelError:
		levelColor = ColorRed
	default:
		levelColor = ColorReset
	}
	fmt.Fprintf(buff, "%s%-5s%s ", levelColor, r.Level.String(), ColorReset)

	// 3. Print the Main Message
	fmt.Fprintf(buff, "%s", r.Message)

	// 4. Loop through and colorize EVERY key
	r.Attrs(logAttribute(buff, h.l))

	h.mu.Lock()
	fmt.Fprintln(h.w, buff.String())
	h.mu.Unlock()
	buff.Reset()
	bufPool.Put(buff)
	return nil
}

var (
	registeredTypes = map[reflect.Type]AttributeHandler{}
	typeLock        sync.Mutex
)

type AttributeHandler func(w io.Writer, a slog.Attr) bool

type Loggable interface {
	Log(w io.Writer, key string) bool
}

func Register[Val any](h AttributeHandler) {
	var v Val
	t := reflect.TypeOf(v)
	typeLock.Lock()
	registeredTypes[t] = h
	typeLock.Unlock()
}

func logAttribute(buff *bytes.Buffer, level slog.Level) func(a slog.Attr) bool {
	return func(a slog.Attr) bool {
		v := a.Value.Any()
		// Key is styled with Cyan, Value stays default text color
		loggable, ok := v.(Loggable)
		if ok {
			if loggable.Log(buff, a.Key) {
				return true
			}
		}

		typeLock.Lock()
		t := reflect.TypeOf(v)
		h, ok := registeredTypes[t]
		typeLock.Unlock()
		if ok {
			if h(buff, a) {
				return true
			}
		}

		colorToUse := ColorGray

		switch val := v.(type) {
		case map[string]any:
			for key, val := range val {
				logAttribute(buff, level)(slog.Any(key, val))
			}
			return true
		case []byte:
			v = string(val)
		case net.Addr, *net.TCPAddr, *net.IPAddr:
			colorToUse = ColorOrange
		case string:
			if IsURL(val) {
				colorToUse = ColorOrange
			} else if isFile(val) || isDir(val) {
				v = ShortenFile(val)
				colorToUse = ColorYellow
			}
		case int, int64, int32, int16, uint, uint64, uint32, uint16, float32, float64:
			colorToUse = ColorYellow
		case bool:
			if val {
				v = "✔️"
			} else {
				v = "❌"
			}
		case error:
			colorToUse = ColorRed
		case nil:
			colorToUse = ColorNil
		case []fs.DirEntry:
			fileNames := make([]string, 0, len(val))
			for _, v := range val {
				fileNames = append(fileNames, v.Name())
			}
			logAttribute(buff, level)(slog.Any(a.Key, fileNames))
			return true
		case []string:
			colorToUse = ColorYellow
		case fmt.Stringer:
			v = val.String()
			colorToUse = ColorGreen
		default:
			if level == slog.LevelDebug {
				data, err := json.MarshalIndent(v, "", "\t")
				if err == nil {
					buff.WriteString(fmt.Sprintf("\n\n%s\n%s%s%s = %s%T%s\n", string(data), ColorCyan, a.Key, ColorReset, ColorYellow, v, ColorReset))
				}
			}
			colorToUse = ColorUnknown
		}

		fmt.Fprintf(buff, " %s%s%s%s = %s%s%v%s ", ColorCyan, a.Key, ColorReset, ColorGray, ColorReset, colorToUse, v, ColorReset)
		return true
	}
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		// os.IsNotExist returns true if the file definitely does not exist
		return false
	}
	// Return true only if it's not a directory
	return !info.IsDir()
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		// os.IsNotExist returns true if the file definitely does not exist
		return false
	}
	// Return true only if it's not a directory
	return info.IsDir()
}

func IsURL(str string) bool {
	str = strings.TrimSpace(str)
	if str == "" {
		return false
	}

	if strings.Count(str, ".") < 2 {
		return false
	}

	// 1. If it doesn't have a scheme, temporarily add one to trick the parser
	if !strings.Contains(str, "://") && !strings.HasPrefix(str, "mailto:") {
		str = "http://" + str
	}

	u, err := url.Parse(str)
	if err != nil {
		return false
	}

	// 2. Extract the host (strips ports like :8080 if present)
	host := u.Hostname()
	if host == "" {
		return false
	}

	// 3. Ensure the host is either a valid IP or a valid domain name containing a dot
	if ip := net.ParseIP(host); ip != nil {
		return true // Valid IPv4 or IPv6 address
	}

	// Domain names must contain at least one dot (e.g., "example.com", not just "localhost")
	// and cannot end with a dot unless it's a fully qualified domain name.
	dotIdx := strings.Index(host, ".")
	return dotIdx > 0 && dotIdx < len(host)-1
}

func Init() {
	Register[time.Duration](handleDuration)
	Register[*time.Duration](handleDuration)

	level := slog.LevelDebug
	switch strings.ToUpper(os.Getenv("LOG_LEVEL")) {
	case "DEBUG":
		level = slog.LevelDebug
	case "INFO":
		level = slog.LevelInfo
	case "WARN":
		level = slog.LevelWarn
	case "ERROR":
		level = slog.LevelError
	}

	logger := slog.New(NewColorHandler(os.Stdout, level))

	slog.SetDefault(logger)
}

func handleDuration(w io.Writer, a slog.Attr) bool {
	v, ok := a.Value.Any().(time.Duration)
	if !ok {
		var val *time.Duration
		val, ok = a.Value.Any().(*time.Duration)
		if ok {
			v = *val
		}
	}
	if !ok {
		return false
	}

	fmt.Fprintf(w, " %s%s%s = %s%s%s", ColorGreen, a.Key, ColorReset, ColorYellow, v.Truncate(time.Second), ColorReset)
	return true
}

const dontLog = `dont_log_things`

func DontLog(ctx context.Context) context.Context {
	return context.WithValue(ctx, dontLog, true)
}

func ShouldntLog(ctx context.Context) bool {
	val := ctx.Value(dontLog)
	if val == nil {
		return false
	}
	return val.(bool)
}

// GetProjectDir returns the directory containing the nearest .git ancestor,
// used to shorten source paths.
func GetProjectDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf(".git not found in any parent directory")
		}
		dir = parent
	}
}

// ShortenFile shortens a source file path relative to the project root, or to
// its basename directory otherwise.
func ShortenFile(fname string) string {
	dir, err := GetProjectDir()
	if err == nil && strings.HasPrefix(fname, dir) {
		return "." + fname[len(dir):]
	}

	gHub := "github.com/"
	idxGithub := strings.Index(fname, gHub)
	if idxGithub != -1 {
		fname = fname[idxGithub:]
		return fname
	}

	return filepath.Base(filepath.Dir(fname)) + "/" + filepath.Base(fname)
}
