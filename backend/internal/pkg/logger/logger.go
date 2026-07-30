package logger

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

type Level = zapcore.Level

const (
	LevelDebug = zapcore.DebugLevel
	LevelInfo  = zapcore.InfoLevel
	LevelWarn  = zapcore.WarnLevel
	LevelError = zapcore.ErrorLevel
	LevelFatal = zapcore.FatalLevel

	// OpsSystemLogSkipField keeps an event in the standard logger while
	// preventing the database-backed Ops system-log sink from indexing it.
	OpsSystemLogSkipField = "ops_system_log_skip"

	// LegacyPrintfField 标记「printf 风格 且 未接 ctx」的日志行，由 LegacyPrintf 打上。
	// #103 拿 count(*) WHERE legacy_printf = true 当迁移进度度量，所以它必须严格等于
	// 「还没迁移」——已迁移的 printf 行走 CtxPrintfField，两者互斥。
	LegacyPrintfField = "legacy_printf"

	// CtxPrintfField 标记「printf 风格 但已接 ctx」的日志行，由 CtxPrintf 打上。
	// 这些行已经带 trace_id，剩下的欠账只是把 printf 拆成结构化字段。
	CtxPrintfField = "ctx_printf"
)

type Sink interface {
	WriteLogEvent(event *LogEvent)
}

type LogEvent struct {
	Time       time.Time
	Level      string
	Component  string
	Message    string
	LoggerName string
	Fields     map[string]any
}

// sharedStdoutSyncer / sharedStderrSyncer are created once for the process, not
// per Init: zapcore.Lock returns a fresh mutex each call, so two wrappers over
// the same fd would not serialize against each other. Every writer that targets
// stdout (the main logger and the business-event emitter) must go through these
// so partial writes cannot interleave into a broken JSON line — Docker's
// json-file driver splits on newlines, and a spliced line reaches Vector as
// unparseable JSON.
var (
	sharedStdoutSyncer = &lockedStreamSyncer{stream: func() *os.File { return os.Stdout }}
	sharedStderrSyncer = &lockedStreamSyncer{stream: func() *os.File { return os.Stderr }}
)

// lockedStreamSyncer serializes writes to one standard stream. The target is
// resolved per write rather than captured once, so tests that swap os.Stdout
// still observe output while every writer keeps sharing a single mutex.
type lockedStreamSyncer struct {
	mu     sync.Mutex
	stream func() *os.File
}

func (s *lockedStreamSyncer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream().Write(p)
}

func (s *lockedStreamSyncer) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream().Sync()
}

var (
	mu            sync.RWMutex
	global        atomic.Pointer[zap.Logger]
	sugar         atomic.Pointer[zap.SugaredLogger]
	atomicLevel   zap.AtomicLevel
	initOptions   InitOptions
	currentSink   atomic.Value // sinkState
	stdLogUndo    func()
	bootstrapOnce sync.Once
)

type sinkState struct {
	sink Sink
}

func InitBootstrap() {
	bootstrapOnce.Do(func() {
		if err := Init(bootstrapOptions()); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "logger bootstrap init failed: %v\n", err)
		}
	})
}

func Init(options InitOptions) error {
	mu.Lock()
	defer mu.Unlock()
	return initLocked(options)
}

func initLocked(options InitOptions) error {
	normalized := options.normalized()
	zl, al, err := buildLogger(normalized)
	if err != nil {
		return err
	}

	prev := global.Load()
	global.Store(zl)
	sugar.Store(zl.Sugar())
	atomicLevel = al
	initOptions = normalized

	configureBusinessEventLogger(normalized.ServiceName)

	bridgeSlogLocked()
	bridgeStdLogLocked()

	if prev != nil {
		_ = prev.Sync()
	}
	return nil
}

func Reconfigure(mutator func(*InitOptions) error) error {
	mu.Lock()
	defer mu.Unlock()
	next := initOptions
	if mutator != nil {
		if err := mutator(&next); err != nil {
			return err
		}
	}
	return initLocked(next)
}

