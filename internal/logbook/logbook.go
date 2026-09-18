package logbook

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxFileBytes = 25 * 1024 * 1024
const maxDirectoryBytes = 1024 * 1024 * 1024
const retention = 30 * 24 * time.Hour

type Book struct {
	mu        sync.RWMutex
	directory string
	file      *os.File
	size      int64
	day       string
	events    []map[string]any
}

func Open(directory string) (*Book, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "windowkeeper.jsonl")
	if err := repair(path); err != nil {
		return nil, err
	}
	if err := prune(directory); err != nil {
		return nil, err
	}
	file, err := openAppend(path)
	if err != nil {
		return nil, err
	}
	info, _ := file.Stat()
	day := time.Now().UTC().Format("2006-01-02")
	if info.Size() > 0 {
		day = info.ModTime().UTC().Format("2006-01-02")
	}
	return &Book{directory: directory, file: file, size: info.Size(), day: day}, nil
}

func repair(path string) error {
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return err
	}
	data := make([]byte, min(info.Size(), 4096))
	_, err = file.ReadAt(data, info.Size()-int64(len(data)))
	if err != nil && err != io.EOF {
		return err
	}
	if data[len(data)-1] == '\n' {
		return nil
	}
	index := strings.LastIndexByte(string(data), '\n')
	if index < 0 {
		return file.Truncate(0)
	}
	return file.Truncate(info.Size() - int64(len(data)) + int64(index) + 1)
}

func openAppend(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_APPEND|syscall.O_CREAT|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func prune(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	type retained struct {
		path string
		info os.FileInfo
	}
	var files []retained
	var total int64
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "windowkeeper-") || !strings.HasSuffix(entry.Name(), ".jsonl") || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		if time.Since(info.ModTime()) > retention {
			_ = os.Remove(path)
			continue
		}
		files = append(files, retained{path, info})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].info.ModTime().Before(files[j].info.ModTime()) })
	for _, file := range files {
		if total <= maxDirectoryBytes {
			break
		}
		if os.Remove(file.path) == nil {
			total -= file.info.Size()
		}
	}
	return nil
}

func (b *Book) Log(level, event, message string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := Redact(map[string]any{"schema": "codex-broker.log/v1", "ts": time.Now().UTC().Format(time.RFC3339Nano), "level": level, "event": event, "message": message}).(map[string]any)
	line, _ := json.Marshal(entry)
	line = append(line, '\n')
	day := time.Now().UTC().Format("2006-01-02")
	if day != b.day || b.size+int64(len(line)) > maxFileBytes {
		_ = b.rotate()
	}
	_, _ = b.file.Write(line)
	b.size += int64(len(line))
	b.events = append(b.events, entry)
	if len(b.events) > 2000 {
		b.events = b.events[len(b.events)-2000:]
	}
}

func (b *Book) rotate() error {
	if err := b.file.Close(); err != nil {
		return err
	}
	source := filepath.Join(b.directory, "windowkeeper.jsonl")
	destination := filepath.Join(b.directory, "windowkeeper-"+b.day+"-"+time.Now().Format("150405.000000000")+".jsonl")
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	_ = prune(b.directory)
	file, err := openAppend(source)
	if err != nil {
		return err
	}
	b.file = file
	b.size = 0
	b.day = time.Now().UTC().Format("2006-01-02")
	return nil
}

func (b *Book) Recent(level, query string, limit int) []map[string]any {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if limit > 2000 {
		limit = 2000
	}
	var result []map[string]any
	query = strings.ToLower(query)
	for i := len(b.events) - 1; i >= 0 && len(result) < limit; i-- {
		event := b.events[i]
		if level != "" && event["level"] != level {
			continue
		}
		encoded, _ := json.Marshal(event)
		if query != "" && !strings.Contains(strings.ToLower(string(encoded)), query) {
			continue
		}
		result = append(result, event)
	}
	return result
}
func (b *Book) Export(level, query string, writer io.Writer) error {
	for _, event := range b.Recent(level, query, 2000) {
		if err := json.NewEncoder(writer).Encode(event); err != nil {
			return err
		}
	}
	return nil
}
func (b *Book) Close() error { b.mu.Lock(); defer b.mu.Unlock(); return b.file.Close() }

type handler struct{ book *Book }

func (h handler) Enabled(context.Context, slog.Level) bool { return true }
func (h handler) Handle(_ context.Context, record slog.Record) error {
	level := "INFO"
	if record.Level >= slog.LevelError {
		level = "ERROR"
	} else if record.Level >= slog.LevelWarn {
		level = "WARNING"
	} else if record.Level < slog.LevelInfo {
		level = "DEBUG"
	}
	h.book.Log(level, "codex_broker", record.Message)
	return nil
}
func (h handler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h handler) WithGroup(string) slog.Handler      { return h }
func (b *Book) Handler() slog.Handler                { return handler{b} }

func Load(path string) ([]map[string]any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var result []map[string]any
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) == nil {
			result = append(result, event)
		}
	}
	return result, scanner.Err()
}
