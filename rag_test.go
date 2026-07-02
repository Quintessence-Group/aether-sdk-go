package aether

import (
	"strings"
	"testing"
)

func makeResult(opts ...func(*RetrievalResult)) RetrievalResult {
	r := RetrievalResult{
		DocID:   "d1",
		Score:   90,
		Content: "full body",
	}
	for _, o := range opts {
		o(&r)
	}
	return r
}

func TestFormatContext_EmptyReturnsEmptyString(t *testing.T) {
	if got := FormatContext(nil); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestFormatContext_DefaultTemplateNumbersSourcesFromOne(t *testing.T) {
	results := []RetrievalResult{
		makeResult(func(r *RetrievalResult) { r.DocID = "a" }),
		makeResult(func(r *RetrievalResult) { r.DocID = "b" }),
	}
	out := FormatContext(results)
	if !strings.Contains(out, "[Source 1]") {
		t.Errorf("missing [Source 1] in %q", out)
	}
	if !strings.Contains(out, "[Source 2]") {
		t.Errorf("missing [Source 2] in %q", out)
	}
	if strings.Contains(out, "[Source 0]") {
		t.Errorf("unexpected [Source 0] in %q", out)
	}
}

func TestFormatContext_DefaultSeparatorIsBlankLine(t *testing.T) {
	results := []RetrievalResult{
		makeResult(func(r *RetrievalResult) { r.DocID = "a"; r.Content = "alpha" }),
		makeResult(func(r *RetrievalResult) { r.DocID = "b"; r.Content = "beta" }),
	}
	want := "[Source 1]\nalpha\n\n[Source 2]\nbeta"
	if got := FormatContext(results); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatContext_PrefersPassageOverContentByDefault(t *testing.T) {
	// Long-form docs: passage is the matched chunk; content is the whole doc.
	results := []RetrievalResult{
		makeResult(func(r *RetrievalResult) {
			r.Content = "100-page handbook"
			r.Passage = ptr("the matched paragraph")
		}),
	}
	out := FormatContext(results)
	if !strings.Contains(out, "the matched paragraph") {
		t.Errorf("expected matched paragraph in %q", out)
	}
	if strings.Contains(out, "100-page handbook") {
		t.Errorf("did not expect full handbook in %q", out)
	}
}

func TestFormatContext_FallsBackToContentWhenPassageMissing(t *testing.T) {
	// Short single-chunk inserts (the quickstart shape) have no separate passage.
	results := []RetrievalResult{
		makeResult(func(r *RetrievalResult) { r.Content = "short doc" }),
	}
	if out := FormatContext(results); !strings.Contains(out, "short doc") {
		t.Errorf("expected short doc in %q", out)
	}
}

func TestFormatContext_WithPreferContentUsesContent(t *testing.T) {
	results := []RetrievalResult{
		makeResult(func(r *RetrievalResult) {
			r.Content = "full body"
			r.Passage = ptr("chunk")
		}),
	}
	out := FormatContext(results, WithPreferContent())
	if !strings.Contains(out, "full body") {
		t.Errorf("expected full body in %q", out)
	}
	if strings.Contains(out, "chunk") {
		t.Errorf("did not expect chunk in %q", out)
	}
}

func TestFormatContext_CustomTemplateWithTitleAndScore(t *testing.T) {
	results := []RetrievalResult{
		makeResult(func(r *RetrievalResult) {
			r.DocID = "d1"
			r.Title = ptr("PTO policy")
			r.Score = 88
			r.Content = "20 days"
		}),
	}
	out := FormatContext(results, WithTemplate("<{title} | s={score}>\n{text}"))
	want := "<PTO policy | s=88>\n20 days"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestFormatContext_CustomTemplateFallsBackToDocIDWhenTitleMissing(t *testing.T) {
	results := []RetrievalResult{
		makeResult(func(r *RetrievalResult) { r.DocID = "d1"; r.Title = nil; r.Content = "body" }),
	}
	out := FormatContext(results, WithTemplate("[{title}] {text}"))
	want := "[d1] body"
	if out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestFormatContext_CustomSeparator(t *testing.T) {
	results := []RetrievalResult{
		makeResult(func(r *RetrievalResult) { r.Content = "a" }),
		makeResult(func(r *RetrievalResult) { r.Content = "b" }),
	}
	out := FormatContext(results, WithSeparator(" --- "))
	if !strings.Contains(out, " --- ") {
		t.Errorf("expected custom separator in %q", out)
	}
	if strings.Contains(out, "\n\n") {
		t.Errorf("did not expect default separator in %q", out)
	}
}

func TestFormatContext_RetrievalWithoutPassageRendersContent(t *testing.T) {
	// Retrieve() always populates Content; passage may be nil for short single-chunk inserts.
	results := []RetrievalResult{
		{DocID: "d1", Score: 90, Content: "search body"},
	}
	if out := FormatContext(results); !strings.Contains(out, "search body") {
		t.Errorf("expected search body in %q", out)
	}
}

func TestFormatContext_RetrievalWithBlankContentAndNoPassageRendersEmptyText(t *testing.T) {
	results := []RetrievalResult{
		{DocID: "d1", Score: 90, Content: ""},
	}
	out := FormatContext(results)
	if !strings.Contains(out, "[Source 1]\n") {
		t.Errorf("expected [Source 1] header in %q", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("expected trailing newline in %q", out)
	}
}
