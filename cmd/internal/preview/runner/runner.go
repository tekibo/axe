// Package runner provides production implementations of the runner interfaces
// defined in the parent preview package. Each type wraps external CLI tools
// (swiftc, simctl, git, etc.) via os/exec.
// xcodebuild operations are handled by the build package.
package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/k-kohey/axe/internal/procgroup"
)

// --- Toolchain ---

// Toolchain executes real toolchain commands (swiftc, clang, codesign, xcrun).
type Toolchain struct {
	swiftcPath        string
	swiftFrontendPath string
	clangPath         string
	sdkPaths          map[string]string
}

func (r *Toolchain) SDKPath(ctx context.Context, sdk string) (string, error) {
	if r.sdkPaths != nil && r.sdkPaths[sdk] != "" {
		return r.sdkPaths[sdk], nil
	}
	out, err := procgroup.Command(ctx, "xcrun", "--sdk", sdk, "--show-sdk-path").Output()
	if err != nil {
		return "", err
	}
	res := strings.TrimSpace(string(out))
	if r.sdkPaths == nil {
		r.sdkPaths = make(map[string]string)
	}
	r.sdkPaths[sdk] = res
	return res, nil
}

func (r *Toolchain) tryFastEmitLibrary(ctx context.Context, args []string) ([]byte, bool) {
	emitLib := false
	var dylibPath string
	for i := 0; i < len(args); i++ {
		if args[i] == "-emit-library" {
			emitLib = true
		} else if args[i] == "-o" && i+1 < len(args) {
			dylibPath = args[i+1]
		}
	}
	if !emitLib || dylibPath == "" || !strings.HasSuffix(dylibPath, ".dylib") {
		return nil, false
	}

	if r.swiftFrontendPath == "" {
		out, err := procgroup.Command(ctx, "xcrun", "-find", "swift-frontend").Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[tryFastEmitLibrary] finding swift-frontend failed: %v\n", err)
			return nil, false
		}
		r.swiftFrontendPath = strings.TrimSpace(string(out))
	}
	if r.clangPath == "" {
		out, err := procgroup.Command(ctx, "xcrun", "-find", "clang").Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[tryFastEmitLibrary] finding clang failed: %v\n", err)
			return nil, false
		}
		r.clangPath = strings.TrimSpace(string(out))
	}

	objPath := strings.TrimSuffix(dylibPath, ".dylib") + ".o"
	defer os.Remove(objPath)

	var sdk, target string
	frontendArgs := []string{r.swiftFrontendPath, "-frontend", "-c", "-enable-objc-interop"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "xcrun" || a == "swiftc" || a == r.swiftcPath || a == "-emit-library" || a == "-no-link-objc-runtime" {
			continue
		}
		if a == "-Xlinker" && i+1 < len(args) {
			i++
			continue
		}
		if a == "-sdk" && i+1 < len(args) {
			sdk = args[i+1]
			frontendArgs = append(frontendArgs, "-sdk", sdk)
			i++
			continue
		}
		if a == "-target" && i+1 < len(args) {
			target = args[i+1]
			frontendArgs = append(frontendArgs, "-target", target)
			i++
			continue
		}
		if a == "-L" && i+1 < len(args) {
			i++
			continue
		}
		if a == "-Xfrontend" && i+1 < len(args) {
			frontendArgs = append(frontendArgs, args[i+1])
			i++
			continue
		}
		if a == "-o" && i+1 < len(args) {
			frontendArgs = append(frontendArgs, "-o", objPath)
			i++
			continue
		}
		frontendArgs = append(frontendArgs, a)
	}

	tFast := time.Now()
	outF, errF := procgroup.Command(ctx, frontendArgs[0], frontendArgs[1:]...).CombinedOutput()
	if errF != nil {
		fmt.Fprintf(os.Stderr, "[tryFastEmitLibrary] swift-frontend failed: %v\nOutput: %s\n", errF, string(outF))
		return outF, false
	}

	clangArgs := []string{
		r.clangPath,
		"-dynamiclib",
		objPath,
		"-O0",
		"--sysroot", sdk,
		"--target=" + target,
		"-L", filepath.Join(sdk, "usr", "lib", "swift"),
		"-rpath", "/usr/lib/swift",
		"-Xlinker", "-undefined", "-Xlinker", "suppress",
		"-Xlinker", "-flat_namespace",
		"-o", dylibPath,
	}

	outC, errC := procgroup.Command(ctx, clangArgs[0], clangArgs[1:]...).CombinedOutput()
	if errC != nil {
		fmt.Fprintf(os.Stderr, "[tryFastEmitLibrary] clang failed: %v\nOutput: %s\nArgs: %v\n", errC, string(outC), clangArgs)
		return outC, false
	}

	fmt.Fprintf(os.Stderr, "[tryFastEmitLibrary] DIRECT COMPILE SUCCESS: swift-frontend + clang took %.3fs\n", time.Since(tFast).Seconds())
	return append(outF, outC...), true
}

