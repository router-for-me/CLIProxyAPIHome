// Package billingexpr implements a small, dependency-free pricing expression
// language. An expression evaluates to USD per million tokens summed over token
// variables, so "p * 3 + c * 15" bills $3/M input and $15/M output. usd(x) adds a
// fixed USD amount. Token buckets that an expression does not reference are folded
// into their parent bucket before evaluation, so a missing term never undercharges.
package billingexpr

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	maxSourceLength = 4096
	maxDepth        = 64
)

// Token variables, in tokens. Their coefficients are USD per million tokens.
const (
	VarInput                  = "p"           // uncached text input
	VarOutput                 = "c"           // non-reasoning text output
	VarReasoning              = "r"           // reasoning output
	VarCacheRead              = "cr"          // cache reads
	VarCacheWrite             = "cc"          // 5-minute cache writes
	VarCacheWrite30m          = "cc30m"       // 30-minute cache writes
	VarCacheWrite1h           = "cc1h"        // 1-hour cache writes
	VarCacheWrite12h          = "cc12h"       // 12-hour cache writes
	VarImageInput             = "img"         // image input
	VarImageOutput            = "imgo"        // image output
	VarAudioInput             = "ai"          // audio input
	VarAudioOutput            = "ao"          // audio output
	VarVideoInput             = "vi"          // video input
	VarVideoOutput            = "vo"          // video output
	VarFileInput              = "fi"          // file/document input
	VarImageCacheRead         = "img_cr"      // cached image input tokens
	VarAudioCacheRead         = "acr"         // cached audio input tokens
	VarCachedText             = "ct"          // cached text input tokens
	VarAcceptedPrediction     = "apred"       // accepted prediction tokens
	VarRejectedPrediction     = "rpred"       // rejected prediction tokens
	VarTotalTokens            = "total"       // total input + output tokens
	VarContextLength          = "len"         // full prompt context, for conditions only
	VarRequest                = "req"         // 1 on success, 0 on failure; "req * usd(0.01)" bills per request
	VarWebSearchCalls         = "ws"          // web search calls; bill with "ws * usd(0.01)"
	VarWebSearchQueries       = "wsq"         // web search queries
	VarWebFetchCalls          = "wf"          // web fetch calls
	VarXSearchCalls           = "xs"          // X search calls
	VarCodeExecutionCalls     = "ce"          // code execution / interpreter calls
	VarFileSearchCalls        = "fs"          // file / collections search calls
	VarDocumentSearchCalls    = "ds"          // document / attachment search calls
	VarMCPCalls               = "mcp"         // remote MCP tool calls
	VarGoogleMapsChunks       = "maps"        // Gemini Google Maps grounding chunks
	VarNumSourcesUsed         = "src"         // sources used (e.g. xAI Live Search)
	VarNumServerSideToolsUsed = "st"          // total server-side tool invocations
	VarPromptMedia            = "pmedia"      // prompt media count
	VarCompletionMedia        = "cmedia"      // completion media count
	VarPromptAudio            = "paudio"      // prompt audio count
	VarImages                 = "imgn"        // generated images; bill with "imgn * usd(0.04)"
	VarImageCountAlias        = "image_count" // new-api alias for imgn
	VarImageOutputAlias       = "img_o"       // new-api alias for imgo
	VarAudioSeconds           = "asec"        // audio/video duration in seconds; bill with "asec * usd(0.001)"
	VarUpstreamCost           = "upcost"      // upstream reported cost in USD; bill with "usd(upcost)"
	VarTier                   = "tier"
	VarUpstreamTier           = "upstream_tier"
	VarSpeed                  = "speed"
	VarGeo                    = "geo"
	VarCacheTTL               = "cache_ttl"
	VarImageSize              = "img_size"
	VarImageResolution        = "img_res"
	VarImageQuality           = "img_quality"
	VarImageAspectRatio       = "img_ratio"
	VarImageTier              = "img_tier"
	VarBYOK                   = "byok"
	VarResponseModel          = "response_model"
	VarEffort                 = "effort"
	VarModel                  = "model"
	VarProvider               = "provider"
	VarEndpoint               = "endpoint"
	tokensPerMillion          = 1000000.0
	defaultSampleSize         = 1000
)

