package codegen

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/k-kohey/axe/internal/preview/analysis"
)

// thunkFuncMap provides helper functions for thunk templates.
var thunkFuncMap = template.FuncMap{
	// escapeSwiftString escapes backslashes and double quotes for use inside Swift string literals.
	"escapeSwiftString": func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `"`, `\"`)
		return s
	},
	"hasPrefix": strings.HasPrefix,
}

// PerFileThunkTmpl generates a per-file thunk that imports only its own source file
// via @_private(sourceFile:). This scoping prevents private type name collisions
// across files because each thunk only sees the private types from its own file.
var PerFileThunkTmpl = template.Must(template.New("perFileThunk").Funcs(thunkFuncMap).Parse(
	`@_private(sourceFile: "{{ .FileName | escapeSwiftString }}") import {{ .ModuleName }}
import SwiftUI
{{ range .ExtraImports }}{{ . }}
{{ end }}
{{ $filePath := .AbsPath }}{{ range .Types }}extension {{ .Name }} {
{{ range .Properties }}    @_dynamicReplacement(for: {{ .Name }}) private var __preview__{{ .Name }}: {{ .TypeExpr }} {
        #sourceLocation(file: "{{ $filePath | escapeSwiftString }}", line: {{ .BodyLine }})
{{ .Source }}
        #sourceLocation()
    }
{{ end }}{{ range .Methods }}{{ if and (ne .Name "init") (not (hasPrefix .Selector "init(")) }}    @_dynamicReplacement(for: {{ .Selector }})
    private func __preview__{{ .Name }}{{ .Signature }} {
        #sourceLocation(file: "{{ $filePath | escapeSwiftString }}", line: {{ .BodyLine }})
{{ .Source }}
        #sourceLocation()
    }
{{ end }}{{ end }}}
{{ end }}`))

// MainThunkTmpl generates the main thunk containing the preview wrapper and
// refresh entry point. It uses @_private(sourceFile:) import for the target file
// so that #Preview blocks can reference private/fileprivate types defined in
// that file.
var MainThunkTmpl = template.Must(template.New("mainThunk").Funcs(thunkFuncMap).Parse(
	`import SwiftUI
import Foundation
{{ range .ExtraImports }}{{ . }}
{{ end }}{{ if .HasPreview }}
@_private(sourceFile: "{{ .TargetFileName | escapeSwiftString }}") import {{ .ModuleName }}

struct _AxePreviewWrapper: View {
{{ range .PreviewProps }}    {{ .Source }}
{{ end }}
    var body: some View {
{{ .PreviewBody }}
        .id(UUID())
    }
}
{{ end }}
import UIKit

@MainActor
@_cdecl("axe_preview_refresh")
public func _axePreviewRefresh() {
{{ if .HasPreview }}
    let hc = UIHostingController(rootView: AnyView(_AxePreviewWrapper()))
    for scene in UIApplication.shared.connectedScenes {
        guard let ws = scene as? UIWindowScene else { continue }
        guard let window = ws.windows.first(where: { $0.isKeyWindow }) ?? ws.windows.first else { continue }
        window.rootViewController = hc
        window.makeKeyAndVisible()
        window.layoutIfNeeded()
        CATransaction.flush()
        break
    }
{{ end }}
}
`))

// PerFileThunkData holds the data used to render a per-file thunk template.
type PerFileThunkData struct {
	FileName     string
	AbsPath      string
	ModuleName   string
	ExtraImports []string
	Types        []analysis.TypeInfo
}

// MainThunkData holds the data used to render the main thunk template.
type MainThunkData struct {
	ModuleName     string
	TargetFileName string   // base name of the target source file (e.g. "ContentView.swift")
	ExtraImports   []string // imports from the target source file (e.g. "import DSTheme")
	HasPreview     bool
	PreviewProps   []analysis.PreviewableProperty
	PreviewBody    string
}