func (r *Toolchain) CompileSwift(ctx context.Context, args []string) ([]byte, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("empty command args")
	}
	if out, ok := r.tryFastEmitLibrary(ctx, args); ok {
		return out, nil
	}
	if len(args) >= 2 && args[0] == "xcrun" {
		if args[1] == "swift-frontend" {
			if r.swiftFrontendPath == "" {
				out, err := procgroup.Command(ctx, "xcrun", "-find", "swift-frontend").Output()
				if err == nil {
					r.swiftFrontendPath = strings.TrimSpace(string(out))
				}
			}
			if r.swiftFrontendPath != "" {
				args = append([]string{r.swiftFrontendPath}, args[2:]...)
			}
		} else if args[1] == "swiftc" {
			if r.swiftcPath == "" {
				out, err := procgroup.Command(ctx, "xcrun", "-find", "swiftc").Output()
				if err == nil {
					r.swiftcPath = strings.TrimSpace(string(out))
				}
			}
			if r.swiftcPath != "" {
				args = append([]string{r.swiftcPath}, args[2:]...)
			}
		}
	}
	return procgroup.Command(ctx, args[0], args[1:]...).CombinedOutput()
}

func (r *Toolchain) CompileC(ctx context.Context, args []string) ([]byte, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("empty command args")
	}
	if len(args) >= 2 && args[0] == "xcrun" && args[1] == "clang" {
		if r.clangPath == "" {
			out, err := procgroup.Command(ctx, "xcrun", "-find", "clang").Output()
			if err == nil {
				r.clangPath = strings.TrimSpace(string(out))
			}
		}
		if r.clangPath != "" {
			args = append([]string{r.clangPath}, args[2:]...)
		}
	}
	return procgroup.Command(ctx, args[0], args[1:]...).CombinedOutput()
}

func (r *Toolchain) Codesign(ctx context.Context, path string) error {
	out, err := procgroup.Command(ctx, "codesign", "--force", "--sign", "-", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}

// --- App ---

// App executes real simctl app commands.
type App struct{}

// simctlCmd builds an exec.Cmd for "xcrun simctl" with optional --set for
// custom device sets. When deviceSetPath is non-empty, "--set <path>" is
// inserted right after "simctl".
func simctlCmd(ctx context.Context, deviceSetPath string, args ...string) *exec.Cmd {
	base := []string{"simctl"}
	if deviceSetPath != "" {
		base = append(base, "--set", deviceSetPath)
	}
	return procgroup.Command(ctx, "xcrun", append(base, args...)...)
}

func (r *App) Terminate(ctx context.Context, device, bundleID, deviceSetPath string) error {
	out, err := simctlCmd(ctx, deviceSetPath, "terminate", device, bundleID).CombinedOutput()
	if err != nil {
		// terminate may fail if app is not running — this is acceptable.
		// Include output for diagnostic purposes (callers log, not propagate).
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}

func (r *App) Install(ctx context.Context, device, appPath, deviceSetPath string) error {
	out, err := simctlCmd(ctx, deviceSetPath, "install", device, appPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}

func (r *App) Launch(ctx context.Context, device, bundleID, deviceSetPath string, env map[string]string, args []string) error {
	launchCmd := simctlCmd(ctx, deviceSetPath, append([]string{"launch", device, bundleID}, args...)...)
	launchCmd.Env = os.Environ()
	for k, v := range env {
		launchCmd.Env = append(launchCmd.Env, k+"="+v)
	}
	// simctl launch prints "<bundle-id>: <pid>" to stdout. Route it to stderr
	// so it doesn't pollute stdout, which may carry binary screenshot data
	// (oneshot mode) or JSON Lines protocol messages (serve mode).
	launchCmd.Stdout = os.Stderr
	launchCmd.Stderr = os.Stderr
	if err := launchCmd.Run(); err != nil {
		return fmt.Errorf("simctl launch failed: %w", err)
	}
	return nil
}

// --- FileCopy ---

// FileCopy executes real file copy commands.
type FileCopy struct{}

func (r *FileCopy) CopyDir(ctx context.Context, src, dst string) error {
	out, err := procgroup.Command(ctx, "cp", "-a", src, dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("copying directory: %w\n%s", err, out)
	}
	return nil
}

// --- SourceList ---

// SourceList uses git to list source files.
type SourceList struct{}

func (r *SourceList) SwiftFiles(ctx context.Context, root string) ([]string, error) {
	out, err := procgroup.Command(ctx,
		"git", "-C", root, "ls-files",
		"--cached", "--others", "--exclude-standard",
		"*.swift",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}

	var files []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		abs := filepath.Join(root, line)
		files = append(files, abs)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no Swift files found")
	}
	return files, nil
}

func (r *SourceList) SwiftDirs(ctx context.Context, root string) ([]string, error) {
	out, err := procgroup.Command(ctx,
		"git", "-C", root, "ls-files",
		"--cached", "--others", "--exclude-standard",
		"*.swift",
	).Output()
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var dirs []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		dir := filepath.Dir(filepath.Join(root, line))
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no Swift files found")
	}
	return dirs, nil
}
