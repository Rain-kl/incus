package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

// PayIncus: keep complete setup diagnostics in terminal scrollback and the session log.
type operationLog struct {
	mu      sync.Mutex
	writer  io.Writer
	emit    func(string)
	path    string
	secrets []string
	err     error
}

type operationLogKey struct{}

type diagnosticError struct {
	message string
	cause   error
}

func (e diagnosticError) Error() string { return e.message }
func (e diagnosticError) Unwrap() error { return e.cause }

func redactedDiagnostic(ctx context.Context, err error) error {
	return diagnosticError{message: contextLog(ctx).redact(err.Error()), cause: err}
}

var credentialPattern = regexp.MustCompile(`\b(?:ait_|ias_)[A-Za-z0-9_-]+`)

func contextLog(ctx context.Context) *operationLog {
	value, _ := ctx.Value(operationLogKey{}).(*operationLog)
	return value
}

func (l *operationLog) redact(text string) string {
	text = credentialPattern.ReplaceAllString(text, "[REDACTED]")
	if l == nil {
		return cleanText(text)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	for _, secret := range l.secrets {
		text = strings.ReplaceAll(text, secret, "[REDACTED]")
	}

	return cleanText(text)
}

func (l *operationLog) secret(value string) {
	if l == nil || value == "" {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	for _, existing := range l.secrets {
		if value == existing {
			return
		}
	}

	l.secrets = append(l.secrets, value)
}

func secretField(key string) bool {
	key = strings.ToLower(key)
	if key == "user.user-data" || key == "user.vendor-data" || key == "cloud-init.user-data" || key == "cloud-init.vendor-data" {
		return true
	}
	return key == "key" || key == "authorization" || strings.Contains(key, "secret") || strings.Contains(key, "password") || strings.Contains(key, "token") || strings.Contains(key, "cookie")
}

func safePayload(ctx context.Context, data []byte) string {
	l := contextLog(ctx)
	var value any
	err := json.Unmarshal(data, &value)
	if err != nil {
		return l.redact(string(data))
	}

	var scrub func(any)
	scrub = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			for key, item := range value {
				if secretField(key) {
					if text, ok := item.(string); ok {
						l.secret(text)
					}
					value[key] = "[REDACTED]"
					continue
				}

				scrub(item)
			}
		case []any:
			for _, item := range value {
				scrub(item)
			}
		}
	}
	scrub(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		return l.redact(string(data))
	}

	return l.redact(string(encoded))
}

func logOperation(ctx context.Context, format string, args ...any) {
	l := contextLog(ctx)
	if l == nil {
		return
	}

	text := l.redact(fmt.Sprintf(format, args...))
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		entry := time.Now().Format(time.RFC3339) + " " + line
		l.mu.Lock()
		if l.writer != nil {
			_, err := fmt.Fprintln(l.writer, entry)
			if err != nil {
				l.err = err
			}
		}
		writeErr := l.err
		l.mu.Unlock()
		if l.emit != nil {
			l.emit(entry)
			if writeErr != nil {
				l.emit("日志文件写入失败：" + writeErr.Error())
			}
		}
	}
}

type commandLogWriter struct {
	ctx     context.Context
	pending bytes.Buffer
}

func (w *commandLogWriter) Write(data []byte) (int, error) {
	count, _ := w.pending.Write(data)
	for {
		line, err := w.pending.ReadString('\n')
		if err != nil {
			_, _ = w.pending.WriteString(line)
			break
		}

		logOperation(w.ctx, "%s", strings.TrimSuffix(line, "\n"))
	}

	return count, nil
}

func (w *commandLogWriter) flush() {
	if w.pending.Len() > 0 {
		logOperation(w.ctx, "%s", w.pending.String())
		w.pending.Reset()
	}
}