func SetLevel(level string) error {
	lv, ok := parseLevel(level)
	if !ok {
		return fmt.Errorf("invalid log level: %s", level)
	}

	mu.Lock()
	defer mu.Unlock()
	atomicLevel.SetLevel(lv)
	initOptions.Level = strings.ToLower(strings.TrimSpace(level))
	return nil
}

func CurrentLevel() string {
	mu.RLock()
	defer mu.RUnlock()
	if global.Load() == nil {
		return "info"
	}
	return atomicLevel.Level().String()
}

func SetSink(sink Sink) {
	currentSink.Store(sinkState{sink: sink})
}

func loadSink() Sink {
	v := currentSink.Load()
	if v == nil {
		return nil
	}
	state, ok := v.(sinkState)
	if !ok {
		return nil
	}
	return state.sink
}

// WriteSinkEvent 直接写入日志 sink，不经过全局日志级别门控。
// 用于需要“可观测性入库”与“业务输出级别”解耦的场景（例如 ops 系统日志索引）。
func WriteSinkEvent(level, component, message string, fields map[string]any) {
	sink := loadSink()
	if sink == nil {
		return
	}

	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" {
		level = "info"
	}
	component = strings.TrimSpace(component)
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}

	eventFields := make(map[string]any, len(fields)+1)
	for k, v := range fields {
		eventFields[k] = v
	}
	if component != "" {
		if _, ok := eventFields["component"]; !ok {
			eventFields["component"] = component
		}
	}

	sink.WriteLogEvent(&LogEvent{
		Time:       time.Now(),
		Level:      level,
		Component:  component,
		Message:    message,
		LoggerName: component,
		Fields:     eventFields,
	})
}

func L() *zap.Logger {
	if l := global.Load(); l != nil {
		return l
	}
	return zap.NewNop()
}

func S() *zap.SugaredLogger {
	if s := sugar.Load(); s != nil {
		return s
	}
	return zap.NewNop().Sugar()
}

func With(fields ...zap.Field) *zap.Logger {
	return L().With(fields...)
}

func Sync() {
	l := global.Load()
	if l != nil {
		_ = l.Sync()
	}
}

func bridgeStdLogLocked() {
	if stdLogUndo != nil {
		stdLogUndo()
		stdLogUndo = nil
	}

	prevFlags := log.Flags()
	prevPrefix := log.Prefix()
	prevWriter := log.Writer()

	log.SetFlags(0)
	log.SetPrefix("")
	base := global.Load()
	if base == nil {
		base = zap.NewNop()
	}
	log.SetOutput(newStdLogBridge(base.Named("stdlog")))

	stdLogUndo = func() {
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	}
}

func bridgeSlogLocked() {
	base := global.Load()
	if base == nil {
		base = zap.NewNop()
	}
	slog.SetDefault(slog.New(newSlogZapHandler(base.Named("slog"))))
}

