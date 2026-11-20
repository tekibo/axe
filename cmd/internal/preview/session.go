package preview

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/k-kohey/axe/internal/platform"
	"github.com/k-kohey/axe/internal/preview/analysis"
	"github.com/k-kohey/axe/internal/preview/build"
	"github.com/k-kohey/axe/internal/preview/codegen"
	"github.com/k-kohey/axe/internal/preview/runner"
	"github.com/k-kohey/axe/internal/procgroup"
	"github.com/k-kohey/axe/internal/simruntime"
	"golang.org/x/sync/errgroup"
)

// SessionConfig holds parameters for creating a PreviewSession.
type SessionConfig struct {
	PC               build.ProjectConfig
	DeviceUDID       string
	DeviceType       string
	Runtime          string
	DeviceSetPath    string
	IsExternalDevice bool
	NoHeadless       bool
	Preparer         *build.Preparer
	ReuseBuild       bool

	BuildRunner    BuildRunner
	Toolchain      ToolchainRunner
	AppRunner      AppRunner
	Copier         FileCopier
	RuntimeManager simruntime.Manager
}

// CaptureRequest describes a single preview capture within an existing session.
type CaptureRequest struct {
	SourceFile      string
	PreviewSelector string
	FullThunk       bool
	OnReady         func(ctx context.Context, device, setPath string) error
}

// PreviewSession manages the Boot/Install lifecycle for a simulator,
// allowing multiple previews to be captured without repeating the full cycle.
//
// PreviewSession is NOT goroutine-safe. Callers must not call CapturePreview
// concurrently on the same session. Each parallel worker should create its
// own session.
type PreviewSession struct {
	cfg         SessionConfig
	dirs        previewDirs
	bs          *build.Settings
	loaderPath  string
	runtime     simruntime.Manager
	sessionID   string
	ownsRuntime bool

	// Hot-reload state (mutable, not goroutine-safe).
	reloadCounter  int  // incremented after each successful reload/launch
	appLaunched    bool // true after first successful cold start
	lastScreenshot []byte
}