// GenerateUnifiedThunks generates a single unified thunk file when only one source file is tracked,
// enabling swiftc to compile the entire thunk in a single frontend job without secondary process spawns.
// Falls back to per-file thunks when multiple files are tracked.
func GenerateUnifiedThunks(
	files []analysis.FileThunkData,
	previews []analysis.PreviewBlock,
	moduleName string,
	thunkDir string,
	previewSelector string,
	targetSourceFile string,
	reloadCounter int,
) ([]string, error) {
	if len(files) == 1 {
		if err := os.MkdirAll(thunkDir, 0o755); err != nil {
			return nil, fmt.Errorf("creating thunk dir: %w", err)
		}
		cleanOldThunkFiles(thunkDir, reloadCounter)

		f := files[0]
		fileModuleName := f.ModuleName
		if fileModuleName == "" {
			fileModuleName = moduleName
		}

		td := PerFileThunkData{
			FileName:     f.FileName,
			AbsPath:      f.AbsPath,
			ModuleName:   fileModuleName,
			ExtraImports: f.Imports,
			Types:        f.Types,
		}

		mtd := MainThunkData{
			ModuleName:     moduleName,
			TargetFileName: filepath.Base(targetSourceFile),
			ExtraImports:   f.Imports,
		}

		if err := resolvePreviewWithExplicit(&mtd, targetSourceFile, previewSelector, previews); err != nil {
			return nil, err
		}

		unifiedPath := filepath.Join(thunkDir, fmt.Sprintf("thunk_%d.swift", reloadCounter))
		file, err := os.OpenFile(unifiedPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return nil, fmt.Errorf("creating unified thunk file: %w", err)
		}
		if err := PerFileThunkTmpl.Execute(file, td); err != nil {
			file.Close()
			return nil, fmt.Errorf("rendering per-file template into unified thunk: %w", err)
		}
		if _, err := file.WriteString("\n\n"); err != nil {
			file.Close()
			return nil, err
		}
		if err := MainThunkTmpl.Execute(file, mtd); err != nil {
			file.Close()
			return nil, fmt.Errorf("rendering main template into unified thunk: %w", err)
		}
		file.Close()

		slog.Debug("Generated single unified thunk (single-job mode)", "path", unifiedPath)
		return []string{unifiedPath}, nil
	}

	return GenerateThunksWithPreviews(files, previews, moduleName, thunkDir, previewSelector, targetSourceFile, reloadCounter)
}

// GenerateThunks generates per-file thunks and a main thunk.
// Each per-file thunk has its own @_private(sourceFile:) import, so private types
// from different files never collide. The main thunk contains the preview wrapper
// and refresh entry point.
//
// Returns the list of all generated thunk paths (per-file + main).
func GenerateThunks(
	files []analysis.FileThunkData,
	moduleName string,
	thunkDir string,
	previewSelector string,
	targetSourceFile string,
	reloadCounter int,
) ([]string, error) {
	return GenerateThunksWithPreviews(files, nil, moduleName, thunkDir, previewSelector, targetSourceFile, reloadCounter)
}

