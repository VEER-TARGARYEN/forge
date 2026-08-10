package repomap

import (
	"regexp"
	"strings"
)

// Def is one declaration found in a file.
type Def struct {
	Name string
	Kind string
	Line int
	Sig  string
}

// FileSyms is what one file defines and what identifiers it mentions.
type FileSyms struct {
	Path  string
	Defs  []Def
	Refs  map[string]int
	Lines int
}

type defPattern struct {
	re   *regexp.Regexp
	kind string
}

// Language extraction is regex-based, not a parser.
//
// A real parser (tree-sitter) would be more accurate, but it is a large
// dependency and the repo map only needs to be *ranked correctly*, not
// perfectly parsed: a handful of missed or spurious declarations moves a file
// a place or two in the ordering and changes nothing else. Being wrong here is
// cheap; being large is not.
var languages = map[string][]defPattern{
	".go": {
		{regexp.MustCompile(`^func\s+\(([^)]*)\)\s+([A-Za-z_]\w*)\s*\(`), "method"},
		{regexp.MustCompile(`^func\s+([A-Za-z_]\w*)\s*[\(\[]`), "func"},
		{regexp.MustCompile(`^type\s+([A-Za-z_]\w*)\s`), "type"},
		{regexp.MustCompile(`^(?:const|var)\s+([A-Za-z_]\w*)\s`), "var"},
	},
	".py": {
		{regexp.MustCompile(`^\s*class\s+([A-Za-z_]\w*)`), "class"},
		{regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)`), "func"},
	},
	".js":  jsLike,
	".jsx": jsLike,
	".mjs": jsLike,
	".ts":  tsLike,
	".tsx": tsLike,
	".rs": {
		{regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?(?:unsafe\s+)?fn\s+([A-Za-z_]\w*)`), "fn"},
		{regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?struct\s+([A-Za-z_]\w*)`), "struct"},
		{regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?enum\s+([A-Za-z_]\w*)`), "enum"},
		{regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?trait\s+([A-Za-z_]\w*)`), "trait"},
		{regexp.MustCompile(`^\s*impl(?:<[^>]*>)?\s+(?:[A-Za-z_]\w*\s+for\s+)?([A-Za-z_]\w*)`), "impl"},
	},
	".java": jvmLike,
	".cs":   jvmLike,
	".kt":   jvmLike,
	".c":    cLike,
	".h":    cLike,
	".cc":   cLike,
	".cpp":  cLike,
	".hpp":  cLike,
	".rb": {
		{regexp.MustCompile(`^\s*(?:class|module)\s+([A-Za-z_]\w*)`), "class"},
		{regexp.MustCompile(`^\s*def\s+(?:self\.)?([A-Za-z_]\w*[?!]?)`), "func"},
	},
	".php": {
		{regexp.MustCompile(`^\s*(?:abstract\s+|final\s+)?class\s+([A-Za-z_]\w*)`), "class"},
		{regexp.MustCompile(`^\s*(?:public\s+|private\s+|protected\s+|static\s+)*function\s+([A-Za-z_]\w*)`), "func"},
	},
	".swift": {
		{regexp.MustCompile(`^\s*(?:public\s+|private\s+|internal\s+|open\s+)?(?:class|struct|enum|protocol)\s+([A-Za-z_]\w*)`), "type"},
		{regexp.MustCompile(`^\s*(?:public\s+|private\s+|internal\s+|static\s+)*func\s+([A-Za-z_]\w*)`), "func"},
	},
}

var jsLike = []defPattern{
	{regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`), "func"},
	{regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`), "class"},
	{regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*=>`), "func"},
}

var tsLike = append([]defPattern{
	{regexp.MustCompile(`^\s*(?:export\s+)?interface\s+([A-Za-z_$][\w$]*)`), "interface"},
	{regexp.MustCompile(`^\s*(?:export\s+)?type\s+([A-Za-z_$][\w$]*)\s*[=<]`), "type"},
	{regexp.MustCompile(`^\s*(?:export\s+)?enum\s+([A-Za-z_$][\w$]*)`), "enum"},
}, jsLike...)