// NewPreviewSession creates a PreviewSession by running Build and Boot in parallel,
// then installing the app and compiling the loader.
func NewPreviewSession(ctx context.Context, cfg SessionConfig) (*PreviewSession, error) {
	runtime := cfg.RuntimeManager
	ownsRuntime := false
	if runtime == nil {
		var err error
		runtime, err = simruntime.New(
			simruntime.WithDeviceSetPath(cfg.DeviceSetPath),
			simruntime.WithSkipOrphanCleanup(cfg.DeviceSetPath == ""),
		)
		if err != nil {
			return nil, fmt.Errorf("creating simulator runtime: %w", err)
		}
		ownsRuntime = true
	}

	projDirs, err := build.NewProjectDirs(cfg.PC.PrimaryPath())
	if err != nil {
		return nil, fmt.Errorf("preview project dirs: %w", err)
	}

	targetUDID := cfg.DeviceUDID
	alreadyRunning := false
	var dirs previewDirs
	if targetUDID != "" {
		if d, dErr := newPreviewDirs(cfg.PC.PrimaryPath(), targetUDID); dErr == nil {
			dirs = d
			if conn, cErr := net.DialTimeout("unix", dirs.Socket, 150*time.Millisecond); cErr == nil {
				_ = conn.Close()
				alreadyRunning = true
				slog.Debug("Reusing warm preview session", "socket", dirs.Socket)
			}
		}
	}

	// Parallel: Build + runtime session creation.
	g, gctx := errgroup.WithContext(ctx)

	var bs *build.Settings
	g.Go(func() error {
		var result *build.Result
		var bErr error
		if cfg.Preparer != nil {
			result, bErr = cfg.Preparer.Prepare(gctx)
		} else {
			result, bErr = build.Prepare(gctx, cfg.PC, projDirs, cfg.ReuseBuild, cfg.BuildRunner)
		}
		if bErr != nil {
			return fmt.Errorf("build: %w", bErr)
		}
		bs = result.Settings
		return nil
	})

	var info *simruntime.SessionInfo
	if !alreadyRunning {
		g.Go(func() error {
			req := simruntime.CreateSessionRequest{
				DeviceType: cfg.DeviceType,
				Runtime:    cfg.Runtime,
				DeviceUDID: cfg.DeviceUDID,
				NoHeadless: cfg.NoHeadless || cfg.IsExternalDevice,
			}
			var createErr error
			info, createErr = runtime.CreateSession(gctx, req)
			if createErr != nil {
				return fmt.Errorf("creating simulator session: %w", createErr)
			}
			return nil
		})
	} else {
		info = &simruntime.SessionInfo{DeviceUDID: targetUDID}
	}

	if err := g.Wait(); err != nil {
		if info != nil && info.ID != "" {
			_ = runtime.StopSession(context.Background(), info.ID)
		}
		if ownsRuntime {
			runtime.Shutdown(context.Background())
		}
		return nil, err
	}

	if !alreadyRunning {
		var err error
		dirs, err = newPreviewDirs(cfg.PC.PrimaryPath(), info.DeviceUDID)
		if err != nil {
			if info.ID != "" {
				_ = runtime.StopSession(context.Background(), info.ID)
			}
			if ownsRuntime {
				runtime.Shutdown(context.Background())
			}
			return nil, fmt.Errorf("preview dirs: %w", err)
		}
	}

	// Sequential: Install + Loader (requires both Build result and Boot completion)
	var appRunner AppRunner
	if runtime != nil && info.ID != "" {
		appRunner = newSimRuntimeAppRunner(runtime, info.ID)
	}

	if !alreadyRunning {
		terminateApp(ctx, bs, info.DeviceUDID, cfg.DeviceSetPath, appRunner)

		if _, err := installApp(ctx, bs, dirs, info.DeviceUDID, cfg.DeviceSetPath, appRunner, cfg.Copier); err != nil {
			if info.ID != "" {
				_ = runtime.StopSession(context.Background(), info.ID)
			}
			if ownsRuntime {
				runtime.Shutdown(context.Background())
			}
			return nil, fmt.Errorf("install: %w", err)
		}
	}

	var loaderPath string
	candidateLoader := filepath.Join(dirs.Loader, "axe_loader.dylib")
	if alreadyRunning {
		if _, err := os.Stat(candidateLoader); err == nil {
			loaderPath = candidateLoader
		}
	}
	if loaderPath == "" {
		var err error
		loaderPath, err = codegen.CompileLoader(ctx, dirs.Loader, bs.DeploymentTarget, bs.TargetArch, cfg.Toolchain)
		if err != nil {
			if info.ID != "" {
				_ = runtime.StopSession(context.Background(), info.ID)
			}
			if ownsRuntime {
				runtime.Shutdown(context.Background())
			}
			return nil, fmt.Errorf("compile loader: %w", err)
		}
	}

	reloadCounter := 0
	if entries, err := os.ReadDir(dirs.Thunk); err == nil {
		for _, e := range entries {
			var n int
			if _, err := fmt.Sscanf(e.Name(), "thunk_%d.dylib", &n); err == nil {
				if n >= reloadCounter {
					reloadCounter = n + 1
				}
			}
		}
	}

	cfg.DeviceUDID = info.DeviceUDID
	if cfg.Toolchain != nil {
		go func() {
			_, _ = cfg.Toolchain.SDKPath(context.Background(), "iphonesimulator")
		}()
	}
	return &PreviewSession{
		cfg:           cfg,
		dirs:          dirs,
		bs:            bs,
		loaderPath:    loaderPath,
		runtime:       runtime,
		sessionID:     info.ID,
		ownsRuntime:   ownsRuntime,
		appLaunched:   alreadyRunning,
		reloadCounter: reloadCounter,
	}, nil
}

