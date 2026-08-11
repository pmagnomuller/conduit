package capture

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pedro-mueller/claude-glm-gateway/internal/redact"
)

type Record struct {
	TS       time.Time         `json:"ts"`
	Provider string            `json:"provider"`
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Model    string            `json:"model,omitempty"`
	Status   int               `json:"status"`
	Headers  map[string]string `json:"headers"`
	Body     string            `json:"body"`
	Note     string            `json:"note,omitempty"`
}

type Writer struct {
	mu   sync.Mutex
	path string
	enabled bool
}

func New(path string, enabled bool) *Writer {
	return &Writer{path: path, enabled: enabled && path != ""}
}

func (w *Writer) Path() string { return w.path }

func (w *Writer) Write(rec Record) error {
	if !w.enabled {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
		return err
	}
	rec.Body = string(redact.Bytes([]byte(rec.Body)))
	for k, v := range rec.Headers {
		if redact.IsSensitiveHeader(k) {
			rec.Headers[k] = "[REDACTED]"
		} else {
			rec.Headers[k] = redact.String(v)
		}
	}
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	return enc.Encode(rec)
}

func FromResponse(provider, method, path, model string, status int, hdr http.Header, body []byte, note string) Record {
	return Record{
		TS:       time.Now().UTC(),
		Provider: provider,
		Method:   method,
		Path:     path,
		Model:    model,
		Status:   status,
		Headers:  redact.HeadersMap(hdr),
		Body:     string(body),
		Note:     note,
	}
}
