package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/pedro-mueller/conduit/internal/redact"
)

// Writer appends records to the usage ledger. Concurrent appends are
// serialised; the file is created 0600 (the directory 0700) because it is
// spend history, and opened lazily so a writer for a disabled path costs
// nothing.
type Writer struct {
	path string
	mu   sync.Mutex
	file *os.File
}

// NewWriter returns a writer for path; "" disables it.
func NewWriter(path string) *Writer {
	return &Writer{path: path}
}

// Enabled reports whether the writer persists anything.
func (w *Writer) Enabled() bool { return w != nil && w.path != "" }

// Append writes rec as one JSON line. The encoded line is passed through the
// credential redactor before it is written: the record has no field for a
// prompt or a credential, and redaction is the backstop for anything a
// provider echoes into a model name or id.
//
// When the ledger reaches maxLedgerBytes it is rotated to <path>.1 (replacing
// an older one), so the file stays bounded and history is kept one
// generation back, like decisions.jsonl.
func (w *Writer) Append(rec Record) error {
	if !w.Enabled() {
		return nil
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append([]byte(redact.String(string(line))), '\n')

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		// Reopen when the path no longer names the open file (moved or
		// deleted by hand), so appends never go to an unlinked inode.
		cur, err1 := w.file.Stat()
		onDisk, err2 := os.Stat(w.path)
		if err1 != nil || err2 != nil || !os.SameFile(cur, onDisk) {
			_ = w.file.Close()
			w.file = nil
		}
	}
	if err := w.open(); err != nil {
		return err
	}
	info, err := w.file.Stat()
	if err != nil {
		return err
	}
	if info.Size()+int64(len(line)) > maxLedgerBytes {
		if err := w.rotate(); err != nil {
			return err
		}
	}
	// One write of one line on an O_APPEND file: concurrent appenders (a
	// second gateway on the same path) cannot interleave inside it.
	_, err = w.file.Write(line)
	return err
}

func (w *Writer) rotate() error {
	_ = w.file.Close()
	w.file = nil
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		return err
	}
	return w.open()
}

func (w *Writer) open() error {
	if w.file != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	w.file = f
	return nil
}

// Close releases the file handle (tests and shutdown).
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// maxLedgerBytes is the rotation size. A record is ~250 bytes, so this is
// on the order of 100k requests per generation.
const maxLedgerBytes = 32 << 20 // 32 MiB
