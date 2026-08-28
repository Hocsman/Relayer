package fixturecapture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type captureResult struct {
	raw       []byte
	outcome   Outcome
	exitCode  *int
	truncated bool
}

// Capture starts exactly one output-only CLI in a PTY or an isolated private
// tmux server. It never forwards caller input and never accepts or records an
// environment map. Timeout and output-limit outcomes still return a fixture.
func Capture(ctx context.Context, options Options) (Fixture, error) {
	if ctx == nil {
		return Fixture{}, errors.New("capture context must not be nil")
	}
	options, err := normalizeOptions(options)
	if err != nil {
		return Fixture{}, err
	}
	if err := ctx.Err(); err != nil {
		return Fixture{}, err
	}

	var result captureResult
	switch options.Backend {
	case BackendPTY:
		result, err = capturePTY(ctx, options)
	case BackendTmux:
		result, err = captureTmux(ctx, options)
	default:
		panic("validated fixture backend became invalid")
	}
	if err != nil {
		zeroBytes(result.raw)
		return Fixture{}, err
	}
	fixture, err := options.Anonymizer.fixture(
		options.Tool,
		options.Adapter,
		options.Backend,
		result.outcome,
		result.exitCode,
		result.truncated,
		result.raw,
	)
	zeroBytes(result.raw)
	if err != nil {
		return Fixture{}, fmt.Errorf("anonymize captured output: %w", err)
	}
	return cloneFixture(fixture), nil
}

type boundedCollector struct {
	reader   io.Reader
	limit    int
	complete chan collectorResult
	close    func() error
	once     sync.Once
}

type collectorResult struct {
	data      []byte
	truncated bool
	err       error
}

func newBoundedCollector(reader io.Reader, closer io.Closer, limit int) *boundedCollector {
	collector := &boundedCollector{
		reader:   reader,
		limit:    limit,
		complete: make(chan collectorResult, 1),
		close:    closer.Close,
	}
	go collector.run()
	return collector
}

func (collector *boundedCollector) run() {
	var output bytes.Buffer
	buffer := make([]byte, 4096)
	for {
		count, err := collector.reader.Read(buffer)
		if count > 0 {
			remaining := collector.limit - output.Len()
			if count > remaining {
				if remaining > 0 {
					_, _ = output.Write(buffer[:remaining])
				}
				collector.complete <- collectorResult{data: bytes.Clone(output.Bytes()), truncated: true}
				return
			}
			_, _ = output.Write(buffer[:count])
		}
		if err != nil {
			if errors.Is(err, io.EOF) || isTerminalCloseError(err) {
				collector.complete <- collectorResult{data: bytes.Clone(output.Bytes())}
			} else {
				collector.complete <- collectorResult{data: bytes.Clone(output.Bytes()), err: err}
			}
			return
		}
	}
}

func (collector *boundedCollector) stop() {
	collector.once.Do(func() { _ = collector.close() })
}

func createPrivateEnvironment(runtimeDirectory string) error {
	for _, name := range []string{"tmp", "home", "xdg-config", "xdg-cache", "xdg-data"} {
		path := filepath.Join(runtimeDirectory, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			return fmt.Errorf("create private capture environment directory: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("restrict private capture environment directory: %w", err)
		}
	}
	return nil
}

func restorePrivateRuntimePermissions(runtimeDirectory string) {
	info, err := os.Lstat(runtimeDirectory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	_ = filepath.WalkDir(runtimeDirectory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr == nil && entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
}

func cleanupPrivateRuntime(runtimeDirectory string) error {
	restorePrivateRuntimePermissions(runtimeDirectory)
	_ = os.RemoveAll(runtimeDirectory)
	if _, err := os.Lstat(runtimeDirectory); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return errors.New("private capture runtime cleanup could not be confirmed")
}

func safeEnvironment(runtimeDirectory string) []string {
	allowed := []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "COLORTERM", "NO_COLOR"}
	result := make([]string, 0, len(allowed)+5)
	for _, name := range allowed {
		if value, exists := os.LookupEnv(name); exists && !strings.ContainsRune(value, 0) {
			result = append(result, name+"="+value)
		}
	}
	if !hasEnvironment(result, "TERM") {
		result = append(result, "TERM=xterm-256color")
	}
	if runtimeDirectory != "" {
		result = append(result,
			"HOME="+filepath.Join(runtimeDirectory, "home"),
			"TMPDIR="+filepath.Join(runtimeDirectory, "tmp"),
			"XDG_CONFIG_HOME="+filepath.Join(runtimeDirectory, "xdg-config"),
			"XDG_CACHE_HOME="+filepath.Join(runtimeDirectory, "xdg-cache"),
			"XDG_DATA_HOME="+filepath.Join(runtimeDirectory, "xdg-data"),
		)
	}
	return result
}

func hasEnvironment(environment []string, name string) bool {
	prefix := name + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
