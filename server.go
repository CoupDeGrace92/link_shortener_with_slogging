package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"time"

	"boot.dev/linko/internal/store"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type server struct {
	httpServer *http.Server
	store      store.Store
	cancel     context.CancelFunc
	logger     *slog.Logger
}

type spyReadCloser struct {
	io.ReadCloser
	bytesRead int
}

func (r *spyReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytesRead += n
	return n, err
}

type spyResponseWriter struct {
	http.ResponseWriter
	bytesWritten int
	statusCode   int
}

func (w *spyResponseWriter) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytesWritten += n
	return n, err
}

func (w *spyResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

const logContextKey contextKey = "log_context"

type LogContext struct {
	Username string
	Error    error
}

func httpError(ctx context.Context, w http.ResponseWriter, status int, err error) {
	if logCtx, ok := ctx.Value(logContextKey).(*LogContext); ok {
		logCtx.Error = err
	}
	if status == 401 || status == 403 || status == 500 {
		http.Error(w, http.StatusText(status), status)
	} else {
		http.Error(w, err.Error(), status)
	}
}

func IDChecker() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ID := r.Header.Get("X-Request-ID")
			if ID == "" {
				ID = rand.Text()
			}
			w.Header().Set("X-Request-ID", ID)
			next.ServeHTTP(w, r)
		})
	}
}

func redactIP(ip string) string {
	host, _, err := net.SplitHostPort(ip)
	if err != nil {
		host = ip
	}
	parsedIP := net.ParseIP(host)
	if parsedIP == nil {
		return host
	}
	ip4 := parsedIP.To4()
	if ip4 == nil {
		return host
	}
	return fmt.Sprintf("%d.%d.%d.x", ip4[0], ip4[1], ip4[2])
}

func requestLogger(l *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			spyReader := &spyReadCloser{ReadCloser: r.Body}
			r.Body = spyReader
			spyWriter := &spyResponseWriter{ResponseWriter: w}
			contextLog := &LogContext{}
			rContext := r.WithContext(context.WithValue(r.Context(), logContextKey, contextLog))
			next.ServeHTTP(spyWriter, rContext)
			attrs := []any{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("client_ip", redactIP(r.RemoteAddr)),
				slog.Duration("duration", time.Since(start)),
				slog.Int("request_body_bytes", spyReader.bytesRead),
				slog.Int("response_status", spyWriter.statusCode),
				slog.Int("response_body_bytes", spyWriter.bytesWritten),
				slog.String("request_id", w.Header().Get("X-Request-ID")),
			}
			if contextLog.Username != "" {
				attrs = append(attrs, slog.String("user", contextLog.Username))
			}
			if contextLog.Error != nil {
				attrs = append(attrs, slog.Any("error", contextLog.Error))
			}
			l.Info("Served request", attrs...)
		})
	}
}

func newServer(store store.Store, port int, cancel context.CancelFunc, l *slog.Logger) *server {

	mux := http.NewServeMux()
	tracingMux := otelhttp.NewHandler(mux, "http.Server")
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: IDChecker()(requestLogger(l)(metricsMiddleware(tracingMux))),
	}

	s := &server{
		httpServer: srv,
		store:      store,
		cancel:     cancel,
		logger:     l,
	}

	mux.HandleFunc("GET /", s.handlerIndex)
	//If we wanted to wrap this individually instead of wrapping the mux in the logger:
	// mux.Handle("GET /", requestLogger(logger)(http.HandleFunc(s.handlerIndex)))
	mux.Handle("POST /api/login", s.authMiddleware(http.HandlerFunc(s.handlerLogin)))
	// mux.Handle("POST /api/login", requestLogger(logger)(s.authMiddleware(http.HandlerFunc(s.handlerLogin))))
	mux.Handle("POST /api/shorten", s.authMiddleware(http.HandlerFunc(s.handlerShortenLink)))
	mux.Handle("GET /api/stats", s.authMiddleware(http.HandlerFunc(s.handlerStats)))
	mux.Handle("GET /api/urls", s.authMiddleware(http.HandlerFunc(s.handlerListURLs)))
	mux.HandleFunc("GET /{shortCode}", s.handlerRedirect)
	mux.HandleFunc("POST /admin/shutdown", s.handlerShutdown)
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.Handle("GET /debug/pprof/", s.authMiddleware(http.HandlerFunc(pprof.Index)))
	mux.Handle("GET /debug/pprof/profile", s.authMiddleware(http.HandlerFunc(pprof.Profile)))

	return s
}

func (s *server) start() error {
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return err
	}
	s.logger.Debug("Linko is running", slog.String("address", fmt.Sprintf("http://localhost:%v", ln.Addr().(*net.TCPAddr).Port)))
	if err := s.httpServer.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *server) shutdown(ctx context.Context) error {
	s.logger.Debug("Linko is shutting down")
	return s.httpServer.Shutdown(ctx)
}

func (s *server) handlerShutdown(w http.ResponseWriter, r *http.Request) {
	if os.Getenv("ENV") == "production" {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
	go s.cancel()
}
