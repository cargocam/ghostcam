// pydanticgen walks a Go package and emits pydantic v2 models that match
// each struct's JSON wire format. It exists so the Python camera consumes
// the same source of truth (common/) as tygo does for the TypeScript UI.
//
// Run via `go generate ./...`; the directive lives in common/types.go
// alongside the existing tygo invocation.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// jsonTag parses the json struct tag for a field, returning the wire name
// and whether the field has `omitempty`.
type jsonTag struct {
	name      string
	omitempty bool
	skip      bool
}

func parseJSONTag(tag string) jsonTag {
	tag = strings.Trim(tag, "`")
	for _, part := range strings.Fields(tag) {
		if !strings.HasPrefix(part, "json:") {
			continue
		}
		raw := strings.Trim(strings.TrimPrefix(part, "json:"), `"`)
		if raw == "-" {
			return jsonTag{skip: true}
		}
		parts := strings.Split(raw, ",")
		t := jsonTag{name: parts[0]}
		for _, opt := range parts[1:] {
			if opt == "omitempty" {
				t.omitempty = true
			}
		}
		return t
	}
	return jsonTag{}
}

// goTypeToPython converts a Go type expression to a pydantic-compatible
// Python type. Pointer types become Optional[...]. Slices become list[...].
// Custom struct refs preserve the Go name (PascalCase) because the
// generator emits matching pydantic class names.
func goTypeToPython(expr ast.Expr) (typ string, optional bool) {
	switch e := expr.(type) {
	case *ast.Ident:
		switch e.Name {
		case "string":
			return "str", false
		case "bool":
			return "bool", false
		case "int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64":
			return "int", false
		case "float32", "float64":
			return "float", false
		default:
			// Sibling struct in the same package.
			return e.Name, false
		}
	case *ast.StarExpr:
		inner, _ := goTypeToPython(e.X)
		return inner, true
	case *ast.ArrayType:
		inner, _ := goTypeToPython(e.Elt)
		return "list[" + inner + "]", false
	case *ast.SelectorExpr:
		// e.g. common.TelemetryDatagram → TelemetryDatagram (assume same emit dir)
		return e.Sel.Name, false
	}
	return "Any", false
}

// pythonDefault picks a default value for a field. omitempty fields default
// to None (Optional). Required fields have no default.
func pythonDefault(t string, optional, omitempty bool) (def string, isOptional bool) {
	if optional || omitempty {
		return " = None", true
	}
	return "", false
}

type field struct {
	pyName     string
	pyType     string
	jsonName   string
	optional   bool
	omitempty  bool
}

type structDef struct {
	name   string
	doc    string
	fields []field
}

type fileEmit struct {
	srcFile string
	modName string // Python module basename without .py
	structs []structDef
}

// usedTypeNames collects every struct-typed identifier referenced by a
// file's fields. Used to compute cross-file imports.
func (e *fileEmit) usedTypeNames() map[string]struct{} {
	out := map[string]struct{}{}
	for _, sd := range e.structs {
		for _, f := range sd.fields {
			for _, n := range collectIdentNames(f.pyType) {
				out[n] = struct{}{}
			}
		}
	}
	return out
}

func (e *fileEmit) localNames() map[string]struct{} {
	out := map[string]struct{}{}
	for _, sd := range e.structs {
		out[sd.name] = struct{}{}
	}
	return out
}

