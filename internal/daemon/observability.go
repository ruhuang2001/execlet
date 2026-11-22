package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"uuid"
)

type requestLoggerKey struct{}

func contextLogger(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(requestLoggerKey{}).(*slog.Logger); ok {
		return logger
	}
	return slog.Default()
}

func requestID(values []string) string {
	if len(values) == 1 && len(values[0]) > 0 && len(values[0]) <= 128 {
		valid := true
		for _, c := range values[0] {
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
			default:
				valid = false
			}
			if !valid {
				break
			}
		}
		if valid {
			return values[0]
		}
	}
	return uuid.New().String()
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *responseRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	if status >= 200 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type flushingResponse struct{ *responseRecorder }

func (w *flushingResponse) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	w.ResponseWriter.(http.Flusher).Flush()
}
