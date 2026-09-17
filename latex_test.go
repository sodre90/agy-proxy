package main

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
)

func filtered(s string) string {
	f := newLatexFilter()
	return f.Write(s) + f.Flush()
}

func TestLatexFilterRewritesMath(t *testing.T) {
	cases := []struct{ in, want string }{
		{`$K = 4$ experts cached`, "K = 4 experts cached"},
		{`top\text{-}k = 10`, "top-k = 10"},
		{`$\frac{a}{b} \times \text{-}k$`, "a/b × -k"},
		{`$\frac{-b \pm \sqrt{b^2 - 4ac}}{2a}$`, "(-b ± √(b² - 4ac))/2a"},
		{`hit rate is $K/E \approx 8.6\%$ here`, "hit rate is K/E ≈ 8.6% here"},
		{`$$E[x] = \sum_{i=1}^{n} x_i$$`, "E[x] = Σᵢ₌₁ⁿ xᵢ"},
		{`\(x \le y\) and \[a \ne b\]`, "x ≤ y and a ≠ b"},
		{`$x \in \mathbb{R}$, $\pi \approx 3.14$`, "x ∈ ℝ, π ≈ 3.14"},
		{`$\alpha_0$ vs $\beta^{10}$`, "α₀ vs β¹⁰"},
		{`$\left( a + b \right) / 2$`, "( a + b ) / 2"},
		{`$\mathrm{VRAM}_{used}$`, "VRAM_used"},

		// Nothing that is not math may be touched.
		{"it costs $5 and $10 total", "it costs $5 and $10 total"},
		{"the $100-$200 range", "the $100-$200 range"},
		{"echo $HOME then $PATH", "echo $HOME then $PATH"},
		{"set $HOME/$PATH and ${JAVA_HOME}/bin", "set $HOME/$PATH and ${JAVA_HOME}/bin"},
		{`C:\Users\name\Documents`, `C:\Users\name\Documents`},
		{`use \n for a newline`, `use \n for a newline`},
		{"`$K = 4$` stays verbatim", "`$K = 4$` stays verbatim"},
		{"``$K = 4$`` stays too", "``$K = 4$`` stays too"},
		{"```sh\necho $HOME\nx=$((1+2))\n```\n$K$", "```sh\necho $HOME\nx=$((1+2))\n```\nK"},
		{"a lone $ sign", "a lone $ sign"},
		{"$ spaced $ opener", "$ spaced $ opener"},
		{"unclosed $K = 4 forever", "unclosed $K = 4 forever"},
		{"across\n\na $blank line", "across\n\na $blank line"},
	}
	for _, c := range cases {
		if got := filtered(c.in); got != c.want {
			t.Errorf("filter(%q)\n got: %q\nwant: %q", c.in, got, c.want)
		}
	}
}

// A fenced block that is never closed must not disable filtering for the rest
// of the turn — the filter resets when the content block ends.
func TestLatexFilterResetsOnFlush(t *testing.T) {
	f := newLatexFilter()
	first := f.Write("```sh\necho $HOME\n") + f.Flush()
	second := f.Write("$K = 4$") + f.Flush()
	if first != "```sh\necho $HOME\n" {
		t.Errorf("fenced text was altered: %q", first)
	}
	if second != "K = 4" {
		t.Errorf("filter stayed in fence state across blocks: %q", second)
	}
}