var jvmLike = []defPattern{
	{regexp.MustCompile(`^\s*(?:public|private|protected|internal|static|final|abstract|sealed|partial|open|data|\s)*(?:class|interface|enum|record|struct)\s+([A-Za-z_]\w*)`), "type"},
	{regexp.MustCompile(`^\s{1,}(?:public|private|protected|internal|static|final|virtual|override|abstract|async|suspend|fun|\s)+[\w<>\[\],.?\s]*?\s([A-Za-z_]\w*)\s*\([^;]*$`), "method"},
}

var cLike = []defPattern{
	{regexp.MustCompile(`^\s*(?:typedef\s+)?(?:struct|class|union|enum)\s+([A-Za-z_]\w*)`), "type"},
	{regexp.MustCompile(`^[A-Za-z_][\w\s\*&:<>,]*?\**\s*([A-Za-z_]\w*)\s*\([^;]*\)\s*\{?\s*$`), "func"},
	{regexp.MustCompile(`^\s*#define\s+([A-Z_][A-Z0-9_]*)`), "macro"},
}

// Supported reports whether a file extension has a def extractor.
func Supported(ext string) bool {
	_, ok := languages[strings.ToLower(ext)]
	return ok
}

// identRe is deliberately 3+ characters: shorter identifiers are dominated by
// loop variables and single-letter receivers, which add noise to the graph
// without carrying signal about which files matter.
var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{2,}`)

// stopWords are language keywords and ubiquitous stdlib names. Left in, they
// would link every file to every other file and flatten the ranking.
var stopWords = map[string]bool{}

func init() {
	words := `func return type var const import package interface struct map chan
		range defer nil true false else for switch case break continue default
		goto fallthrough select make new len cap append copy delete panic recover
		string int int8 int16 int32 int64 uint uint8 uint16 uint32 uint64 byte rune
		float32 float64 bool error any comparable
		def class self none pass elif lambda yield async await from with try except
		finally raise assert global nonlocal print list dict set tuple str repr
		function let export default extends implements typeof instanceof void
		null undefined this super new delete require module exports console
		public private protected static final abstract virtual override sealed
		partial namespace using enum record unsafe extern inline template
		fn impl trait pub mut crate use mod match where dyn
		if while do then end begin module include require
		std string_view size_t nullptr constexpr noexcept`
	for _, w := range strings.Fields(words) {
		stopWords[w] = true
	}
}

// Extract pulls declarations and referenced identifiers out of one file.
func Extract(path, ext, content string) *FileSyms {
	pats := languages[strings.ToLower(ext)]
	fs := &FileSyms{Path: path, Refs: map[string]int{}}

	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	fs.Lines = len(lines)
	own := map[string]bool{}

	for i, line := range lines {
		if len(line) > 500 {
			continue // minified or generated; nothing useful to extract
		}
		for _, p := range pats {
			m := p.re.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			// The last capture group is the name; earlier groups are context
			// such as a Go method receiver.
			name := m[len(m)-1]
			if name == "" || stopWords[name] {
				continue
			}
			fs.Defs = append(fs.Defs, Def{
				Name: name, Kind: p.kind, Line: i + 1, Sig: signature(line),
			})
			own[name] = true
			break // one declaration per line
		}
	}

	// References: every identifier mentioned that this file does not itself
	// declare. Self-references say nothing about which files depend on which.
	for _, line := range lines {
		if len(line) > 500 {
			continue
		}
		for _, id := range identRe.FindAllString(line, -1) {
			if stopWords[id] || own[id] {
				continue
			}
			fs.Refs[id]++
		}
	}
	return fs
}

// signature trims a declaration line down to something worth spending tokens
// on: no leading indentation, no trailing brace, capped length.
func signature(line string) string {
	s := strings.TrimSpace(line)
	s = strings.TrimRight(s, " \t{")
	s = strings.TrimRight(s, " \t")
	if i := strings.Index(s, "//"); i > 0 {
		s = strings.TrimRight(s[:i], " \t")
	}
	const max = 120
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