// CapturePreview compiles a main-only thunk for the given source file and
// delivers it to the running app. On the first call, a cold start (terminate ->
// launch -> WaitForReady -> SendReloadCommand) is performed. The explicit reload
// after launch makes the initial preview mount deterministic instead of relying
// on the loader's best-effort startup hook. Subsequent calls use hot-reload via
// SendReloadCommand, falling back to cold start on failure.
func (s *PreviewSession) CapturePreview(ctx context.Context, req CaptureRequest) error {
	tStart := time.Now()
	counter := s.reloadCounter
	var dylibPath string
	var err error
	var tParse, tCompile, tSendReload time.Duration
	var files []analysis.FileThunkData
	var previews []analysis.PreviewBlock
	if req.FullThunk {
		t0 := time.Now()
		files = parseTrackedFiles(req.SourceFile, []string{req.SourceFile}, nil)
		tParse = time.Since(t0)
		if len(files) == 0 {
			return fmt.Errorf("no tracked files found for %s", req.SourceFile)
		}
		pBlocks, _ := analysis.PreviewBlocks(req.SourceFile)
		previews = pBlocks
		fmt.Fprintf(os.Stderr, "[axe] AST parsed via swift-syntax in %.4fs\n", tParse.Seconds())

		t1 := time.Now()
		dylibPath, err = compileFullThunkFilesWithPreviews(ctx, req.SourceFile, files, previews, nil, s.bs, s.dirs, req.PreviewSelector, counter, s.cfg.Toolchain)
		tCompile = time.Since(t1)
		if err != nil {
			return fmt.Errorf("compile full thunk: %w", err)
		}
		fmt.Fprintf(os.Stderr, "[axe] compileFullThunkFiles succeeded: %s\n", dylibPath)
	} else {
		dylibPath, err = compileMainOnlyPipeline(ctx, req.SourceFile, s.bs, s.dirs, req.PreviewSelector, counter, s.cfg.Toolchain)
		if err != nil {
			return fmt.Errorf("compile thunk: %w", err)
		}
	}

	if s.appLaunched {
		t2 := time.Now()
		shot, rErr := codegen.SendReloadCommand(ctx, s.dirs.Socket, dylibPath)
		if rErr != nil {
			if ctx.Err() != nil || errors.Is(rErr, context.Canceled) || errors.Is(rErr, context.DeadlineExceeded) {
				return fmt.Errorf("hot-reload canceled: %w", rErr)
			}
			slog.Warn("Hot-reload failed, falling back to cold start", "err", rErr)
			if strings.Contains(rErr.Error(), "symbol not found in flat namespace") {
				fmt.Fprintf(os.Stderr, "[axe] Missing symbol in app binary, auto-rebuilding host app...\n")
				if rbErr := s.RebuildHostApp(ctx); rbErr != nil {
					fmt.Fprintf(os.Stderr, "[axe] Rebuilding host app failed: %v\n", rbErr)
				}
			}
			s.appLaunched = false
		} else if len(shot) > 0 {
			s.lastScreenshot = shot
			fmt.Fprintf(os.Stderr, "[axe] In-app snapshot received: %d bytes\n", len(shot))
		}
		tSendReload = time.Since(t2)
	}

	if !s.appLaunched {
		if err := s.coldStart(ctx, dylibPath); err != nil {
			return err
		}
	}

	fmt.Fprintf(os.Stderr, "[axe] Capture timings: parse=%.3fs, compile=%.3fs, sendReload=%.3fs, total=%.3fs\n",
		tParse.Seconds(), tCompile.Seconds(), tSendReload.Seconds(), time.Since(tStart).Seconds())

	// Increment counter AFTER successful reload/launch, before OnReady.
	// This ensures dlopen sees a unique path on retry (avoids cache hit).
	s.reloadCounter++
	cleanOldDylibs(s.dirs.Thunk, counter-1)

	if req.OnReady != nil {
		if err := req.OnReady(ctx, s.cfg.DeviceUDID, s.cfg.DeviceSetPath); err != nil {
			return fmt.Errorf("on-ready: %w", err)
		}
	}

	return nil
}

