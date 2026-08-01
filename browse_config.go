package janus

import (
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"math"
	"mime"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/template/parse"
	"time"
	"unicode/utf8"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/dustin/go-humanize"
)

const (
	browseDefaultTimeout     = 10 * time.Second
	browseDefaultMaxOutput   = int64(8 << 20)
	browseDefaultConcurrency = 4
	browseThemeMaxBytes      = int64(16 << 20)
	browseAssetPrefix        = "/_janus/browse/"
)

//go:embed browse/*
var embeddedBrowseTheme embed.FS

type BrowseSettings struct {
	Theme       string           `json:"theme,omitempty"`
	Timeout     *caddy.Duration  `json:"timeout,omitempty"`
	MaxOutput   *int64           `json:"max_output,omitempty"`
	Concurrency *int             `json:"concurrency,omitempty"`
	Renderers   []BrowseRenderer `json:"renderers,omitempty"`

	theme     *browseTheme
	renderers map[string]*browseRenderer
}

type BrowseRenderer struct {
	Extension   string          `json:"extension"`
	Command     []string        `json:"command"`
	ContentType string          `json:"content_type"`
	Timeout     *caddy.Duration `json:"timeout,omitempty"`
	MaxOutput   *int64          `json:"max_output,omitempty"`
	Concurrency *int            `json:"concurrency,omitempty"`
}

type BrowseSiteSettings struct {
	Enabled *bool        `json:"enabled,omitempty"`
	Roots   []BrowseRoot `json:"roots,omitempty"`
}

type BrowseRoot struct {
	Path  string `json:"path"`
	Cache string `json:"cache,omitempty"`
}

type browseRenderer struct {
	extension   string
	executable  string
	args        []string
	contentType string
	timeout     time.Duration
	maxOutput   int64
	concurrency int
	id          string
}

type browseThemeAsset struct {
	data        []byte
	contentType string
	etag        string
}

type browseTheme struct {
	hash     string
	template *template.Template
	assets   map[string]browseThemeAsset
}

type BrowsePage struct {
	Version     int
	Title       string
	Path        string
	RootName    string
	AssetBase   string
	Parent      *BrowseLink
	Breadcrumbs []BrowseLink
	Entries     []BrowseEntry
}

type BrowseLink struct {
	Name string
	URL  string
}

type BrowseEntry struct {
	Name         string
	URL          string
	RawURL       string
	Kind         string
	Icon         string
	Size         int64
	SizeText     string
	Modified     time.Time
	ModifiedText string
	PreviewURL   string
	Rendered     bool
}

func parseGlobalBrowseDirective(d *caddyfile.Dispenser) (*BrowseSettings, error) {
	if len(d.RemainingArgs()) != 0 {
		return nil, d.Err("global browse accepts no arguments; use browse or browse { ... }")
	}
	bs := &BrowseSettings{}
	seen := map[string]bool{}
	extensions := map[string]bool{}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		sub := d.Val()
		if sub != "renderer" {
			if seen[sub] {
				return nil, d.Errf("browse: duplicate subdirective %q", sub)
			}
			seen[sub] = true
		}
		switch sub {
		case "theme":
			value, err := oneDirectiveArg(d, "browse", sub)
			if err != nil {
				return nil, err
			}
			if !filepath.IsAbs(value) || filepath.Clean(value) != value {
				return nil, d.Errf("browse theme: want an absolute clean directory, got %q", value)
			}
			bs.Theme = value
		case "timeout":
			value, err := oneDirectiveArg(d, "browse", sub)
			if err != nil {
				return nil, err
			}
			duration, err := caddy.ParseDuration(value)
			if err != nil || duration <= 0 {
				return nil, d.Errf("browse timeout: want a positive duration, got %q", value)
			}
			v := caddy.Duration(duration)
			bs.Timeout = &v
		case "max_output":
			value, err := oneDirectiveArg(d, "browse", sub)
			if err != nil {
				return nil, err
			}
			size, err := parseBrowseSize(value)
			if err != nil || size == 0 || size >= uint64(math.MaxInt64) {
				return nil, d.Errf("browse max_output: want a positive size, got %q", value)
			}
			v := int64(size)
			bs.MaxOutput = &v
		case "concurrency":
			value, err := oneDirectiveArg(d, "browse", sub)
			if err != nil {
				return nil, err
			}
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return nil, d.Errf("browse concurrency: want a positive integer, got %q", value)
			}
			bs.Concurrency = &n
		case "renderer":
			renderer, err := parseBrowseRenderer(d)
			if err != nil {
				return nil, err
			}
			if extensions[renderer.Extension] {
				return nil, d.Errf("browse renderer: duplicate extension %q", renderer.Extension)
			}
			extensions[renderer.Extension] = true
			bs.Renderers = append(bs.Renderers, renderer)
		default:
			return nil, d.Errf("unrecognized browse subdirective: %s", sub)
		}
		if sub != "renderer" && d.NextBlock(d.Nesting()) {
			return nil, d.Errf("browse %s does not take a nested block", sub)
		}
	}
	return bs, nil
}