func buildLogger(options InitOptions) (*zap.Logger, zap.AtomicLevel, error) {
	level, _ := parseLevel(options.Level)
	atomic := zap.NewAtomicLevelAt(level)

	encoderCfg := zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.CapitalLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.MillisDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	var enc zapcore.Encoder
	if options.Format == "console" {
		enc = zapcore.NewConsoleEncoder(encoderCfg)
	} else {
		enc = zapcore.NewJSONEncoder(encoderCfg)
	}

	sinkCore := newSinkCore()
	cores := make([]zapcore.Core, 0, 3)

	if options.Output.ToStdout {
		infoPriority := zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
			return lvl >= atomic.Level() && lvl < zapcore.WarnLevel
		})
		errPriority := zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
			return lvl >= atomic.Level() && lvl >= zapcore.WarnLevel
		})
		cores = append(cores, zapcore.NewCore(enc, sharedStdoutSyncer, infoPriority))
		cores = append(cores, zapcore.NewCore(enc, sharedStderrSyncer, errPriority))
	}

	if options.Output.ToFile {
		fileCore, filePath, fileErr := buildFileCore(enc, atomic, options)
		if fileErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "time=%s level=WARN msg=\"日志文件输出初始化失败，降级为仅标准输出\" path=%s err=%v\n",
				time.Now().Format(time.RFC3339Nano),
				filePath,
				fileErr,
			)
		} else {
			cores = append(cores, fileCore)
		}
	}

	if len(cores) == 0 {
		cores = append(cores, zapcore.NewCore(enc, sharedStdoutSyncer, atomic))
	}

	core := zapcore.NewTee(cores...)
	if options.Sampling.Enabled {
		core = zapcore.NewSamplerWithOptions(core, samplingTick(), options.Sampling.Initial, options.Sampling.Thereafter)
	}
	core = sinkCore.Wrap(core)

	stacktraceLevel, _ := parseStacktraceLevel(options.StacktraceLevel)
	zapOpts := make([]zap.Option, 0, 5)
	if options.Caller {
		zapOpts = append(zapOpts, zap.AddCaller())
	}
	if stacktraceLevel <= zapcore.FatalLevel {
		zapOpts = append(zapOpts, zap.AddStacktrace(stacktraceLevel))
	}

	logger := zap.New(core, zapOpts...).With(
		zap.String("service", options.ServiceName),
		zap.String("env", options.Environment),
	)
	return logger, atomic, nil
}

func buildFileCore(enc zapcore.Encoder, atomic zap.AtomicLevel, options InitOptions) (zapcore.Core, string, error) {
	filePath := options.Output.FilePath
	if strings.TrimSpace(filePath) == "" {
		filePath = resolveLogFilePath("")
	}

	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, filePath, err
	}
	lj := &lumberjack.Logger{
		Filename:   filePath,
		MaxSize:    options.Rotation.MaxSizeMB,
		MaxBackups: options.Rotation.MaxBackups,
		MaxAge:     options.Rotation.MaxAgeDays,
		Compress:   options.Rotation.Compress,
		LocalTime:  options.Rotation.LocalTime,
	}
	return zapcore.NewCore(enc, zapcore.AddSync(lj), atomic), filePath, nil
}

type sinkCore struct {
	core   zapcore.Core
	fields []zapcore.Field
}

func newSinkCore() *sinkCore {
	return &sinkCore{}
}

func (s *sinkCore) Wrap(core zapcore.Core) zapcore.Core {
	cp := *s
	cp.core = core
	return &cp
}

func (s *sinkCore) Enabled(level zapcore.Level) bool {
	return s.core.Enabled(level)
}

func (s *sinkCore) With(fields []zapcore.Field) zapcore.Core {
	nextFields := append([]zapcore.Field{}, s.fields...)
	nextFields = append(nextFields, fields...)
	return &sinkCore{
		core:   s.core.With(fields),
		fields: nextFields,
	}
}

func (s *sinkCore) Check(entry zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	// Delegate to inner core (tee) so each sub-core's level enabler is respected.
	// Then add ourselves for sink forwarding only.
	ce = s.core.Check(entry, ce)
	if ce != nil {
		ce = ce.AddCore(entry, s)
	}
	return ce
}

func (s *sinkCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	// Only handle sink forwarding — the inner cores write via their own
	// Write methods (added to CheckedEntry by s.core.Check above).
	sink := loadSink()
	if sink == nil {
		return nil
	}

	enc := zapcore.NewMapObjectEncoder()
	for _, f := range s.fields {
		f.AddTo(enc)
	}
	for _, f := range fields {
		f.AddTo(enc)
	}

	event := &LogEvent{
		Time:       entry.Time,
		Level:      strings.ToLower(entry.Level.String()),
		Component:  entry.LoggerName,
		Message:    entry.Message,
		LoggerName: entry.LoggerName,
		Fields:     enc.Fields,
	}
	sink.WriteLogEvent(event)
	return nil
}

func (s *sinkCore) Sync() error {
	return s.core.Sync()
}

