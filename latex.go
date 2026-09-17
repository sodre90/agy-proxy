package main

import "strings"

// latexFilter rewrites LaTeX in a model's text stream as plain Unicode, so a
// terminal that cannot render math does not print the markup literally.
//
// Write takes an arbitrary slice of the stream and returns everything it has
// decided on; a trailing fragment whose meaning depends on bytes that have not
// arrived yet (a lone "$", a half-typed command, an unclosed span) is held back
// until the next Write or Flush. Code fences and inline code pass through
// untouched — a shell line is not mathematics.
type latexFilter struct {
	buf         string
	inFence     bool
	fenceLen    int
	atLineStart bool
}

// Spans longer than these are treated as literal text rather than held for a
// delimiter that may never arrive.
const (
	inlineMathLimit  = 400
	displayMathLimit = 4000
	inlineCodeLimit  = 1000
	commandLimit     = 512
)

func newLatexFilter() *latexFilter {
	return &latexFilter{atLineStart: true}
}

// A nil filter is the disabled filter: it passes everything through.
func (f *latexFilter) Write(s string) string {
	if f == nil {
		return s
	}
	f.buf += s
	out, consumed := f.scan(false)
	f.buf = f.buf[consumed:]
	return out
}

// Flush resolves whatever is still held as literal text and resets the filter
// for the next content block.
func (f *latexFilter) Flush() string {
	if f == nil {
		return ""
	}
	out, _ := f.scan(true)
	f.buf = ""
	f.inFence, f.fenceLen, f.atLineStart = false, 0, true
	return out
}

func (f *latexFilter) scan(final bool) (string, int) {
	var out strings.Builder
	emit := func(s string) {
		if s == "" {
			return
		}
		out.WriteString(s)
		if nl := strings.LastIndexByte(s, '\n'); nl >= 0 {
			s = s[nl+1:]
			f.atLineStart = true
		}
		if strings.TrimLeft(s, " \t") != "" {
			f.atLineStart = false
		}
	}

	i := 0
	for i < len(f.buf) {
		if f.inFence {
			end, complete := lineEnd(f.buf, i)
			if !complete && !final && f.atLineStart && couldCloseFence(f.buf[i:]) {
				return out.String(), i
			}
			if f.atLineStart && closesFence(f.buf[i:end], f.fenceLen) {
				f.inFence = false
			}
			emit(f.buf[i:end])
			i = end
			continue
		}
		switch f.buf[i] {
		case '`':
			consumed, ok := f.scanCode(i, final)
			if !ok {
				return out.String(), i
			}
			emit(f.buf[i : i+consumed])
			i += consumed
		case '$':
			consumed, text, ok := f.scanDollarMath(i, final)
			if !ok {
				return out.String(), i
			}
			emit(text)
			i += consumed
		case '\\':
			consumed, text, ok := f.scanCommand(i, final)
			if !ok {
				return out.String(), i
			}
			emit(text)
			i += consumed
		default:
			j := i + 1
			for j < len(f.buf) && !isTrigger(f.buf[j]) {
				j++
			}
			emit(f.buf[i:j])
			i = j
		}
	}
	return out.String(), i
}

func isTrigger(c byte) bool {
	return c == '`' || c == '$' || c == '\\'
}

// scanCode consumes a fence marker or a whole inline code span verbatim.
func (f *latexFilter) scanCode(i int, final bool) (int, bool) {
	n := 0
	for i+n < len(f.buf) && f.buf[i+n] == '`' {
		n++
	}
	if i+n == len(f.buf) && !final {
		return 0, false
	}
	if f.atLineStart && n >= 3 {
		f.inFence = true
		f.fenceLen = n
		end, _ := lineEnd(f.buf, i)
		return end - i, true
	}
	for j := i + n; j < len(f.buf); {
		switch f.buf[j] {
		case '\n':
			return n, true
		case '`':
			run := 0
			for j+run < len(f.buf) && f.buf[j+run] == '`' {
				run++
			}
			if j+run == len(f.buf) && !final {
				return 0, false
			}
			if run == n {
				return j + run - i, true
			}
			j += run
		default:
			j++
		}
	}
	if final || len(f.buf)-i > inlineCodeLimit {
		return n, true
	}
	return 0, false
}