// varAliases maps compatibility aliases (such as new-api names) to canonical variable names.
var varAliases = map[string]string{
	VarImageCountAlias:  VarImages,
	VarImageOutputAlias: VarImageOutput,
	"ai_cr":             VarAudioCacheRead,
	"ci":                VarCodeExecutionCalls,
}

func canonicalVarName(name string) string {
	if canonical, ok := varAliases[name]; ok {
		return canonical
	}
	return name
}

var numberVars = map[string]struct{}{
	VarInput: {}, VarOutput: {}, VarReasoning: {}, VarCacheRead: {}, VarCacheWrite: {},
	VarCacheWrite30m: {}, VarCacheWrite1h: {}, VarCacheWrite12h: {}, VarImageInput: {}, VarImageOutput: {},
	VarAudioInput: {}, VarAudioOutput: {}, VarVideoInput: {}, VarVideoOutput: {}, VarFileInput: {},
	VarImageCacheRead: {}, VarAudioCacheRead: {}, VarCachedText: {},
	VarAcceptedPrediction: {}, VarRejectedPrediction: {}, VarTotalTokens: {},
	VarContextLength: {}, VarRequest: {}, VarWebSearchCalls: {}, VarWebSearchQueries: {},
	VarWebFetchCalls: {}, VarXSearchCalls: {}, VarCodeExecutionCalls: {}, VarFileSearchCalls: {},
	VarDocumentSearchCalls: {}, VarMCPCalls: {}, VarGoogleMapsChunks: {},
	VarNumSourcesUsed: {}, VarNumServerSideToolsUsed: {},
	VarPromptMedia: {}, VarCompletionMedia: {}, VarPromptAudio: {},
	VarImages: {}, VarAudioSeconds: {}, VarUpstreamCost: {},
}

var stringVars = map[string]struct{}{
	VarTier: {}, VarUpstreamTier: {}, VarSpeed: {}, VarGeo: {}, VarCacheTTL: {},
	VarImageSize: {}, VarImageResolution: {}, VarImageQuality: {}, VarImageAspectRatio: {}, VarImageTier: {},
	VarBYOK: {}, VarResponseModel: {}, VarEffort: {}, VarModel: {}, VarProvider: {}, VarEndpoint: {},
}

type function struct {
	minArgs int
	maxArgs int
}

var functions = map[string]function{
	"hour":     {minArgs: 0, maxArgs: 1},
	"minute":   {minArgs: 0, maxArgs: 1},
	"weekday":  {minArgs: 0, maxArgs: 1},
	"month":    {minArgs: 0, maxArgs: 1},
	"day":      {minArgs: 0, maxArgs: 1},
	"min":      {minArgs: 1, maxArgs: 16},
	"max":      {minArgs: 1, maxArgs: 16},
	"abs":      {minArgs: 1, maxArgs: 1},
	"ceil":     {minArgs: 1, maxArgs: 1},
	"floor":    {minArgs: 1, maxArgs: 1},
	"round":    {minArgs: 1, maxArgs: 1},
	"usd":      {minArgs: 1, maxArgs: 1},
	"fixed":    {minArgs: 1, maxArgs: 1},
	"tier":     {minArgs: 2, maxArgs: 2},
	"contains": {minArgs: 2, maxArgs: 2},
	"has":      {minArgs: 2, maxArgs: 2},
	"u":        {minArgs: 1, maxArgs: 1},
	"s":        {minArgs: 1, maxArgs: 1},
	"param":    {minArgs: 1, maxArgs: 1},
}

// Env supplies variable values, dynamic lookup hooks, and the evaluation clock.
type Env struct {
	Numbers      map[string]float64
	Strings      map[string]string
	LookupNumber func(path string) float64
	LookupString func(path string) string
	Now          time.Time
	MatchedTier  string
}

// Program is a compiled expression.
type Program struct {
	source string
	root   node
	uses   map[string]bool
}

// Source returns the original expression text.
func (p *Program) Source() string { return p.source }

// Uses reports whether the expression references a variable.
func (p *Program) Uses(name string) bool { return p != nil && p.uses[name] }