type stdLogBridge struct {
	logger *zap.Logger
}

func newStdLogBridge(l *zap.Logger) io.Writer {
	if l == nil {
		l = zap.NewNop()
	}
	return &stdLogBridge{logger: l}
}

func (b *stdLogBridge) Write(p []byte) (int, error) {
	msg := normalizeStdLogMessage(string(p))
	if msg == "" {
		return len(p), nil
	}

	level := inferStdLogLevel(msg)
	entry := b.logger.WithOptions(zap.AddCallerSkip(4))

	switch level {
	case LevelDebug:
		entry.Debug(msg, zap.Bool("legacy_stdlog", true))
	case LevelWarn:
		entry.Warn(msg, zap.Bool("legacy_stdlog", true))
	case LevelError, LevelFatal:
		entry.Error(msg, zap.Bool("legacy_stdlog", true))
	default:
		entry.Info(msg, zap.Bool("legacy_stdlog", true))
	}
	return len(p), nil
}

func normalizeStdLogMessage(raw string) string {
	msg := strings.TrimSpace(strings.ReplaceAll(raw, "\n", " "))
	if msg == "" {
		return ""
	}
	return strings.Join(strings.Fields(msg), " ")
}

func inferStdLogLevel(msg string) Level {
	lower := strings.ToLower(strings.TrimSpace(msg))
	if lower == "" {
		return LevelInfo
	}

	if strings.HasPrefix(lower, "[debug]") || strings.HasPrefix(lower, "debug:") {
		return LevelDebug
	}
	if strings.HasPrefix(lower, "[warn]") || strings.HasPrefix(lower, "[warning]") || strings.HasPrefix(lower, "warn:") || strings.HasPrefix(lower, "warning:") {
		return LevelWarn
	}
	if strings.HasPrefix(lower, "[error]") || strings.HasPrefix(lower, "error:") || strings.HasPrefix(lower, "fatal:") || strings.HasPrefix(lower, "panic:") {
		return LevelError
	}

	if strings.Contains(lower, " failed") || strings.Contains(lower, "error") || strings.Contains(lower, "panic") || strings.Contains(lower, "fatal") {
		return LevelError
	}
	if strings.Contains(lower, "warning") || strings.Contains(lower, "warn") || strings.Contains(lower, " queue full") || strings.Contains(lower, "fallback") {
		return LevelWarn
	}
	return LevelInfo
}

// LegacyPrintf 用于平滑迁移历史的 printf 风格日志到结构化 logger。
//
// 它用全局 L()，日志行拿不到 trace_id / request_id。签名里已有 ctx 的调用点
// 应改用 CtxPrintf(ctx, ...)——只差首个参数。LegacyPrintfField 是剩余工作量的度量口径。
func LegacyPrintf(component, format string, args ...any) {
	msg := normalizeStdLogMessage(fmt.Sprintf(format, args...))
	if msg == "" {
		return
	}

	initialized := global.Load() != nil
	if !initialized {
		// 在日志系统未初始化前，回退到标准库 log，避免测试/工具链丢日志。
		log.Print(msg)
		return
	}

	l := L()
	if component != "" {
		l = l.With(zap.String("component", component))
	}
	l = l.WithOptions(zap.AddCallerSkip(1))

	switch inferStdLogLevel(msg) {
	case LevelDebug:
		l.Debug(msg, zap.Bool(LegacyPrintfField, true))
	case LevelWarn:
		l.Warn(msg, zap.Bool(LegacyPrintfField, true))
	case LevelError, LevelFatal:
		l.Error(msg, zap.Bool(LegacyPrintfField, true))
	default:
		l.Info(msg, zap.Bool(LegacyPrintfField, true))
	}
}