// scanDollarMath applies the pandoc-style rules for what a "$" delimits: an
// opening "$" is followed by neither a space nor a digit, and a closing "$" is
// not preceded by a space. Prose about money is far more common than a math
// span that opens on a bare numeral, so "costs $5 and $10" stays as typed.
func (f *latexFilter) scanDollarMath(i int, final bool) (int, string, bool) {
	if i+1 >= len(f.buf) {
		if !final {
			return 0, "", false
		}
		return 1, "$", true
	}
	if f.buf[i+1] == '$' {
		if close := strings.Index(f.buf[i+2:], "$$"); close >= 0 {
			return close + 4, renderMath(f.buf[i+2 : i+2+close]), true
		}
		if final || len(f.buf)-i > displayMathLimit {
			return 2, "$$", true
		}
		return 0, "", false
	}
	if isSpaceByte(f.buf[i+1]) || isDigit(f.buf[i+1]) || looksLikeShellVariable(f.buf[i+1:]) {
		return 1, "$", true
	}
	for j := i + 1; j < len(f.buf); j++ {
		switch f.buf[j] {
		case '\\':
			j++
			continue
		case '\n':
			if j+1 < len(f.buf) && f.buf[j+1] == '\n' {
				return 1, "$", true
			}
			continue
		case '$':
			if isSpaceByte(f.buf[j-1]) {
				continue
			}
			return j + 1 - i, renderMath(f.buf[i+1 : j]), true
		}
	}
	if final || len(f.buf)-i > inlineMathLimit {
		return 1, "$", true
	}
	return 0, "", false
}

// A dollar followed by a SHELL_STYLE name is an environment variable, not
// mathematics: math variables are single letters or start with a command. The
// name can only grow as more of the stream arrives, so a prefix decides this.
func looksLikeShellVariable(s string) bool {
	if strings.HasPrefix(s, "{") {
		s = s[1:]
	}
	n := 0
	for n < len(s) && (s[n] >= 'A' && s[n] <= 'Z' || s[n] == '_' || n > 0 && isDigit(s[n])) {
		n++
	}
	return n >= 2
}

// commandsOutsideMath is deliberately tiny: outside a math span a backslash is
// far more likely to be a Windows path or an escape sequence than markup, so
// only forms that have no other meaning are rewritten.
var commandsOutsideMath = map[string]bool{
	"text": true, "mathrm": true, "mathbf": true, "mathit": true,
	"operatorname": true, "frac": true, "dfrac": true, "tfrac": true,
}

func (f *latexFilter) scanCommand(i int, final bool) (int, string, bool) {
	if i+1 >= len(f.buf) {
		if !final {
			return 0, "", false
		}
		return 1, "\\", true
	}
	switch f.buf[i+1] {
	case '(', '[':
		closing := "\\)"
		if f.buf[i+1] == '[' {
			closing = "\\]"
		}
		if close := strings.Index(f.buf[i+2:], closing); close >= 0 {
			return close + 4, renderMath(f.buf[i+2 : i+2+close]), true
		}
		if final || len(f.buf)-i > displayMathLimit {
			return 2, f.buf[i : i+2], true
		}
		return 0, "", false
	}
	if !isLetter(f.buf[i+1]) {
		return 2, f.buf[i : i+2], true
	}
	name, after := readName(f.buf, i+1)
	if after == len(f.buf) && !final {
		return 0, "", false
	}
	if !commandsOutsideMath[name] {
		return after - i, f.buf[i:after], true
	}
	args, end, ok := readArgs(f.buf, after, argCount(name))
	if !ok {
		if final || len(f.buf)-i > commandLimit {
			return after - i, f.buf[i:after], true
		}
		return 0, "", false
	}
	return end - i, applyCommand(name, args), true
}

func argCount(name string) int {
	switch name {
	case "frac", "dfrac", "tfrac":
		return 2
	}
	return 1
}

func applyCommand(name string, args []string) string {
	switch name {
	case "frac", "dfrac", "tfrac":
		return formatFraction(renderMath(args[0]), renderMath(args[1]))
	}
	return renderMath(args[0])
}

// renderMath rewrites the inside of a math span, where every backslash is
// markup and unknown commands can safely lose their backslash.
func renderMath(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '\\':
			consumed, text := renderCommand(s, i)
			b.WriteString(text)
			i += consumed
		case c == '{' || c == '}':
			i++
		case c == '^' || c == '_':
			consumed, text := renderScript(s, i)
			b.WriteString(text)
			i += consumed
		case c == '&' || c == '~':
			b.WriteByte(' ')
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return collapseSpaces(b.String())
}