// GenerateThunksWithPreviews is identical to GenerateThunks but accepts an optional pre-parsed
// slice of PreviewBlocks to avoid re-parsing the AST when previews are already known.
func GenerateThunksWithPreviews(
	files []analysis.FileThunkData,
	previews []analysis.PreviewBlock,
	moduleName string,
	thunkDir string,
	previewSelector string,
	targetSourceFile string,
	reloadCounter int,
) (thunkPaths []string, retErr error) {
	slog.Debug("Generating per-file thunks")

	if err := os.MkdirAll(thunkDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating thunk dir: %w", err)
	}

	// Clean up old thunk files from the current cycle prefix.
	cleanOldThunkFiles(thunkDir, reloadCounter)

	// Track basename usage to handle duplicate file names.
	baseNameCount := make(map[string]int)
	for _, f := range files {
		base := strings.TrimSuffix(filepath.Base(f.AbsPath), filepath.Ext(f.AbsPath))
		baseNameCount[base]++
	}
	for base, count := range baseNameCount {
		if count > 1 {
			slog.Warn("Duplicate Swift basenames detected; @_private(sourceFile:) may bind ambiguously", "basename", base, "count", count)
		}
	}
	baseNameSeen := make(map[string]int)

	// Generate per-file thunks.
	for _, f := range files {
		base := strings.TrimSuffix(filepath.Base(f.AbsPath), filepath.Ext(f.AbsPath))

		// Handle duplicate basenames by appending an index.
		fileName := base
		if baseNameCount[base] > 1 {
			idx := baseNameSeen[base]
			baseNameSeen[base]++
			if idx > 0 {
				fileName = fmt.Sprintf("%s_%d", base, idx)
			}
		}

		thunkFileName := fmt.Sprintf("thunk_%d_%s.swift", reloadCounter, fileName)
		thunkPath := filepath.Join(thunkDir, thunkFileName)

		// Use per-file module name from index store if available.
		// Falls back to the main module name when the file's module is unknown
		// (e.g. index store not loaded or file not in index store).
		fileModuleName := f.ModuleName
		if fileModuleName == "" {
			fileModuleName = moduleName
		}

		td := PerFileThunkData{
			FileName:     f.FileName,
			AbsPath:      f.AbsPath,
			ModuleName:   fileModuleName,
			ExtraImports: f.Imports,
			Types:        f.Types,
		}

		if err := writeTemplate(thunkPath, PerFileThunkTmpl, td); err != nil {
			return nil, fmt.Errorf("generating per-file thunk for %s: %w", f.FileName, err)
		}

		thunkPaths = append(thunkPaths, thunkPath)
	}

	// Collect imports from all files because the preview body may reference
	// symbols that are only imported in dependency files.
	seenImports := make(map[string]struct{})
	var mergedImports []string
	for _, f := range files {
		for _, imp := range f.Imports {
			if _, exists := seenImports[imp]; exists {
				continue
			}
			seenImports[imp] = struct{}{}
			mergedImports = append(mergedImports, imp)
		}
	}

	// Build main thunk data.
	mtd := MainThunkData{
		ModuleName:     moduleName,
		TargetFileName: filepath.Base(targetSourceFile),
		ExtraImports:   mergedImports,
	}

	if err := resolvePreviewWithExplicit(&mtd, targetSourceFile, previewSelector, previews); err != nil {
		return nil, err
	}

	mainThunkPath := filepath.Join(thunkDir, fmt.Sprintf("thunk_%d__main.swift", reloadCounter))
	if err := writeTemplate(mainThunkPath, MainThunkTmpl, mtd); err != nil {
		return nil, fmt.Errorf("generating main thunk: %w", err)
	}
	thunkPaths = append(thunkPaths, mainThunkPath)

	slog.Debug("Generated thunks", "count", len(thunkPaths), "files", len(files))
	return thunkPaths, nil
}

// GenerateMainOnlyThunk generates a single main thunk file without per-file
// dynamic replacements. This is the "degraded" / "lightweight" thunk that only
// contains the preview wrapper and refresh entry point. It is used when full
// thunk compilation fails or is not requested.
//
// Returns the list of generated thunk paths (always a single main thunk).
func GenerateMainOnlyThunk(
	moduleName string,
	thunkDir string,
	targetSourceFile string,
	previewSelector string,
	imports []string,
	reloadCounter int,
) ([]string, error) {
	if err := os.MkdirAll(thunkDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating thunk dir: %w", err)
	}

	cleanOldThunkFiles(thunkDir, reloadCounter)

	mtd := MainThunkData{
		ModuleName:     moduleName,
		TargetFileName: filepath.Base(targetSourceFile),
		ExtraImports:   imports,
	}

	if err := resolvePreview(&mtd, targetSourceFile, previewSelector); err != nil {
		return nil, err
	}

	mainThunkPath := filepath.Join(thunkDir, fmt.Sprintf("thunk_%d__main.swift", reloadCounter))
	if err := writeTemplate(mainThunkPath, MainThunkTmpl, mtd); err != nil {
		return nil, fmt.Errorf("generating main-only thunk: %w", err)
	}

	slog.Debug("Generated main-only thunk", "path", mainThunkPath)
	return []string{mainThunkPath}, nil
}