var programCache sync.Map

// Compile parses and checks an expression. Compiled programs are cached by source.
func Compile(source string) (*Program, error) {
	source = strings.TrimSpace(source)
	if cached, ok := programCache.Load(source); ok {
		return cached.(*Program), nil
	}
	body := strings.TrimSpace(strings.TrimPrefix(source, "v1:"))
	if body == "" {
		return nil, fmt.Errorf("expression is empty")
	}
	if len(source) > maxSourceLength {
		return nil, fmt.Errorf("expression exceeds %d characters", maxSourceLength)
	}
	tokens, errLex := lex(body)
	if errLex != nil {
		return nil, errLex
	}
	parser := &parser{tokens: tokens, uses: make(map[string]bool)}
	root, errParse := parser.parseExpression(0)
	if errParse != nil {
		return nil, errParse
	}
	if parser.peek().kind != tokenEOF {
		return nil, fmt.Errorf("unexpected %q at position %d", parser.peek().text, parser.peek().pos)
	}
	program := &Program{source: source, root: root, uses: parser.uses}
	programCache.Store(source, program)
	return program, nil
}

// Validate compiles an expression and evaluates it against sample usage. The
// result must be a finite, non-negative number.
func Validate(source string) error {
	program, errCompile := Compile(source)
	if errCompile != nil {
		return errCompile
	}
	sample := Env{Numbers: map[string]float64{}, Strings: map[string]string{}, Now: time.Now().UTC()}
	for name := range numberVars {
		sample.Numbers[name] = defaultSampleSize
	}
	for _, unitVar := range []string{
		VarRequest, VarWebSearchCalls, VarWebSearchQueries, VarWebFetchCalls,
		VarXSearchCalls, VarCodeExecutionCalls, VarFileSearchCalls, VarDocumentSearchCalls,
		VarMCPCalls, VarGoogleMapsChunks, VarNumSourcesUsed, VarNumServerSideToolsUsed,
		VarPromptMedia, VarCompletionMedia, VarPromptAudio,
		VarImages, VarAudioSeconds, VarUpstreamCost,
	} {
		sample.Numbers[unitVar] = 1
	}
	for name := range stringVars {
		sample.Strings[name] = ""
	}
	_, errEval := program.EvalUSD(sample)
	return errEval
}

// EvalUSD evaluates the expression and converts the result to USD.
func (p *Program) EvalUSD(env Env) (float64, error) {
	amount, _, errEval := p.EvalUSDWithTrace(env)
	return amount, errEval
}

// EvalUSDWithTrace evaluates the expression and returns the USD amount and any
// tier name recorded by tier(name, expr).
func (p *Program) EvalUSDWithTrace(env Env) (float64, string, error) {
	value, errEval := p.root.eval(&env)
	if errEval != nil {
		return 0, "", errEval
	}
	number, ok := value.(float64)
	if !ok {
		return 0, "", fmt.Errorf("expression must evaluate to a number, got %s", typeName(value))
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, "", fmt.Errorf("expression result is not finite")
	}
	if number < 0 {
		return 0, "", fmt.Errorf("expression result must be non-negative")
	}
	return number / tokensPerMillion, env.MatchedTier, nil
}

// FoldTokens moves token buckets the program does not reference into their parent
// bucket so every reported token is billed at least at the parent coefficient.
func (p *Program) FoldTokens(numbers map[string]float64) {
	move := func(from, to string) {
		if p.Uses(from) {
			return
		}
		numbers[to] += numbers[from]
		numbers[from] = 0
	}
	if p.Uses(VarCacheWrite1h) {
		move(VarCacheWrite12h, VarCacheWrite1h)
	} else if p.Uses(VarCacheWrite30m) {
		move(VarCacheWrite12h, VarCacheWrite30m)
	} else {
		move(VarCacheWrite12h, VarCacheWrite)
	}
	if p.Uses(VarCacheWrite30m) {
		move(VarCacheWrite1h, VarCacheWrite30m)
	} else {
		move(VarCacheWrite1h, VarCacheWrite)
	}
	move(VarCacheWrite30m, VarCacheWrite)
	move(VarCachedText, VarCacheRead)
	move(VarImageCacheRead, VarCacheRead)
	move(VarAudioCacheRead, VarCacheRead)
	move(VarCacheWrite, VarInput)
	move(VarCacheRead, VarInput)
	move(VarImageInput, VarInput)
	move(VarAudioInput, VarInput)
	move(VarVideoInput, VarInput)
	move(VarFileInput, VarInput)
	move(VarImageOutput, VarOutput)
	move(VarAudioOutput, VarOutput)
	move(VarVideoOutput, VarOutput)
	move(VarReasoning, VarOutput)
}