// Every lookahead decision has to survive arriving one byte at a time, which is
// exactly what a streamed response does.
func TestLatexFilterIsIndependentOfChunkBoundaries(t *testing.T) {
	inputs := []string{
		`The cache holds $K = 4$ experts, so $\frac{K}{E} \approx 8.6\%$.`,
		"Run `echo $HOME` first.\n\n```py\nx = f\"${cost}\"\n```\n\nThen $x^2 + y^2 = r^2$.",
		`Costs $5, needs \text{-}k tuning, path C:\tmp\a, and $$\sum_{i}^{n} a_i$$ total.`,
		"unclosed `code and $math and \\fra",
		"the $100-$200 range with $K = 4$ too",
		"export $PWD/bin:$PATH while $K = 4$ holds",
		"```\n$K = 4$ inside a fence\n```\nand ``$K$`` inline, then $K$ live",
		"````md\n```\n$K = 4$\n```\n````\ndone $K$",
		"```\nx = ```\nstill inside\n```\n$K$",
		"$$display\nover\nlines$$ and \\(inline\\)",
	}
	for _, in := range inputs {
		want := filtered(in)
		for i := 0; i <= len(in); i++ {
			f := newLatexFilter()
			got := f.Write(in[:i]) + f.Write(in[i:]) + f.Flush()
			if got != want {
				t.Fatalf("split at %d of %q\n got: %q\nwant: %q", i, in, got, want)
			}
		}
		rng := rand.New(rand.NewSource(1))
		for trial := 0; trial < 200; trial++ {
			f := newLatexFilter()
			var b strings.Builder
			rest := in
			for rest != "" {
				n := rng.Intn(len(rest)) + 1
				b.WriteString(f.Write(rest[:n]))
				rest = rest[n:]
			}
			b.WriteString(f.Flush())
			if got := b.String(); got != want {
				t.Fatalf("random split of %q\n got: %q\nwant: %q", in, got, want)
			}
		}
	}
}

// Ordinary prose and code must cost nothing: no rewriting and no buffering, so
// the stream is not delayed for text that cannot contain math.
func TestLatexFilterHoldsNothingWithoutTriggers(t *testing.T) {
	f := newLatexFilter()
	for _, chunk := range []string{"The function ", "readAll(r io.Reader) ", "returns []byte.\n"} {
		if got := f.Write(chunk); got != chunk {
			t.Errorf("Write(%q) = %q, want it returned whole", chunk, got)
		}
	}
	if held := f.Flush(); held != "" {
		t.Errorf("filter held %q of trigger-free text", held)
	}
}

// A code block must stream out as it arrives rather than a line at a time,
// which is what holding every partial line for a possible closing fence would
// do — most of this proxy's output is code.
func TestLatexFilterDoesNotDelayCodeBlocks(t *testing.T) {
	f := newLatexFilter()
	for _, chunk := range []string{"```py\n", "x = f(", "y, z)\n", "print(x)\n", "```\n"} {
		if got := f.Write(chunk); got != chunk {
			t.Errorf("Write(%q) = %q, want it returned whole", chunk, got)
		}
	}
	if held := f.Flush(); held != "" {
		t.Errorf("filter held %q at the end of a code block", held)
	}
}

// The real hook order: text, then a tool call, then more text. Pending math
// must reach the client before its content block closes.
func TestStreamStateFlushesPendingMathBeforeToolUse(t *testing.T) {
	st := newStreamState(newSignatureStore(t.TempDir()))
	var events []streamEvent
	feed := func(parts string) {
		var chunk streamChunk
		raw := `{"response":{"candidates":[{"content":{"parts":` + parts + `}}]}}`
		if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
			t.Fatalf("fixture does not parse: %v", err)
		}
		events = append(events, st.Feed(chunk)...)
	}
	feed(`[{"text":"cached $K = 4"}]`)
	feed(`[{"text":"$ experts"}]`)
	feed(`[{"functionCall":{"name":"Bash","args":{"command":"ls"}}}]`)
	feed(`[{"text":"then $x^2$"}]`)
	events = append(events, st.closeAll()...)

	var text strings.Builder
	sawToolUse := false
	for _, ev := range events {
		data, _ := ev.Data.(map[string]any)
		if ev.Name == "content_block_start" {
			if block, _ := data["content_block"].(map[string]any); block["type"] == "tool_use" {
				if text.String() != "cached K = 4 experts" {
					t.Errorf("text block closed with %q, pending math not flushed", text.String())
				}
				sawToolUse = true
			}
		}
		if ev.Name != "content_block_delta" {
			continue
		}
		if delta, _ := data["delta"].(map[string]any); delta["type"] == "text_delta" {
			text.WriteString(delta["text"].(string))
		}
	}
	if !sawToolUse {
		t.Fatal("no tool_use block was emitted")
	}
	if got := text.String(); got != "cached K = 4 experts"+"then x²" {
		t.Errorf("text across blocks = %q", got)
	}
}
