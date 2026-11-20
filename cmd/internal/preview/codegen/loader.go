package codegen

import (
	"bufio"
	"context"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// loaderSource is an Objective-C source that acts as a dylib loader injected
// into the simulator app via DYLD_INSERT_LIBRARIES.
//
// On load (__attribute__((constructor))):
//  1. Reads the Unix domain socket path from AXE_PREVIEW_SOCKET_PATH env var
//  2. Spawns a background pthread that listens on that socket
//  3. For each connection: reads a dylib path, dlopen()s it, calls
//     axe_preview_refresh via dlsym to replace rootViewController with preview content
//  4. Registers a UIApplicationDidBecomeActiveNotification observer (+fallback timer)
//     for initial preview refresh on app launch
//
//go:embed loader_source/loader.m
var loaderSource string

// LoaderCacheKey computes a SHA256 cache key from the loader source,
// SDK path, deployment target, and architecture. This ensures the cached dylib is
// invalidated when any of these change (e.g. Xcode update).
func LoaderCacheKey(source, sdk, deploymentTarget, arch string) string {
	hashInput := source + "\x00" + sdk + "\x00" + deploymentTarget + "\x00" + arch
	return fmt.Sprintf("%x", sha256.Sum256([]byte(hashInput)))
}

// CompileLoader compiles the Obj-C loader dylib for the simulator.
// The result is cached: recompilation is skipped if the source hash matches.
func CompileLoader(ctx context.Context, loaderDir, deploymentTarget, arch string, tc ToolchainRunner) (string, error) {
	if arch == "" {
		if runtime.GOARCH == "amd64" {
			arch = "x86_64"
		} else {
			arch = "arm64"
		}
	}
	if err := os.MkdirAll(loaderDir, 0o755); err != nil {
		return "", fmt.Errorf("creating loader dir: %w", err)
	}

	dylibPath := filepath.Join(loaderDir, "axe-preview-loader.dylib")
	hashPath := filepath.Join(loaderDir, "loader.sha256")

	// SDK path is needed both for cache key and compilation
	sdk, err := tc.SDKPath(ctx, "iphonesimulator")
	if err != nil {
		return "", fmt.Errorf("getting simulator SDK path: %w", err)
	}

	// Check if source hash matches the cached build
	currentHash := LoaderCacheKey(loaderSource, sdk, deploymentTarget, arch)
	if _, err := os.Stat(dylibPath); err == nil {
		if cached, err := os.ReadFile(hashPath); err == nil && string(cached) == currentHash {
			slog.Debug("Loader dylib cached, skipping compile", "path", dylibPath)
			return dylibPath, nil
		}
	}

	srcPath := filepath.Join(loaderDir, "loader.m")
	if err := os.WriteFile(srcPath, []byte(loaderSource), 0o600); err != nil {
		return "", fmt.Errorf("writing loader source: %w", err)
	}

	target := fmt.Sprintf("%s-apple-ios%s-simulator", arch, deploymentTarget)

	compileArgs := []string{
		"xcrun", "clang",
		"-dynamiclib",
		"-fobjc-arc",
		"-target", target,
		"-isysroot", sdk,
		"-framework", "Foundation",
		"-framework", "UIKit",
		"-o", dylibPath,
		srcPath,
	}
	slog.Debug("Compiling loader", "args", compileArgs)
	if out, err := tc.CompileC(ctx, compileArgs); err != nil {
		return "", fmt.Errorf("compiling loader: %w\n%s", err, out)
	}

	if err := tc.Codesign(ctx, dylibPath); err != nil {
		return "", fmt.Errorf("codesigning loader: %w", err)
	}

	// Save source hash for cache invalidation
	if err := os.WriteFile(hashPath, []byte(currentHash), 0o600); err != nil {
		slog.Warn("Failed to write loader hash", "err", err)
	}

	slog.Debug("Loader dylib ready", "path", dylibPath)
	return dylibPath, nil
}

var (
	waitForReadyBackoffs = []time.Duration{
		50 * time.Millisecond,
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1 * time.Second,
		1 * time.Second,
		1 * time.Second,
	}
	sendReloadBackoffs = []time.Duration{
		50 * time.Millisecond,
		100 * time.Millisecond,
		200 * time.Millisecond,
	}
)

// dialWithRetry connects to a Unix domain socket with exponential backoff.
// It respects context cancellation between retries.
func dialWithRetry(ctx context.Context, socketPath string, backoffs []time.Duration) (net.Conn, error) {
	var lastErr error
	for _, d := range backoffs {
		conn, err := net.DialTimeout("unix", socketPath, 1*time.Second)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		slog.Debug("Socket not ready, retrying", "backoff", d, "err", err)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connecting to loader socket: %w (last dial error: %w)", ctx.Err(), lastErr)
		case <-time.After(d):
		}
	}
	return nil, fmt.Errorf("connecting to loader socket: %w", lastErr)
}

// WaitForReady connects to the loader socket to verify the app runtime is
// ready, then immediately disconnects. This is safe because the loader's
// read() returns 0 bytes on disconnect and simply continues accepting.
func WaitForReady(ctx context.Context, socketPath string) error {
	conn, err := dialWithRetry(ctx, socketPath, waitForReadyBackoffs)
	if err != nil {
		return fmt.Errorf("waiting for loader ready: %w", err)
	}
	_ = conn.Close()
	return nil
}

// SendReloadCommand connects to the loader's Unix domain socket and sends
// a dylib path for hot-reload. It retries with exponential backoff if the
// socket is not yet ready. Returns in-app snapshot PNG bytes if provided.
func SendReloadCommand(ctx context.Context, socketPath, dylibPath string) ([]byte, error) {
	conn, err := dialWithRetry(ctx, socketPath, sendReloadBackoffs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := fmt.Fprintf(conn, "%s\n", dylibPath); err != nil {
		return nil, fmt.Errorf("sending dylib path: %w", err)
	}

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	line = strings.TrimSpace(line)

	if after, ok := strings.CutPrefix(line, "ERR:"); ok {
		return nil, fmt.Errorf("loader error: %s", after)
	}
	if !strings.HasPrefix(line, "OK") {
		return nil, fmt.Errorf("unexpected loader response: %s", line)
	}

	var screenshot []byte
	parts := strings.Fields(line)
	if len(parts) >= 2 {
		var length int
		if _, err := fmt.Sscanf(parts[1], "%d", &length); err == nil && length > 0 {
			screenshot = make([]byte, length)
			if _, err := io.ReadFull(reader, screenshot); err != nil {
				return nil, fmt.Errorf("reading screenshot payload (%d bytes): %w", length, err)
			}
		}
	}

	return screenshot, nil
}