func parseBrowseRenderer(d *caddyfile.Dispenser) (BrowseRenderer, error) {
	var out BrowseRenderer
	args := d.RemainingArgs()
	if len(args) != 1 {
		return out, d.Err("browse renderer: want exactly one extension and a block")
	}
	extension, err := normalizeBrowseExtension(args[0])
	if err != nil {
		return out, d.Errf("browse renderer: %v", err)
	}
	out.Extension = extension
	nesting := d.Nesting()
	if !d.NextBlock(nesting) {
		return out, d.Errf("browse renderer %s: want a nonempty block", extension)
	}
	seen := map[string]bool{}
	for {
		sub := d.Val()
		if seen[sub] {
			return out, d.Errf("browse renderer %s: duplicate subdirective %q", extension, sub)
		}
		seen[sub] = true
		switch sub {
		case "command":
			out.Command = append([]string(nil), d.RemainingArgs()...)
			if err := validateRendererCommand(out.Command); err != nil {
				return out, d.Errf("browse renderer %s command: %v", extension, err)
			}
		case "content_type":
			value, err := oneDirectiveArg(d, "browse renderer", sub)
			if err != nil {
				return out, err
			}
			if _, _, err := mime.ParseMediaType(value); err != nil {
				return out, d.Errf("browse renderer %s content_type: %v", extension, err)
			}
			out.ContentType = value
		case "timeout":
			value, err := oneDirectiveArg(d, "browse renderer", sub)
			if err != nil {
				return out, err
			}
			duration, err := caddy.ParseDuration(value)
			if err != nil || duration <= 0 {
				return out, d.Errf("browse renderer %s timeout: want a positive duration, got %q", extension, value)
			}
			v := caddy.Duration(duration)
			out.Timeout = &v
		case "max_output":
			value, err := oneDirectiveArg(d, "browse renderer", sub)
			if err != nil {
				return out, err
			}
			size, err := parseBrowseSize(value)
			if err != nil || size == 0 || size >= uint64(math.MaxInt64) {
				return out, d.Errf("browse renderer %s max_output: want a positive size, got %q", extension, value)
			}
			v := int64(size)
			out.MaxOutput = &v
		case "concurrency":
			value, err := oneDirectiveArg(d, "browse renderer", sub)
			if err != nil {
				return out, err
			}
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return out, d.Errf("browse renderer %s concurrency: want a positive integer, got %q", extension, value)
			}
			out.Concurrency = &n
		default:
			return out, d.Errf("unrecognized browse renderer subdirective: %s", sub)
		}
		if d.NextBlock(d.Nesting()) {
			return out, d.Errf("browse renderer %s %s does not take a nested block", extension, sub)
		}
		if !d.NextBlock(nesting) {
			break
		}
	}
	if len(out.Command) == 0 {
		return out, d.Errf("browse renderer %s: command is required", extension)
	}
	if out.ContentType == "" {
		return out, d.Errf("browse renderer %s: content_type is required", extension)
	}
	return out, nil
}