func renderCommand(s string, i int) (int, string) {
	if i+1 >= len(s) {
		return 1, ""
	}
	if c := s[i+1]; !isLetter(c) {
		switch c {
		case '\\':
			return 2, "\n"
		case ',', ';', ':', '!', ' ', '\n':
			return 2, " "
		default:
			return 2, string(c)
		}
	}
	name, after := readName(s, i+1)
	switch name {
	case "text", "mathrm", "mathbf", "mathit", "mathsf", "mathtt", "textrm",
		"textbf", "textit", "operatorname", "mbox", "hbox":
		if args, end, ok := readArgs(s, after, 1); ok {
			return end - i, renderMath(args[0])
		}
	case "mathbb":
		if args, end, ok := readArgs(s, after, 1); ok {
			return end - i, blackboard(args[0])
		}
	case "frac", "dfrac", "tfrac":
		if args, end, ok := readArgs(s, after, 2); ok {
			return end - i, formatFraction(renderMath(args[0]), renderMath(args[1]))
		}
	case "sqrt":
		if args, end, ok := readArgs(s, after, 1); ok {
			return end - i, "√" + parenthesizeCompound(renderMath(args[0]))
		}
		return after - i, "√"
	case "left", "right", "bigl", "bigr", "Bigl", "Bigr", "big", "Big":
		if after < len(s) && s[after] == '.' {
			return after + 1 - i, ""
		}
		return after - i, ""
	case "begin", "end":
		if _, end, ok := readArgs(s, after, 1); ok {
			return end - i, "\n"
		}
	}
	if symbol, ok := mathSymbols[name]; ok {
		return after - i, symbol
	}
	return after - i, name
}

func renderScript(s string, i int) (int, string) {
	arg, end := "", i+1
	switch {
	case end < len(s) && s[end] == '{':
		args, groupEnd, ok := readArgs(s, end, 1)
		if !ok {
			return 1, string(s[i])
		}
		arg, end = renderMath(args[0]), groupEnd
	case end < len(s):
		arg, end = string(s[end]), end+1
	default:
		return 1, string(s[i])
	}
	table := superscripts
	if s[i] == '_' {
		table = subscripts
	}
	if mapped, ok := mapAll(arg, table); ok {
		return end - i, mapped
	}
	return end - i, string(s[i]) + parenthesizeCompound(arg)
}

// readArgs reads n consecutive brace groups, reporting failure when the text
// runs out mid-group so the caller can wait for more of the stream.
func readArgs(s string, i, n int) ([]string, int, bool) {
	args := make([]string, 0, n)
	for len(args) < n {
		for i < len(s) && (s[i] == ' ' || s[i] == '\n') {
			i++
		}
		if i >= len(s) || s[i] != '{' {
			return nil, i, false
		}
		depth, j := 1, i+1
		for ; j < len(s) && depth > 0; j++ {
			switch s[j] {
			case '\\':
				j++
			case '{':
				depth++
			case '}':
				depth--
			}
		}
		if depth > 0 {
			return nil, j, false
		}
		args = append(args, s[i+1:j-1])
		i = j
	}
	return args, i, true
}

func readName(s string, i int) (string, int) {
	j := i
	for j < len(s) && isLetter(s[j]) {
		j++
	}
	return s[i:j], j
}

func formatFraction(numerator, denominator string) string {
	return parenthesizeCompound(numerator) + "/" + parenthesizeCompound(denominator)
}

func parenthesizeCompound(s string) string {
	for i := 0; i < len(s); i++ {
		if !isLetter(s[i]) && !isDigit(s[i]) && s[i] != '.' && s[i] != '_' && s[i] < 0x80 {
			return "(" + s + ")"
		}
	}
	return s
}

func blackboard(s string) string {
	switch strings.TrimSpace(s) {
	case "R":
		return "ℝ"
	case "N":
		return "ℕ"
	case "Z":
		return "ℤ"
	case "Q":
		return "ℚ"
	case "C":
		return "ℂ"
	}
	return renderMath(s)
}

func mapAll(s string, table map[rune]rune) (string, bool) {
	if s == "" {
		return "", false
	}
	var b strings.Builder
	for _, r := range s {
		mapped, ok := table[r]
		if !ok {
			return "", false
		}
		b.WriteRune(mapped)
	}
	return b.String(), true
}

func collapseSpaces(s string) string {
	var b strings.Builder
	pendingSpace := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t':
			pendingSpace = true
		case '\n':
			pendingSpace = false
			b.WriteByte('\n')
		default:
			if pendingSpace && b.Len() > 0 && b.String()[b.Len()-1] != '\n' {
				b.WriteByte(' ')
			}
			pendingSpace = false
			b.WriteByte(s[i])
		}
	}
	return strings.Trim(b.String(), " \t\n")
}

