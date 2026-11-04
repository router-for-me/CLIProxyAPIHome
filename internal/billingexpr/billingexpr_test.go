package billingexpr

import (
	"math"
	"strings"
	"testing"
	"time"
)

func evalUSD(t *testing.T, source string, numbers map[string]float64, strs map[string]string, now time.Time) float64 {
	t.Helper()
	program, errCompile := Compile(source)
	if errCompile != nil {
		t.Fatalf("Compile(%q) error = %v", source, errCompile)
	}
	if numbers == nil {
		numbers = map[string]float64{}
	}
	program.FoldTokens(numbers)
	value, errEval := program.EvalUSD(Env{Numbers: numbers, Strings: strs, Now: now})
	if errEval != nil {
		t.Fatalf("EvalUSD(%q) error = %v", source, errEval)
	}
	return value
}

func TestEvalTieredAndConditionalPricing(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 27, 2, 0, 0, 0, time.UTC) // 10:00 Asia/Shanghai, Tuesday
	tests := []struct {
		name    string
		source  string
		numbers map[string]float64
		strs    map[string]string
		want    float64
	}{
		{"linear", "p * 3 + c * 15", map[string]float64{"p": 1e6, "c": 2e6}, nil, 33},
		{"long context band", "len > 200000 ? p * 6 + c * 22.5 : p * 3 + c * 15", map[string]float64{"p": 1e6, "c": 1e6, "len": 250000}, nil, 28.5},
		{"tier and effort", "(tier == 'priority' ? 2 : 1) * (effort == 'XHIGH' ? 1.5 : 1) * p * 4", map[string]float64{"p": 1e6}, map[string]string{"tier": "priority", "effort": "xhigh"}, 12},
		{"peak hours", "(hour('Asia/Shanghai') >= 9 && hour('Asia/Shanghai') < 18 && weekday('Asia/Shanghai') >= 1 && weekday('Asia/Shanghai') <= 5 ? 1 : 0.5) * p * 2", map[string]float64{"p": 1e6}, nil, 2},
		{"per request fee", "p * 1 + req * usd(0.01)", map[string]float64{"p": 1e6, "req": 1}, nil, 1.01},
		{"min max contains", "max(p * 1, usd(0.5)) + (contains(model, 'mini') ? 0 : usd(1))", map[string]float64{"p": 1000}, map[string]string{"model": "gpt-5-MINI"}, 0.5},
		{"numeric separators", "p * 1_000 / 1e3", map[string]float64{"p": 1e6}, nil, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := evalUSD(t, tt.source, tt.numbers, tt.strs, now); math.Abs(got-tt.want) > 1e-9 {
				t.Fatalf("EvalUSD(%q) = %v, want %v", tt.source, got, tt.want)
			}
		})
	}
}

func TestFoldTokensNeverDropsUnreferencedBuckets(t *testing.T) {
	t.Parallel()

	numbers := map[string]float64{"p": 100, "c": 10, "r": 5, "cr": 50, "ct": 30, "cc": 20, "cc1h": 7, "cc12h": 3, "img": 40, "imgo": 2, "ai": 1, "ao": 1}
	// Only p and c are priced, so every other bucket folds into them.
	got := evalUSD(t, "p * 1 + c * 1", numbers, nil, time.Time{})
	if want := (100.0 + 50 + 30 + 20 + 7 + 3 + 40 + 1 + 10 + 5 + 2 + 1) / 1e6; math.Abs(got-want) > 1e-15 {
		t.Fatalf("folded = %v, want %v", got, want)
	}
	// Referenced buckets keep their own coefficient; cc12h folds into cc1h when cc1h is priced.
	numbers = map[string]float64{"p": 100, "cr": 50, "ct": 30, "cc1h": 7, "cc12h": 3, "r": 5, "c": 10}
	got = evalUSD(t, "p * 1 + cr * 0 + cc1h * 2 + c * 1 + r * 3", numbers, nil, time.Time{})
	if want := (100.0 + 0 + 20 + 10 + 15) / 1e6; math.Abs(got-want) > 1e-15 {
		t.Fatalf("partial fold = %v, want %v", got, want)
	}
}

func TestCompileAndValidateRejectInvalidExpressions(t *testing.T) {
	t.Parallel()

	for source, wantErr := range map[string]string{
		"":                       "empty",
		"p * ":                   "unexpected end",
		"p * unknown":            "unknown variable",
		"foo(p)":                 "unknown function",
		"hour('Mars/Base') * p":  "unknown timezone",
		"hour(model) * p":        "string literal",
		"usd(1, 2)":              "arguments",
		"p * 2 )":                "unexpected",
		"'abc'":                  "must evaluate to a number",
		"p > 1":                  "must evaluate to a number",
		"p - usd(1)":             "non-negative",
		"p / (c - c)":            "division by zero",
		"tier == 1 ? p : c":      "compares",
		"len ? p : c":            "condition must be a boolean",
		strings.Repeat("(", 100): "nesting",
		"p @ 2":                  "unexpected character",
	} {
		if errValidate := Validate(source); errValidate == nil || !strings.Contains(errValidate.Error(), wantErr) {
			t.Fatalf("Validate(%q) error = %v, want containing %q", source, errValidate, wantErr)
		}
	}
	if errValidate := Validate("len > 272000 ? p * 5 + c * 22.5 + cr * 0.5 : p * 2.5 + c * 15 + cr * 0.25"); errValidate != nil {
		t.Fatalf("Validate(valid) error = %v", errValidate)
	}
}