func parseBrowseSize(value string) (uint64, error) {
	lower := strings.ToLower(value)
	for _, unit := range []string{"kb", "mb", "gb", "tb", "pb", "eb"} {
		if strings.HasSuffix(lower, unit) && !strings.HasSuffix(lower, "i"+unit) {
			value = value[:len(value)-len(unit)] + strings.ToUpper(unit[:1]) + "iB"
			break
		}
	}
	return humanize.ParseBytes(value)
}

func normalizeBrowseExtension(raw string) (string, error) {
	if len(raw) < 2 || raw[0] != '.' {
		return "", fmt.Errorf("extension %q must start with . and contain at least one character", raw)
	}
	for i := 1; i < len(raw); i++ {
		c := raw[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') &&
			(c < '0' || c > '9') && c != '_' && c != '-' && c != '.' {
			return "", fmt.Errorf("extension %q contains illegal byte %q", raw, c)
		}
	}
	return strings.ToLower(raw), nil
}

func validateRendererCommand(command []string) error {
	if len(command) == 0 || command[0] == "" {
		return fmt.Errorf("want an executable and arguments")
	}
	count := 0
	for i, arg := range command {
		if arg == "{file}" {
			if i == 0 {
				return fmt.Errorf("{file} cannot be argv zero")
			}
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("want exactly one argv token equal to {file}, got %d", count)
	}
	return nil
}

func parseSiteBrowseDirective(d *caddyfile.Dispenser) (*BrowseSiteSettings, error) {
	on, err := parseOnOff(d.RemainingArgs())
	if err != nil {
		return nil, d.Errf("browse: %v", err)
	}
	out := &BrowseSiteSettings{Enabled: &on}
	nesting := d.Nesting()
	if !d.NextBlock(nesting) {
		return out, nil
	}
	if !on {
		return nil, d.Err("browse off does not take a block")
	}
	seen := map[string]bool{}
	for {
		if d.Val() != "root" {
			switch d.Val() {
			case "theme", "renderer", "timeout", "max_output", "concurrency":
				return nil, d.Errf("browse %s is process-wide; configure it in the global janus options block", d.Val())
			default:
				return nil, d.Errf("unrecognized site browse subdirective: %s", d.Val())
			}
		}
		args := d.RemainingArgs()
		if len(args) < 1 || len(args) > 2 {
			return nil, d.Err("browse root: want an absolute path and optional cache policy")
		}
		cache := filesCacheRevalidate
		if len(args) == 2 {
			cache = args[1]
		}
		if err := validateBrowseRoot(args[0], cache); err != nil {
			return nil, d.Errf("browse root: %v", err)
		}
		if seen[args[0]] {
			return nil, d.Errf("browse root: duplicate root %q", args[0])
		}
		seen[args[0]] = true
		out.Roots = append(out.Roots, BrowseRoot{Path: args[0], Cache: cache})
		if d.NextBlock(d.Nesting()) {
			return nil, d.Err("browse root does not take a nested block")
		}
		if !d.NextBlock(nesting) {
			break
		}
	}
	return out, nil
}

func validateBrowseRoot(path, cache string) error {
	if err := validateConfiguredPath(path, "browse root", false); err != nil {
		return err
	}
	if cache == "" {
		cache = filesCacheRevalidate
	}
	switch cache {
	case filesCacheNever, filesCacheRevalidate, filesCacheForever:
		return nil
	default:
		return fmt.Errorf("root %q has invalid cache %q (want never, revalidate, or forever)", path, cache)
	}
}

func (a *App) provisionBrowse() error {
	bs := &BrowseSettings{}
	if a.Browse != nil {
		*bs = *a.Browse
		bs.Renderers = append([]BrowseRenderer(nil), a.Browse.Renderers...)
	}
	a.browseRuntime = bs
	if bs.Timeout != nil && *bs.Timeout <= 0 || bs.MaxOutput != nil && (*bs.MaxOutput <= 0 || *bs.MaxOutput == math.MaxInt64) ||
		bs.Concurrency != nil && *bs.Concurrency <= 0 {
		return fmt.Errorf("janus browse: limits must be positive")
	}
	theme, err := loadBrowseTheme(bs.Theme)
	if err != nil {
		return fmt.Errorf("janus browse theme: %w", err)
	}
	bs.theme = theme
	bs.renderers = make(map[string]*browseRenderer, len(bs.Renderers))
	defaultTimeout := browseDefaultTimeout
	defaultOutput := browseDefaultMaxOutput
	defaultConcurrency := browseDefaultConcurrency
	if bs.Timeout != nil {
		defaultTimeout = time.Duration(*bs.Timeout)
	}
	if bs.MaxOutput != nil {
		defaultOutput = *bs.MaxOutput
	}
	if bs.Concurrency != nil {
		defaultConcurrency = *bs.Concurrency
	}
	for _, configured := range bs.Renderers {
		extension, err := normalizeBrowseExtension(configured.Extension)
		if err != nil {
			return fmt.Errorf("janus browse renderer: %w", err)
		}
		if _, exists := bs.renderers[extension]; exists {
			return fmt.Errorf("janus browse renderer: duplicate extension %q", extension)
		}
		if err := validateRendererCommand(configured.Command); err != nil {
			return fmt.Errorf("janus browse renderer %s command: %w", extension, err)
		}
		if _, _, err := mime.ParseMediaType(configured.ContentType); err != nil {
			return fmt.Errorf("janus browse renderer %s content_type: %w", extension, err)
		}
		executable, err := exec.LookPath(configured.Command[0])
		if err != nil {
			return fmt.Errorf("janus browse renderer %s executable %q: %w", extension, configured.Command[0], err)
		}
		executable, err = filepath.Abs(executable)
		if err != nil {
			return fmt.Errorf("janus browse renderer %s executable: %w", extension, err)
		}
		renderer := &browseRenderer{
			extension: extension, executable: executable,
			args:        append([]string(nil), configured.Command[1:]...),
			contentType: configured.ContentType, timeout: defaultTimeout,
			maxOutput: defaultOutput, concurrency: defaultConcurrency,
		}
		if configured.Timeout != nil {
			renderer.timeout = time.Duration(*configured.Timeout)
		}
		if configured.MaxOutput != nil {
			renderer.maxOutput = *configured.MaxOutput
		}
		if configured.Concurrency != nil {
			renderer.concurrency = *configured.Concurrency
		}
		if renderer.timeout <= 0 || renderer.maxOutput <= 0 || renderer.maxOutput == math.MaxInt64 || renderer.concurrency <= 0 {
			return fmt.Errorf("janus browse renderer %s: limits must be positive", extension)
		}
		identity := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d\x00%d", extension, executable,
			configured.ContentType, renderer.timeout, renderer.maxOutput, renderer.concurrency)
		identity += "\x00" + strings.Join(renderer.args, "\x00")
		sum := sha256.Sum256([]byte(identity))
		renderer.id = fmt.Sprintf("%x", sum[:12])
		bs.renderers[extension] = renderer
	}
	return nil
}

func loadBrowseTheme(directory string) (*browseTheme, error) {
	var root fs.FS
	prefix := "."
	if directory == "" {
		root = embeddedBrowseTheme
		prefix = "browse"
	} else {
		info, err := os.Stat(directory)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", directory)
		}
		root = os.DirFS(directory)
	}
	assets := map[string]browseThemeAsset{}
	var total int64
	err := fs.WalkDir(root, prefix, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == prefix {
			return nil
		}
		relative := strings.TrimPrefix(strings.TrimPrefix(name, prefix), "/")
		if !utf8.ValidString(relative) {
			return fmt.Errorf("asset path %q is not valid UTF-8", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("asset %q is a symlink", relative)
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("asset %q is not a regular file", relative)
		}
		if info.Size() < 0 || info.Size() > browseThemeMaxBytes-total {
			return fmt.Errorf("theme exceeds 16 MiB")
		}
		file, err := root.Open(name)
		if err != nil {
			return err
		}
		limited := io.LimitReader(file, info.Size()+1)
		data := make([]byte, int(info.Size()))
		if _, err := io.ReadFull(limited, data); err != nil {
			file.Close()
			return fmt.Errorf("asset %q changed size while reading: %w", relative, err)
		}
		var extra [1]byte
		if count, err := limited.Read(extra[:]); count != 0 || err != nil && !errors.Is(err, io.EOF) {
			file.Close()
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			return fmt.Errorf("asset %q changed size while reading", relative)
		}
		if err := file.Close(); err != nil {
			return err
		}
		total += info.Size()
		sum := sha256.Sum256(data)
		assets[relative] = browseThemeAsset{
			data: data, contentType: browseAssetContentType(relative),
			etag: fmt.Sprintf(`"%x"`, sum[:]),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, required := range []string{"index.html", "browse.css", "browse.js", "icons.svg"} {
		if _, ok := assets[required]; !ok {
			return nil, fmt.Errorf("required asset %q is missing", required)
		}
	}
	tmpl, err := template.New("index.html").Parse(string(assets["index.html"].data))
	if err != nil {
		return nil, fmt.Errorf("index.html: %w", err)
	}
	if err := validateBrowseTemplate(tmpl); err != nil {
		return nil, fmt.Errorf("index.html: %w", err)
	}
	var paths []string
	for name := range assets {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	hash := sha256.New()
	var size [8]byte
	for _, name := range paths {
		binary.BigEndian.PutUint64(size[:], uint64(len(name)))
		hash.Write(size[:])
		hash.Write([]byte(name))
		binary.BigEndian.PutUint64(size[:], uint64(len(assets[name].data)))
		hash.Write(size[:])
		hash.Write(assets[name].data)
	}
	page := sentinelBrowsePage()
	if err := tmpl.ExecuteTemplate(io.Discard, "index.html", page); err != nil {
		return nil, fmt.Errorf("sentinel execution: %w", err)
	}
	return &browseTheme{hash: fmt.Sprintf("%x", hash.Sum(nil))[:24], template: tmpl, assets: assets}, nil
}

func browseAssetContentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml; charset=utf-8"
	default:
		if value := mime.TypeByExtension(strings.ToLower(filepath.Ext(name))); value != "" {
			return value
		}
		return "application/octet-stream"
	}
}

func validateBrowseTemplate(tmpl *template.Template) error {
	validator := browseTemplateValidator{
		template: tmpl,
		visited:  map[browseTemplateVisit]bool{},
		names:    map[string]bool{},
	}
	if tmpl.Lookup("index.html") == nil {
		return fmt.Errorf(`template "index.html" is missing`)
	}
	if err := validator.walkTemplate("index.html", reflect.TypeOf(BrowsePage{})); err != nil {
		return err
	}
	for _, candidate := range tmpl.Templates() {
		if !validator.names[candidate.Name()] {
			if err := validator.walkTemplate(candidate.Name(), reflect.TypeOf(BrowsePage{})); err != nil {
				return err
			}
		}
	}
	return nil
}

type browseTemplateVisit struct {
	name string
	dot  reflect.Type
}

type browseTemplateValidator struct {
	template *template.Template
	visited  map[browseTemplateVisit]bool
	names    map[string]bool
}

func (v *browseTemplateValidator) walkTemplate(name string, dot reflect.Type) error {
	visit := browseTemplateVisit{name: name, dot: dot}
	if v.visited[visit] {
		return nil
	}
	v.visited[visit] = true
	v.names[name] = true
	candidate := v.template.Lookup(name)
	if candidate == nil || candidate.Tree == nil || candidate.Tree.Root == nil {
		return fmt.Errorf("template %q has no tree", name)
	}
	if err := v.walkNode(candidate.Tree.Root, dot); err != nil {
		return fmt.Errorf("template %q: %w", name, err)
	}
	return nil
}

func (v *browseTemplateValidator) walkNode(node parse.Node, dot reflect.Type) error {
	if node == nil || reflect.ValueOf(node).Kind() == reflect.Pointer && reflect.ValueOf(node).IsNil() {
		return nil
	}
	switch n := node.(type) {
	case *parse.ListNode:
		for _, child := range n.Nodes {
			if err := v.walkNode(child, dot); err != nil {
				return err
			}
		}
	case *parse.ActionNode:
		return walkBrowsePipe(n.Pipe, dot)
	case *parse.RangeNode:
		next, err := browsePipeType(n.Pipe, dot)
		if err != nil {
			return err
		}
		next = indirectType(next)
		if next.Kind() == reflect.Slice || next.Kind() == reflect.Array {
			next = indirectType(next.Elem())
		}
		if err := v.walkNode(n.List, next); err != nil {
			return err
		}
		return v.walkNode(n.ElseList, dot)
	case *parse.WithNode:
		next, err := browsePipeType(n.Pipe, dot)
		if err != nil {
			return err
		}
		if err := v.walkNode(n.List, indirectType(next)); err != nil {
			return err
		}
		return v.walkNode(n.ElseList, dot)
	case *parse.IfNode:
		if _, err := browsePipeType(n.Pipe, dot); err != nil {
			return err
		}
		if err := v.walkNode(n.List, dot); err != nil {
			return err
		}
		return v.walkNode(n.ElseList, dot)
	case *parse.TemplateNode:
		next := dot
		if n.Pipe != nil {
			var err error
			next, err = browsePipeType(n.Pipe, dot)
			if err != nil {
				return err
			}
		}
		return v.walkTemplate(n.Name, next)
	}
	return nil
}

func walkBrowsePipe(pipe *parse.PipeNode, dot reflect.Type) error {
	_, err := browsePipeType(pipe, dot)
	return err
}

func browsePipeType(pipe *parse.PipeNode, dot reflect.Type) (reflect.Type, error) {
	current := dot
	for _, command := range pipe.Cmds {
		for _, arg := range command.Args {
			switch value := arg.(type) {
			case *parse.FieldNode:
				fieldType, err := browseFieldType(dot, value.Ident)
				if err != nil {
					return nil, err
				}
				current = fieldType
			case *parse.ChainNode:
				base := dot
				if field, ok := value.Node.(*parse.FieldNode); ok {
					var err error
					base, err = browseFieldType(dot, field.Ident)
					if err != nil {
						return nil, err
					}
				} else if variable, ok := value.Node.(*parse.VariableNode); ok &&
					len(variable.Ident) == 1 && variable.Ident[0] == "$" {
					base = reflect.TypeOf(BrowsePage{})
				}
				fieldType, err := browseFieldType(base, value.Field)
				if err != nil {
					return nil, err
				}
				current = fieldType
			case *parse.PipeNode:
				nested, err := browsePipeType(value, dot)
				if err != nil {
					return nil, err
				}
				current = nested
			}
		}
	}
	return current, nil
}

func browseFieldType(root reflect.Type, fields []string) (reflect.Type, error) {
	current := root
	for _, name := range fields {
		current = indirectType(current)
		if current.Kind() != reflect.Struct {
			return nil, fmt.Errorf("field chain .%s reaches non-struct %s", strings.Join(fields, "."), current)
		}
		field, ok := current.FieldByName(name)
		if !ok || !field.IsExported() {
			return nil, fmt.Errorf("unknown field .%s", strings.Join(fields, "."))
		}
		current = field.Type
	}
	return current, nil
}

func indirectType(value reflect.Type) reflect.Type {
	for value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	return value
}

func sentinelBrowsePage() BrowsePage {
	now := time.Unix(0, 0).UTC()
	return BrowsePage{
		Version: 1, Title: "Index of /", Path: "/", RootName: "root",
		AssetBase:   browseAssetPrefix + strings.Repeat("0", 24),
		Parent:      &BrowseLink{Name: "..", URL: "/"},
		Breadcrumbs: []BrowseLink{{Name: "root", URL: "/"}},
		Entries: []BrowseEntry{{
			Name: "file", URL: "/file", RawURL: "/file?raw", Kind: "file",
			Icon: "text", Size: 1, SizeText: "1 B", Modified: now,
			ModifiedText: now.Format(time.RFC3339), PreviewURL: "/file",
			Rendered: true,
		}},
	}
}

func (bs *BrowseSettings) matchRenderer(name string) *browseRenderer {
	if bs == nil {
		return nil
	}
	base := strings.ToLower(filepath.Base(name))
	var best *browseRenderer
	for extension, renderer := range bs.renderers {
		if len(base) <= len(extension) || !strings.HasSuffix(base, extension) {
			continue
		}
		if best == nil || len(extension) > len(best.extension) {
			best = renderer
		}
	}
	return best
}