// collectIdentNames pulls capitalized identifiers out of a Python type
// annotation string like "list[Optional[CameraCommand]]". Anything that
// starts with an uppercase letter is treated as a struct ref candidate.
func collectIdentNames(typ string) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(typ); i++ {
		c := typ[i]
		if (c >= 'A' && c <= 'Z') || (cur.Len() > 0 && ((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_')) {
			cur.WriteByte(c)
		} else {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	// Drop pydantic/typing builtins.
	filtered := out[:0]
	for _, n := range out {
		switch n {
		case "Any", "BaseModel", "ConfigDict", "Field", "None":
			continue
		}
		filtered = append(filtered, n)
	}
	return filtered
}

func extractStructs(filename string) (*fileEmit, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	emit := &fileEmit{
		srcFile: filepath.Base(filename),
		modName: strings.TrimSuffix(filepath.Base(filename), ".go"),
	}

	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts := spec.(*ast.TypeSpec)
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			sd := structDef{name: ts.Name.Name}
			if gen.Doc != nil {
				sd.doc = strings.TrimSpace(gen.Doc.Text())
			}
			for _, fld := range st.Fields.List {
				if fld.Tag == nil {
					continue
				}
				tag := parseJSONTag(fld.Tag.Value)
				if tag.skip || tag.name == "" {
					continue
				}
				typ, optional := goTypeToPython(fld.Type)
				for _, fname := range fld.Names {
					_ = fname // we use the JSON tag name as the Python attribute via alias
					sd.fields = append(sd.fields, field{
						pyName:    snakeFromJSON(tag.name),
						pyType:    typ,
						jsonName:  tag.name,
						optional:  optional,
						omitempty: tag.omitempty,
					})
				}
			}
			emit.structs = append(emit.structs, sd)
		}
	}
	return emit, nil
}

// snakeFromJSON converts a JSON wire name (which is already lowercase /
// snake_case in this codebase) to a valid Python identifier. The wire
// names use snake_case throughout common/, so this is essentially a
// passthrough that also escapes Python keywords.
func snakeFromJSON(name string) string {
	if name == "" {
		return name
	}
	// All wire field names in common/ are already snake_case or short
	// single letters (s, t, w, p in QRPayload). Keep them as-is — the
	// Field(alias=...) mechanism reconciles them with attribute access.
	if isPyKeyword(name) {
		return name + "_"
	}
	return name
}

func isPyKeyword(s string) bool {
	switch s {
	case "from", "class", "def", "return", "import", "as", "is", "in", "and", "or", "not":
		return true
	}
	return false
}

// emitFile renders one Python module. nameToFile maps every struct name to
// the module it lives in, so this function can emit `from
// ghostcam.wire.<other> import X` for cross-file references.
func emitFile(emit *fileEmit, nameToFile map[string]string) string {
	var b strings.Builder
	b.WriteString("# Generated by tools/pydanticgen — do not edit.\n")
	b.WriteString("# Source: common/" + emit.srcFile + "\n")
	b.WriteString("from __future__ import annotations\n\n")
	b.WriteString("from pydantic import BaseModel, ConfigDict, Field\n")

	// Compute cross-file imports.
	local := emit.localNames()
	used := emit.usedTypeNames()
	imports := map[string][]string{} // module -> []name
	for name := range used {
		if _, isLocal := local[name]; isLocal {
			continue
		}
		mod, ok := nameToFile[name]
		if !ok {
			continue
		}
		if mod == emit.modName {
			continue
		}
		imports[mod] = append(imports[mod], name)
	}
	if len(imports) > 0 {
		mods := make([]string, 0, len(imports))
		for m := range imports {
			mods = append(mods, m)
		}
		sort.Strings(mods)
		b.WriteString("\n")
		for _, m := range mods {
			names := imports[m]
			sort.Strings(names)
			b.WriteString("from ghostcam.wire." + m + " import " + strings.Join(names, ", ") + "\n")
		}
	}
	b.WriteString("\n")

	for _, sd := range emit.structs {
		b.WriteString("\nclass " + sd.name + "(BaseModel):\n")
		if sd.doc != "" {
			b.WriteString("    \"\"\"" + escapeDoc(sd.doc) + "\"\"\"\n\n")
		}
		b.WriteString("    model_config = ConfigDict(populate_by_name=True)\n\n")
		if len(sd.fields) == 0 {
			b.WriteString("    pass\n")
			continue
		}
		for _, f := range sd.fields {
			pyType := f.pyType
			if f.optional || f.omitempty {
				pyType = pyType + " | None"
			}
			line := "    " + f.pyName + ": " + pyType
			alias := ""
			if f.jsonName != f.pyName {
				alias = ", alias=\"" + f.jsonName + "\""
			}
			if f.optional || f.omitempty {
				line += " = Field(default=None" + alias + ")"
			} else if alias != "" {
				line += " = Field(..." + alias + ")"
			}
			b.WriteString(line + "\n")
		}
	}

	// Resolve forward refs (intra-file ordering and string annotations from
	// `from __future__ import annotations`).
	if len(emit.structs) > 0 {
		b.WriteString("\n\n")
		for _, sd := range emit.structs {
			b.WriteString(sd.name + ".model_rebuild()\n")
		}
	}
	return b.String()
}

func escapeDoc(s string) string {
	return strings.ReplaceAll(s, "\"\"\"", "\\\"\\\"\\\"")
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: pydanticgen <input_dir> <output_dir>")
		os.Exit(2)
	}
	inDir := os.Args[1]
	outDir := os.Args[2]

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		die(err)
	}

	entries, err := os.ReadDir(inDir)
	if err != nil {
		die(err)
	}

	var srcFiles []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		srcFiles = append(srcFiles, filepath.Join(inDir, e.Name()))
	}
	sort.Strings(srcFiles)

	// Two-pass: first collect every struct name and its source module, so
	// per-file emission can produce cross-module imports.
	emits := make([]*fileEmit, 0, len(srcFiles))
	nameToFile := map[string]string{}
	for _, src := range srcFiles {
		emit, err := extractStructs(src)
		if err != nil {
			die(err)
		}
		if len(emit.structs) == 0 {
			continue
		}
		for _, sd := range emit.structs {
			nameToFile[sd.name] = emit.modName
		}
		emits = append(emits, emit)
	}

	for _, emit := range emits {
		outName := emit.modName + ".py"
		outPath := filepath.Join(outDir, outName)
		if err := os.WriteFile(outPath, []byte(emitFile(emit, nameToFile)), 0o644); err != nil {
			die(err)
		}
		fmt.Fprintf(os.Stderr, "pydanticgen: wrote %s (%d structs)\n", outPath, len(emit.structs))
	}
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "pydanticgen:", err)
	os.Exit(1)
}
