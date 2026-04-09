package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	pkgerr "github.com/pkg/errors"

	"gopkg.in/natefinch/lumberjack.v2"

	build "boot.dev/linko/internal/build"
	"boot.dev/linko/internal/linkoerr"
	"boot.dev/linko/internal/store"
)

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}

type multiError interface {
	error
	Unwrap() []error
}

func main() {

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	logger, cleanup, err := initializeLogger()
	if err != nil {
		slog.Error(fmt.Sprintf("Logger failed to initialize: %d", err))
		os.Exit(1)
	}
	host, err := os.Hostname()
	if err != nil {
		logger.Error("failed to get hostname", "message", err)
	}
	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", os.Getenv("ENV")),
		slog.String("hostname", host),
	)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir, logger)
	cancel()
	cleanup()
	os.Exit(status)
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string, log *slog.Logger) int {
	st, err := store.New(dataDir, log)
	if err != nil {
		log.Error(fmt.Sprintf("failed to create store: %v\n", err))
		return 1
	}
	s := newServer(*st, httpPort, cancel, log)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		log.Error(fmt.Sprintf("failed to shutdown server: %v\n", err))
		return 1
	}
	if serverErr != nil {
		log.Error(fmt.Sprintf("server error: %v\n", serverErr))
		return 1
	}
	return 0
}

func initializeLogger() (*slog.Logger, func(), error) {
	closeFunc := func() {}
	nocolor := true
	if isatty.IsCygwinTerminal(os.Stderr.Fd()) || isatty.IsTerminal(os.Stderr.Fd()) {
		nocolor = false
	}

	handlers := []slog.Handler{tint.NewHandler(os.Stderr, &tint.Options{
		Level:       slog.LevelDebug,
		ReplaceAttr: replaceAttr,
		NoColor:     nocolor,
	})}

	f := os.Getenv("LINKO_LOG_FILE")
	if f != "" {
		logger := &lumberjack.Logger{
			Filename:   f,
			MaxSize:    1,
			MaxAge:     28,
			MaxBackups: 10,
			LocalTime:  false,
			Compress:   true,
		}
		closeFunc = func() {
			logger.Close()
		}
		handlers = append(handlers, slog.NewJSONHandler(logger, &slog.HandlerOptions{
			ReplaceAttr: replaceAttr,
		}))
	}

	l := slog.New(slog.NewMultiHandler(
		handlers...,
	))

	return l, closeFunc, nil
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	a = checkSensitive(groups, a)
	k := a.Key
	if k == "error" {
		err, ok := a.Value.Any().(error)
		if ok {
			if multiError, ok := errors.AsType[multiError](err); ok {
				var meAttrs []slog.Attr
				for i, me := range multiError.Unwrap() {
					meAttrs = append(meAttrs, slog.GroupAttrs(fmt.Sprintf("error_%d", i+1), errAttrs(me)...))
				}
				return slog.GroupAttrs("errors", meAttrs...)
			}
			attrs := errAttrs(err)
			return slog.GroupAttrs("error", attrs...)
		}
	}
	return a
}

var sensitive = map[string](struct{}){
	"password":     struct{}{},
	"key":          struct{}{},
	"apikey":       struct{}{},
	"secret":       struct{}{},
	"pin":          struct{}{},
	"creditcardno": struct{}{},
	"user":         struct{}{},
}

func checkSensitive(groups []string, a slog.Attr) slog.Attr {
	k := a.Key
	_, ok := sensitive[k]
	if ok {
		a = slog.String(a.Key, "[REDACTED]")
	}
	u := a.Value
	parsed, err := url.Parse(u.String())
	if err != nil {
		return a
	}
	if parsed.User != nil {
		if _, set := parsed.User.Password(); set {
			parsed.User = url.UserPassword(parsed.User.Username(), "[REDACTED]")
			a = slog.String(a.Key, parsed.String())
		}
	}
	return a
}

func errAttrs(err error) []slog.Attr {
	attrs := []slog.Attr{slog.Attr{
		Key:   "message",
		Value: slog.StringValue(err.Error()),
	}}
	if stackErr, ok := errors.AsType[stackTracer](err); ok {
		attrs = append(attrs, slog.Attr{
			Key:   "stack_trace",
			Value: slog.StringValue(fmt.Sprintf("%+v", stackErr.StackTrace())),
		})
	}
	attrs = append(attrs, linkoerr.Attrs(err)...)
	return attrs
}