type tokenKind int

const (
	tokenEOF tokenKind = iota
	tokenNumber
	tokenString
	tokenIdent
	tokenOperator
)

type token struct {
	kind tokenKind
	text string
	num  float64
	pos  int
}

func lex(source string) ([]token, error) {
	var tokens []token
	runes := []rune(source)
	for index := 0; index < len(runes); {
		char := runes[index]
		switch {
		case unicode.IsSpace(char):
			index++
		case unicode.IsDigit(char) || (char == '.' && index+1 < len(runes) && unicode.IsDigit(runes[index+1])):
			start := index
			for index < len(runes) && (unicode.IsDigit(runes[index]) || runes[index] == '.' || runes[index] == '_') {
				index++
			}
			if index < len(runes) && (runes[index] == 'e' || runes[index] == 'E') {
				index++
				if index < len(runes) && (runes[index] == '+' || runes[index] == '-') {
					index++
				}
				for index < len(runes) && unicode.IsDigit(runes[index]) {
					index++
				}
			}
			text := strings.ReplaceAll(string(runes[start:index]), "_", "")
			value, errParse := strconv.ParseFloat(text, 64)
			if errParse != nil {
				return nil, fmt.Errorf("invalid number %q at position %d", text, start)
			}
			tokens = append(tokens, token{kind: tokenNumber, text: text, num: value, pos: start})
		case char == '"' || char == '\'':
			start := index
			index++
			var builder strings.Builder
			for index < len(runes) && runes[index] != char {
				builder.WriteRune(runes[index])
				index++
			}
			if index >= len(runes) {
				return nil, fmt.Errorf("unterminated string at position %d", start)
			}
			index++
			tokens = append(tokens, token{kind: tokenString, text: builder.String(), pos: start})
		case unicode.IsLetter(char) || char == '_':
			start := index
			for index < len(runes) && (unicode.IsLetter(runes[index]) || unicode.IsDigit(runes[index]) || runes[index] == '_') {
				index++
			}
			tokens = append(tokens, token{kind: tokenIdent, text: string(runes[start:index]), pos: start})
		default:
			start := index
			two := ""
			if index+1 < len(runes) {
				two = string(runes[index : index+2])
			}
			switch two {
			case "==", "!=", "<=", ">=", "&&", "||":
				tokens = append(tokens, token{kind: tokenOperator, text: two, pos: start})
				index += 2
				continue
			}
			if !strings.ContainsRune("+-*/()<>!?:,", char) {
				return nil, fmt.Errorf("unexpected character %q at position %d", char, start)
			}
			tokens = append(tokens, token{kind: tokenOperator, text: string(char), pos: start})
			index++
		}
	}
	return append(tokens, token{kind: tokenEOF, pos: len(runes)}), nil
}

type parser struct {
	tokens []token
	pos    int
	depth  int
	uses   map[string]bool
}

func (p *parser) peek() token { return p.tokens[p.pos] }

func (p *parser) next() token {
	current := p.tokens[p.pos]
	if current.kind != tokenEOF {
		p.pos++
	}
	return current
}