func lineEnd(s string, i int) (int, bool) {
	if nl := strings.IndexByte(s[i:], '\n'); nl >= 0 {
		return i + nl + 1, true
	}
	return len(s), false
}

func closesFence(line string, fenceLen int) bool {
	trimmed := strings.TrimSpace(line)
	return len(trimmed) >= fenceLen && strings.Trim(trimmed, "`") == ""
}

// couldCloseFence reports whether a line this far in might still turn out to be
// a closing fence. Only those lines are worth holding — the rest of a code
// block is emitted as it arrives, so a long code block is not painted a line at
// a time.
func couldCloseFence(partialLine string) bool {
	return strings.Trim(strings.TrimLeft(partialLine, " \t"), "`") == ""
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

var superscripts = map[rune]rune{
	'0': '⁰', '1': '¹', '2': '²', '3': '³', '4': '⁴', '5': '⁵', '6': '⁶',
	'7': '⁷', '8': '⁸', '9': '⁹', '+': '⁺', '-': '⁻', '=': '⁼', '(': '⁽',
	')': '⁾', 'n': 'ⁿ', 'i': 'ⁱ',
}

var subscripts = map[rune]rune{
	'0': '₀', '1': '₁', '2': '₂', '3': '₃', '4': '₄', '5': '₅', '6': '₆',
	'7': '₇', '8': '₈', '9': '₉', '+': '₊', '-': '₋', '=': '₌', '(': '₍',
	')': '₎', 'a': 'ₐ', 'e': 'ₑ', 'h': 'ₕ', 'i': 'ᵢ', 'j': 'ⱼ', 'k': 'ₖ',
	'l': 'ₗ', 'm': 'ₘ', 'n': 'ₙ', 'o': 'ₒ', 'p': 'ₚ', 'r': 'ᵣ', 's': 'ₛ',
	't': 'ₜ', 'u': 'ᵤ', 'v': 'ᵥ', 'x': 'ₓ',
}

var mathSymbols = map[string]string{
	"alpha": "α", "beta": "β", "gamma": "γ", "delta": "δ", "epsilon": "ε",
	"varepsilon": "ε", "zeta": "ζ", "eta": "η", "theta": "θ", "iota": "ι",
	"kappa": "κ", "lambda": "λ", "mu": "μ", "nu": "ν", "xi": "ξ", "pi": "π",
	"rho": "ρ", "sigma": "σ", "tau": "τ", "upsilon": "υ", "phi": "φ",
	"varphi": "φ", "chi": "χ", "psi": "ψ", "omega": "ω",
	"Gamma": "Γ", "Delta": "Δ", "Theta": "Θ", "Lambda": "Λ", "Xi": "Ξ",
	"Pi": "Π", "Sigma": "Σ", "Phi": "Φ", "Psi": "Ψ", "Omega": "Ω",

	"times": "×", "div": "÷", "cdot": "·", "pm": "±", "mp": "∓",
	"approx": "≈", "sim": "∼", "simeq": "≃", "cong": "≅", "equiv": "≡",
	"neq": "≠", "ne": "≠", "leq": "≤", "le": "≤", "geq": "≥", "ge": "≥",
	"ll": "≪", "gg": "≫", "propto": "∝",

	"sum": "Σ", "prod": "∏", "int": "∫", "iint": "∬", "oint": "∮",
	"partial": "∂", "nabla": "∇", "infty": "∞", "sqrt": "√",
	"forall": "∀", "exists": "∃", "nexists": "∄", "in": "∈", "notin": "∉",
	"subset": "⊂", "subseteq": "⊆", "supset": "⊃", "supseteq": "⊇",
	"cup": "∪", "cap": "∩", "emptyset": "∅", "setminus": "\\",

	"to": "→", "rightarrow": "→", "leftarrow": "←", "Rightarrow": "⇒",
	"Leftarrow": "⇐", "leftrightarrow": "↔", "Leftrightarrow": "⇔",
	"mapsto": "↦", "implies": "⇒",

	"ldots": "…", "cdots": "⋯", "dots": "…", "vdots": "⋮", "ddots": "⋱",
	"lfloor": "⌊", "rfloor": "⌋", "lceil": "⌈", "rceil": "⌉",
	"langle": "⟨", "rangle": "⟩", "|": "|",
	"quad": " ", "qquad": "  ", "space": " ",
}