// coldStart terminates any running app, launches fresh, waits for the loader
// socket, then explicitly asks the loader to mount the preview dylib.
func (s *PreviewSession) coldStart(ctx context.Context, dylibPath string) error {
	var appRunner AppRunner
	if s.runtime != nil && s.sessionID != "" {
		appRunner = newSimRuntimeAppRunner(s.runtime, s.sessionID)
	} else if s.cfg.AppRunner != nil {
		appRunner = s.cfg.AppRunner
	} else {
		appRunner = &runner.App{}
	}
	terminateApp(ctx, s.bs, s.cfg.DeviceUDID, s.cfg.DeviceSetPath, appRunner)

	if _, err := installApp(ctx, s.bs, s.dirs, s.cfg.DeviceUDID, s.cfg.DeviceSetPath, appRunner, s.cfg.Copier); err != nil {
		slog.Warn("installApp before launch failed", "err", err)
	}

	if err := launchWithHotReload(ctx, s.bs, s.loaderPath, dylibPath, s.dirs.Socket, s.cfg.DeviceUDID, s.cfg.DeviceSetPath, appRunner); err != nil {
		return fmt.Errorf("launch: %w", err)
	}

	if err := codegen.WaitForReady(ctx, s.dirs.Socket); err != nil {
		return fmt.Errorf("wait for ready: %w", err)
	}
	shot, rErr := codegen.SendReloadCommand(ctx, s.dirs.Socket, dylibPath)
	if rErr != nil {
		return fmt.Errorf("initial reload: %w", rErr)
	}
	if len(shot) > 0 {
		s.lastScreenshot = shot
		fmt.Fprintf(os.Stderr, "[axe] In-app snapshot received (cold-start): %d bytes\n", len(shot))
	}

	s.appLaunched = true
	return nil
}

func (s *PreviewSession) Screenshot(ctx context.Context) ([]byte, error) {
	if len(s.lastScreenshot) > 0 {
		data := s.lastScreenshot
		s.lastScreenshot = nil
		return data, nil
	}
	if s.runtime != nil && s.sessionID != "" {
		return s.runtime.Screenshot(ctx, s.sessionID)
	}
	if s.cfg.DeviceUDID != "" {
		return platform.Screenshot(ctx, s.cfg.DeviceUDID, s.cfg.DeviceSetPath)
	}
	return nil, fmt.Errorf("no screenshot available: in-app snapshot buffer is empty and no simulator runtime active")
}

// ModuleName returns the module name for the active preview session.
func (s *PreviewSession) ModuleName() string {
	if s.bs != nil {
		return s.bs.ModuleName
	}
	return ""
}

// RebuildModule scans the cached source directory and re-emits the .swiftmodule
// so newly added structs/views are immediately available to the preview compiler.
func (s *PreviewSession) RebuildModule(ctx context.Context) error {
	if s.bs == nil || len(s.bs.ExtraIncludePaths) == 0 {
		return fmt.Errorf("no module search path in settings")
	}
	moduleDir := s.bs.ExtraIncludePaths[0]
	modulePath := filepath.Join(moduleDir, s.bs.ModuleName+".swiftmodule")
	sdk, err := s.cfg.Toolchain.SDKPath(ctx, "iphonesimulator")
	if err != nil {
		return err
	}
	target := fmt.Sprintf("%s-apple-ios%s-simulator", s.bs.TargetArch, s.bs.DeploymentTarget)
	if s.bs.TargetArch == "" {
		target = fmt.Sprintf("x86_64-apple-ios%s-simulator", s.bs.DeploymentTarget)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	sourcesDir := filepath.Join(home, "Library/Caches/mac-sim.nvim/sources")
	var swiftSources []string
	_ = filepath.Walk(sourcesDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".swift") && !strings.HasSuffix(p, "Package.swift") {
			swiftSources = append(swiftSources, p)
		}
		return nil
	})
	if len(swiftSources) == 0 {
		return fmt.Errorf("no swift sources found in %s", sourcesDir)
	}

	sv := s.bs.SwiftVersion
	if sv == "" {
		sv = "6"
	}
	if major, _, ok := strings.Cut(sv, "."); ok && sv != "4.2" {
		sv = major
	}

	args := []string{
		"xcrun", "swiftc",
		"-emit-module",
		"-enable-private-imports",
		"-Xfrontend", "-enable-implicit-dynamic",
		"-emit-module-path", modulePath,
		"-module-name", s.bs.ModuleName,
		"-target", target,
		"-sdk", sdk,
	}
	args = append(args, swiftSources...)
	args = append(args, "-swift-version", sv)

	out, err := procgroup.Command(ctx, args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("rebuilding module failed: %w\n%s", err, out)
	}
	return nil
}