// CtxPrintf 与 LegacyPrintf 行为一致，但 logger 取自 ctx，因此日志行会自动带上
// RequestLogger() 绑定的 trace_id / request_id / client_request_id / path / method。
// 迁移 printf 风格日志时，只需在原 LegacyPrintf 调用前加一个 ctx 参数。
//
// 标记字段用 ctx_printf 而非 legacy_printf：#103 把 legacy_printf 当"还没迁移"的
// 进度度量用（count(*) WHERE legacy_printf = true 应单调下降至只剩豁免项），
// 两者共用一个字段会让这个数永远降不下去。ctx_printf 则标出"已迁移但仍是 printf
// 风格"，供后续把 printf 改成结构化字段的批次继续度量。两个字段互斥。
func CtxPrintf(ctx context.Context, component, format string, args ...any) {
	msg := normalizeStdLogMessage(fmt.Sprintf(format, args...))
	if msg == "" {
		return
	}

	initialized := global.Load() != nil
	if !initialized {
		// 在日志系统未初始化前，回退到标准库 log，避免测试/工具链丢日志。
		log.Print(msg)
		return
	}

	l := FromContext(ctx)
	if component != "" {
		l = l.With(zap.String("component", component))
	}
	// 与 LegacyPrintf 同为一层包装，skip 1 让 caller 指向业务调用点。
	l = l.WithOptions(zap.AddCallerSkip(1))

	switch inferStdLogLevel(msg) {
	case LevelDebug:
		l.Debug(msg, zap.Bool(CtxPrintfField, true))
	case LevelWarn:
		l.Warn(msg, zap.Bool(CtxPrintfField, true))
	case LevelError, LevelFatal:
		l.Error(msg, zap.Bool(CtxPrintfField, true))
	default:
		l.Info(msg, zap.Bool(CtxPrintfField, true))
	}
}

type contextKey string

const loggerContextKey contextKey = "ctx_logger"

func IntoContext(ctx context.Context, l *zap.Logger) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if l == nil {
		l = L()
	}
	return context.WithValue(ctx, loggerContextKey, l)
}

// FromContext 取出 request-scoped logger。
//
// 三级兜底：
//  1. IntoContext 塞进来的 logger（RequestLogger() 中间件的正常路径）——最完整，
//     带 path / method 等只有中间件知道的字段；
//  2. 没有 logger 但 ctx 里还有 ctxkey 关联 ID 时，现场用这些 ID 组一个。
//     这一级专门兜后台/脱离请求生命周期的 ctx：像 handler.usageRecordContext 那样
//     只把 ctxkey 从请求 ctx 搬到 context.Background() 派生 ctx 的 helper，
//     搬 value 时很容易忘了 logger，没有这级兜底就会静默丢 trace_id；
//  3. 都没有则退回全局 L()。
func FromContext(ctx context.Context) *zap.Logger {
	if ctx == nil {
		return L()
	}
	if l, ok := ctx.Value(loggerContextKey).(*zap.Logger); ok && l != nil {
		return l
	}
	if fields := correlationFieldsFromContext(ctx); len(fields) > 0 {
		return L().With(fields...)
	}
	return L()
}

// correlationFieldsFromContext 把 ctx 里的关联 ID 还原成日志字段。
// 字段名与 middleware.RequestLogger() 绑的保持一致，否则同一条链路在
// OpenObserve 里会分裂成两套字段名。
func correlationFieldsFromContext(ctx context.Context) []zap.Field {
	fields := make([]zap.Field, 0, 3)
	for _, item := range []struct {
		key   ctxkey.Key
		field string
	}{
		{ctxkey.RequestID, "request_id"},
		{ctxkey.TraceID, "trace_id"},
		{ctxkey.ClientRequestID, "client_request_id"},
	} {
		if v, _ := ctx.Value(item.key).(string); strings.TrimSpace(v) != "" {
			fields = append(fields, zap.String(item.field, strings.TrimSpace(v)))
		}
	}
	return fields
}

// C 是 FromContext 的短别名。全仓有上千个调用点要迁移，名字长度直接影响采用率。
// 它只做取值，不加包装层，因此 C(ctx).Warn(...) 的 caller 仍指向业务调用点。
func C(ctx context.Context) *zap.Logger {
	return FromContext(ctx)
}