// resolvePreview parses #Preview blocks from targetSourceFile and populates
// the preview-related fields on mtd. If no #Preview blocks are found, it uses
// AST type inspection to find the primary View conforming type in targetSourceFile.
// If neither exists, it returns a descriptive error (fail fast and loud).
func resolvePreview(mtd *MainThunkData, targetSourceFile, previewSelector string) error {
	return resolvePreviewWithExplicit(mtd, targetSourceFile, previewSelector, nil)
}

func resolvePreviewWithExplicit(mtd *MainThunkData, targetSourceFile, previewSelector string, explicitPreviews []analysis.PreviewBlock) error {
	previews := explicitPreviews
	if len(previews) == 0 {
		var err error
		previews, err = analysis.PreviewBlocks(targetSourceFile)
		if err != nil {
			slog.Warn("Failed to parse #Preview blocks", "err", err)
		}
	}
	if len(previews) > 0 {
		selected, err := analysis.SelectPreview(previews, previewSelector)
		if err != nil {
			return err
		}
		tp := analysis.TransformPreviewBlock(selected)
		mtd.HasPreview = true
		mtd.PreviewProps = tp.Properties
		mtd.PreviewBody = tp.BodySource
		return nil
	}

	// If no #Preview block exists, inspect AST for a SwiftUI View conforming struct
	types, _, sErr := analysis.SourceFile(targetSourceFile, nil)
	if sErr == nil {
		for _, t := range types {
			if t.IsView() {
				mtd.HasPreview = true
				mtd.PreviewBody = fmt.Sprintf("%s()", t.Name)
				slog.Info("Resolved primary View struct from AST", "view", t.Name, "file", targetSourceFile)
				return nil
			}
		}
	}

	return fmt.Errorf("no #Preview block or SwiftUI View conforming struct found in %s", targetSourceFile)
}

// HasBaseNameCollision reports whether newFile's basename (without extension,
// case-insensitive) collides with any file in existingFiles.
// If newFile is the same path as an existing file, it is not considered a collision.
func HasBaseNameCollision(newFile string, existingFiles []string) bool {
	newBase := strings.ToLower(strings.TrimSuffix(filepath.Base(newFile), filepath.Ext(newFile)))
	newClean := filepath.Clean(newFile)
	for _, f := range existingFiles {
		if filepath.Clean(f) == newClean {
			continue // same file, not a collision
		}
		base := strings.ToLower(strings.TrimSuffix(filepath.Base(f), filepath.Ext(f)))
		if base == newBase {
			return true
		}
	}
	return false
}

// writeTemplate renders a template to a file.
func writeTemplate(path string, tmpl *template.Template, data any) (retErr error) {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating file: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("closing file: %w", cerr)
		}
	}()

	if err := tmpl.Execute(f, data); err != nil {
		return fmt.Errorf("executing template: %w", err)
	}
	return nil
}

// cleanOldThunkFiles removes thunk Swift files from a previous cycle.
func cleanOldThunkFiles(thunkDir string, currentCounter int) {
	patterns := []string{
		filepath.Join(thunkDir, fmt.Sprintf("thunk_%d_*.swift", currentCounter)),
		filepath.Join(thunkDir, fmt.Sprintf("thunk_%d.swift", currentCounter)),
	}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, m := range matches {
			if err := os.Remove(m); err == nil {
				slog.Debug("Cleaned old thunk file", "path", m)
			}
		}
	}
}