// RebuildHostApp compiles all cached Swift sources into the host app binary and .swiftmodule.
func (s *PreviewSession) RebuildHostApp(ctx context.Context) error {
	if s.bs == nil || len(s.bs.ExtraIncludePaths) == 0 {
		return fmt.Errorf("no build settings")
	}
	sdk, err := s.cfg.Toolchain.SDKPath(ctx, "iphonesimulator")
	if err != nil {
		return err
	}
	target := fmt.Sprintf("%s-apple-ios%s-simulator", s.bs.TargetArch, s.bs.DeploymentTarget)
	if s.bs.TargetArch == "" {
		target = fmt.Sprintf("x86_64-apple-ios%s-simulator", s.bs.DeploymentTarget)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	sourcesDir := filepath.Join(home, "Library/Caches/mac-sim.nvim/sources")
	var swiftSources []string
	_ = filepath.Walk(sourcesDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".swift") && !strings.HasSuffix(p, "Package.swift") {
			swiftSources = append(swiftSources, p)
		}
		return nil
	})
	if len(swiftSources) == 0 {
		return fmt.Errorf("no swift sources found in %s", sourcesDir)
	}

	sv := s.bs.SwiftVersion
	if sv == "" {
		sv = "6"
	}
	if major, _, ok := strings.Cut(sv, "."); ok && sv != "4.2" {
		sv = major
	}

	moduleDir := s.bs.ExtraIncludePaths[0]
	modulePath := filepath.Join(moduleDir, s.bs.ModuleName+".swiftmodule")
	appBundle := filepath.Join(s.bs.BuiltProductsDir, s.bs.ModuleName+".app")
	appExec := filepath.Join(appBundle, s.bs.ModuleName)

	args := []string{
		"xcrun", "swiftc",
		"-sdk", sdk,
		"-target", target,
		"-emit-executable",
		"-emit-module",
		"-emit-module-path", modulePath,
		"-enable-private-imports",
		"-Xfrontend", "-enable-implicit-dynamic",
		"-module-name", s.bs.ModuleName,
	}
	args = append(args, swiftSources...)
	args = append(args, "-o", appExec, "-swift-version", sv)

	fmt.Fprintf(os.Stderr, "[axe] Auto-rebuilding %s.app and %s.swiftmodule...\n", s.bs.ModuleName, s.bs.ModuleName)
	out, err := procgroup.Command(ctx, args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("rebuilding host app: %w\n%s", err, out)
	}
	_ = s.cfg.Toolchain.Codesign(ctx, appBundle)
	return nil
}

// Close terminates the app, removes the socket, and stops the runtime session.
func (s *PreviewSession) Close() {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if s.runtime != nil && s.sessionID != "" {
		if err := s.runtime.StopSession(cleanupCtx, s.sessionID); err != nil {
			slog.Debug("Failed to stop preview runtime session", "sessionId", s.sessionID, "err", err)
		}
	}

	if err := os.Remove(s.dirs.Socket); err != nil && !os.IsNotExist(err) {
		slog.Debug("Failed to remove socket", "path", s.dirs.Socket, "err", err)
	}

	if s.ownsRuntime && s.runtime != nil {
		s.runtime.Shutdown(cleanupCtx)
	}
}

// DefaultSessionRunners returns the production implementations of runner interfaces
// for use with SessionConfig. This keeps the report package from directly
// depending on the runner package.
func DefaultSessionRunners() (BuildRunner, ToolchainRunner, AppRunner, FileCopier) {
	return build.NewRunner(), &runner.Toolchain{}, &runner.App{}, &runner.FileCopy{}
}