func (p *parser) acceptOperator(text string) bool {
	if current := p.peek(); current.kind == tokenOperator && current.text == text {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expectOperator(text string) error {
	if p.acceptOperator(text) {
		return nil
	}
	current := p.peek()
	return fmt.Errorf("expected %q at position %d", text, current.pos)
}

var binaryPrecedence = map[string]int{
	"||": 1, "&&": 2, "==": 3, "!=": 3, "<": 4, "<=": 4, ">": 4, ">=": 4,
	"+": 5, "-": 5, "*": 6, "/": 6,
}

func (p *parser) parseExpression(minPrecedence int) (node, error) {
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > maxDepth {
		return nil, fmt.Errorf("expression nesting exceeds %d levels", maxDepth)
	}
	left, errUnary := p.parseUnary()
	if errUnary != nil {
		return nil, errUnary
	}
	for {
		current := p.peek()
		if current.kind != tokenOperator {
			break
		}
		if current.text == "?" && minPrecedence == 0 {
			p.next()
			whenTrue, errTrue := p.parseExpression(0)
			if errTrue != nil {
				return nil, errTrue
			}
			if errColon := p.expectOperator(":"); errColon != nil {
				return nil, errColon
			}
			whenFalse, errFalse := p.parseExpression(0)
			if errFalse != nil {
				return nil, errFalse
			}
			left = &ternaryNode{cond: left, whenTrue: whenTrue, whenFalse: whenFalse}
			continue
		}
		precedence, ok := binaryPrecedence[current.text]
		if !ok || precedence <= minPrecedence {
			break
		}
		p.next()
		right, errRight := p.parseExpression(precedence)
		if errRight != nil {
			return nil, errRight
		}
		left = &binaryNode{op: current.text, left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseUnary() (node, error) {
	if p.acceptOperator("-") || p.acceptOperator("!") {
		op := p.tokens[p.pos-1].text
		p.depth++
		defer func() { p.depth-- }()
		if p.depth > maxDepth {
			return nil, fmt.Errorf("expression nesting exceeds %d levels", maxDepth)
		}
		operand, errOperand := p.parseUnary()
		if errOperand != nil {
			return nil, errOperand
		}
		return &unaryNode{op: op, operand: operand}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (node, error) {
	current := p.next()
	switch current.kind {
	case tokenNumber:
		return &literalNode{value: current.num}, nil
	case tokenString:
		return &literalNode{value: current.text}, nil
	case tokenIdent:
		switch current.text {
		case "true":
			return &literalNode{value: true}, nil
		case "false":
			return &literalNode{value: false}, nil
		}
		if p.acceptOperator("(") {
			return p.parseCall(current)
		}
		canonical := canonicalVarName(current.text)
		if _, ok := numberVars[canonical]; ok {
			p.uses[canonical] = true
			return &variableNode{name: canonical, number: true}, nil
		}
		if _, ok := stringVars[canonical]; ok {
			p.uses[canonical] = true
			return &variableNode{name: canonical}, nil
		}
		return nil, fmt.Errorf("unknown variable %q at position %d", current.text, current.pos)
	case tokenOperator:
		if current.text == "(" {
			inner, errInner := p.parseExpression(0)
			if errInner != nil {
				return nil, errInner
			}
			if errClose := p.expectOperator(")"); errClose != nil {
				return nil, errClose
			}
			return inner, nil
		}
	case tokenEOF:
		return nil, fmt.Errorf("unexpected end of expression")
	}
	return nil, fmt.Errorf("unexpected %q at position %d", current.text, current.pos)
}

func (p *parser) parseCall(name token) (node, error) {
	spec, ok := functions[name.text]
	if !ok {
		return nil, fmt.Errorf("unknown function %q at position %d", name.text, name.pos)
	}
	var args []node
	if !p.acceptOperator(")") {
		for {
			arg, errArg := p.parseExpression(0)
			if errArg != nil {
				return nil, errArg
			}
			args = append(args, arg)
			if p.acceptOperator(")") {
				break
			}
			if errComma := p.expectOperator(","); errComma != nil {
				return nil, errComma
			}
		}
	}
	if len(args) < spec.minArgs || len(args) > spec.maxArgs {
		return nil, fmt.Errorf("%s() takes %d to %d arguments, got %d", name.text, spec.minArgs, spec.maxArgs, len(args))
	}
	switch name.text {
	case "hour", "minute", "weekday", "month", "day":
		if len(args) == 1 {
			literal, isLiteral := args[0].(*literalNode)
			if !isLiteral {
				return nil, fmt.Errorf("%s() timezone must be a string literal", name.text)
			}
			zone, isString := literal.value.(string)
			if !isString {
				return nil, fmt.Errorf("%s() timezone must be a string literal", name.text)
			}
			if _, errZone := loadLocation(zone); errZone != nil {
				return nil, fmt.Errorf("%s(): unknown timezone %q", name.text, zone)
			}
		}
	}
	return &callNode{name: name.text, args: args}, nil
}

type node interface {
	eval(env *Env) (any, error)
}

type literalNode struct{ value any }

func (n *literalNode) eval(*Env) (any, error) { return n.value, nil }

type variableNode struct {
	name   string
	number bool
}

func (n *variableNode) eval(env *Env) (any, error) {
	if n.number {
		return env.Numbers[n.name], nil
	}
	return env.Strings[n.name], nil
}

type unaryNode struct {
	op      string
	operand node
}

func (n *unaryNode) eval(env *Env) (any, error) {
	value, errEval := n.operand.eval(env)
	if errEval != nil {
		return nil, errEval
	}
	if n.op == "!" {
		flag, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("! requires a boolean, got %s", typeName(value))
		}
		return !flag, nil
	}
	number, ok := value.(float64)
	if !ok {
		return nil, fmt.Errorf("- requires a number, got %s", typeName(value))
	}
	return -number, nil
}

type binaryNode struct {
	op          string
	left, right node
}

func (n *binaryNode) eval(env *Env) (any, error) {
	left, errLeft := n.left.eval(env)
	if errLeft != nil {
		return nil, errLeft
	}
	if n.op == "&&" || n.op == "||" {
		leftFlag, ok := left.(bool)
		if !ok {
			return nil, fmt.Errorf("%s requires booleans, got %s", n.op, typeName(left))
		}
		if (n.op == "&&" && !leftFlag) || (n.op == "||" && leftFlag) {
			return leftFlag, nil
		}
		right, errRight := n.right.eval(env)
		if errRight != nil {
			return nil, errRight
		}
		rightFlag, ok := right.(bool)
		if !ok {
			return nil, fmt.Errorf("%s requires booleans, got %s", n.op, typeName(right))
		}
		return rightFlag, nil
	}
	right, errRight := n.right.eval(env)
	if errRight != nil {
		return nil, errRight
	}
	if n.op == "==" || n.op == "!=" {
		if typeName(left) != typeName(right) {
			return nil, fmt.Errorf("%s compares %s with %s", n.op, typeName(left), typeName(right))
		}
		equal := left == right
		if leftString, ok := left.(string); ok {
			equal = strings.EqualFold(leftString, right.(string))
		}
		return equal == (n.op == "=="), nil
	}
	leftNumber, okLeft := left.(float64)
	rightNumber, okRight := right.(float64)
	if !okLeft || !okRight {
		return nil, fmt.Errorf("%s requires numbers, got %s and %s", n.op, typeName(left), typeName(right))
	}
	switch n.op {
	case "+":
		return leftNumber + rightNumber, nil
	case "-":
		return leftNumber - rightNumber, nil
	case "*":
		return leftNumber * rightNumber, nil
	case "/":
		if rightNumber == 0 {
			return nil, fmt.Errorf("division by zero")
		}
		return leftNumber / rightNumber, nil
	case "<":
		return leftNumber < rightNumber, nil
	case "<=":
		return leftNumber <= rightNumber, nil
	case ">":
		return leftNumber > rightNumber, nil
	case ">=":
		return leftNumber >= rightNumber, nil
	}
	return nil, fmt.Errorf("unsupported operator %q", n.op)
}

type ternaryNode struct {
	cond, whenTrue, whenFalse node
}

func (n *ternaryNode) eval(env *Env) (any, error) {
	cond, errCond := n.cond.eval(env)
	if errCond != nil {
		return nil, errCond
	}
	flag, ok := cond.(bool)
	if !ok {
		return nil, fmt.Errorf("?: condition must be a boolean, got %s", typeName(cond))
	}
	if flag {
		return n.whenTrue.eval(env)
	}
	return n.whenFalse.eval(env)
}

type callNode struct {
	name string
	args []node
}

func (n *callNode) eval(env *Env) (any, error) {
	values := make([]any, len(n.args))
	for index, arg := range n.args {
		value, errEval := arg.eval(env)
		if errEval != nil {
			return nil, errEval
		}
		values[index] = value
	}
	switch n.name {
	case "hour", "minute", "weekday", "month", "day":
		now := env.Now
		if now.IsZero() {
			now = time.Now()
		}
		location := time.UTC
		if len(values) == 1 {
			loaded, errZone := loadLocation(values[0].(string))
			if errZone != nil {
				return nil, errZone
			}
			location = loaded
		}
		local := now.In(location)
		switch n.name {
		case "hour":
			return float64(local.Hour()), nil
		case "minute":
			return float64(local.Minute()), nil
		case "month":
			return float64(local.Month()), nil
		case "day":
			return float64(local.Day()), nil
		default:
			return float64(local.Weekday()), nil
		}
	case "min", "max":
		result := 0.0
		for index, value := range values {
			number, ok := value.(float64)
			if !ok {
				return nil, fmt.Errorf("%s() requires numbers, got %s", n.name, typeName(value))
			}
			if index == 0 || (n.name == "min" && number < result) || (n.name == "max" && number > result) {
				result = number
			}
		}
		return result, nil
	case "abs", "ceil", "floor", "round":
		number, ok := values[0].(float64)
		if !ok {
			return nil, fmt.Errorf("%s() requires a number, got %s", n.name, typeName(values[0]))
		}
		switch n.name {
		case "abs":
			return math.Abs(number), nil
		case "ceil":
			return math.Ceil(number), nil
		case "floor":
			return math.Floor(number), nil
		default:
			return math.Round(number), nil
		}
	case "usd":
		number, ok := values[0].(float64)
		if !ok {
			return nil, fmt.Errorf("usd() requires a number, got %s", typeName(values[0]))
		}
		return number * tokensPerMillion, nil
	case "fixed":
		number, ok := values[0].(float64)
		if !ok {
			return nil, fmt.Errorf("fixed() requires a number, got %s", typeName(values[0]))
		}
		reqFactor := 1.0
		if env.Numbers != nil {
			if req, exists := env.Numbers[VarRequest]; exists {
				reqFactor = req
			}
		}
		return number * reqFactor * tokensPerMillion, nil
	case "tier":
		name, okName := values[0].(string)
		if !okName {
			return nil, fmt.Errorf("tier() first argument must be a string, got %s", typeName(values[0]))
		}
		number, okNum := values[1].(float64)
		if !okNum {
			return nil, fmt.Errorf("tier() second argument must be a number, got %s", typeName(values[1]))
		}
		env.MatchedTier = name
		return number, nil
	case "contains", "has":
		haystack, okHaystack := values[0].(string)
		needle, okNeedle := values[1].(string)
		if !okHaystack || !okNeedle {
			return nil, fmt.Errorf("%s() requires strings", n.name)
		}
		return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle)), nil
	case "u":
		path, ok := values[0].(string)
		if !ok {
			return nil, fmt.Errorf("u() requires a string path, got %s", typeName(values[0]))
		}
		if env.LookupNumber != nil {
			return env.LookupNumber(path), nil
		}
		return 0.0, nil
	case "s", "param":
		path, ok := values[0].(string)
		if !ok {
			return nil, fmt.Errorf("%s() requires a string path, got %s", n.name, typeName(values[0]))
		}
		if env.LookupString != nil {
			return env.LookupString(path), nil
		}
		return "", nil
	}
	return nil, fmt.Errorf("unknown function %q", n.name)
}

var locationCache sync.Map

func loadLocation(name string) (*time.Location, error) {
	if cached, ok := locationCache.Load(name); ok {
		return cached.(*time.Location), nil
	}
	location, errLoad := time.LoadLocation(name)
	if errLoad != nil {
		return nil, errLoad
	}
	locationCache.Store(name, location)
	return location, nil
}

func typeName(value any) string {
	switch value.(type) {
	case float64:
		return "number"
	case string:
		return "string"
	case bool:
		return "boolean"
	default:
		return fmt.Sprintf("%T", value)
	}
}
