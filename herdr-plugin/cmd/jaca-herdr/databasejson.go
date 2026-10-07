package main

import (
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The pure parts of the database pane: the row's JSON as the app's row detail builds it
// (DBRowDetail.json in DatabaseSessionView.swift), the statement a table opens with, and the
// text of a grid cell.

// dbPageSize is DatabaseSession.pageSize.
const dbPageSize = 100

// dbTableQuery is the statement DatabaseSession.selectTable puts in the SQL box. The app writes
// the name between double quotes as it is; a double quote inside the name is doubled here so
// the statement still names that table.
func dbTableQuery(table string) string {
	return `SELECT * FROM "` + strings.ReplaceAll(table, `"`, `""`) + `"`
}

// dbRowJSON is a row as JSON with its columns in order: numbers unquoted when their text
// round-trips, a value that is itself a JSON object or array embedded as nested JSON, NULL as
// null, anything else a quoted string. A row shorter than the columns ends where it ends.
func dbRowJSON(columns []string, row []*string) string {
	n := min(len(columns), len(row))
	lines := make([]string, 0, n+2)
	lines = append(lines, "{")
	for i := 0; i < n; i++ {
		comma := ","
		if i == n-1 {
			comma = ""
		}
		lines = append(lines, "  "+dbQuote(columns[i])+": "+dbJSONValue(row[i], "  ")+comma)
	}
	return strings.Join(append(lines, "}"), "\n")
}

// dbJSONValue is one cell's JSON (DBRowDetail.value).
func dbJSONValue(v *string, indent string) string {
	if v == nil {
		return "null"
	}
	if pretty, ok := dbNestedJSON(*v); ok {
		return strings.ReplaceAll(pretty, "\n", "\n"+indent)
	}
	// No number's text is this long, and a huge value is not worth reading twice to find out.
	if len(*v) <= 400 && (dbIsSwiftInt(*v) || dbIsSwiftDouble(*v)) {
		return *v
	}
	return dbQuote(*v)
}

// dbQuote is DBRowDetail.quote: the backslash, the double quote, the line break and the tab
// are escaped, and nothing else.
func dbQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`).Replace(s) + `"`
}

// dbIsSwiftInt reports whether Int(v) parses and String of it is v again: a 64-bit integer in
// its plain decimal form.
func dbIsSwiftInt(v string) bool {
	n, err := strconv.ParseInt(v, 10, 64)
	return err == nil && strconv.FormatInt(n, 10) == v
}

// dbIsSwiftDouble reports whether Double(v) parses and String of it is v again. Swift prints
// the shortest digits that read back as the same number, in plain form with at least one
// decimal ("1.0", "0.25") or with an exponent ("1e-05", "1e+16"). Which of the two it picks
// near 1e15 and 1e16 is not reproduced here, so a number in that range is reported as not
// round-tripping and gets quoted.
func dbIsSwiftDouble(v string) bool {
	switch v {
	case "nan", "inf", "-inf":
		return true
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return false
	}
	abs := math.Abs(f)
	switch {
	case abs == 0 || (abs >= 1e-4 && abs < 1e15):
		plain := strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.Contains(plain, ".") {
			plain += ".0"
		}
		return plain == v
	case abs < 1e-4 || abs >= 1<<54:
		return strconv.FormatFloat(f, 'e', -1, 64) == v
	}
	return false
}

// dbNestedJSON is a value that is itself a JSON object or array, printed the way the app
// embeds it: JSONSerialization's pretty printing with sorted keys. ok is false for any other
// value, which the caller then writes as a number or a string.
func dbNestedJSON(v string) (string, bool) {
	// The app looks at the first character after spaces and tabs.
	lead := strings.TrimLeft(v, " \t")
	if lead == "" || (lead[0] != '{' && lead[0] != '[') {
		return "", false
	}
	p := jsonReader{s: v}
	p.space()
	node, ok := p.value(0)
	if !ok {
		return "", false
	}
	if p.space(); p.at != len(p.s) {
		return "", false
	}
	var b strings.Builder
	node.write(&b, "")
	return b.String(), true
}

// jsonNode is a parsed JSON value: an object ('o', keys and items), an array ('a', items), a
// string ('s', text) or a number or literal ('n', text as it is printed).
type jsonNode struct {
	kind  byte
	text  string
	keys  []string
	items []jsonNode
}

// jsonMaxDepth is how deep JSONSerialization reads before it gives up.
const jsonMaxDepth = 512

// jsonReader reads JSON the way Foundation's JSONSerialization does, which is what decides
// whether the app embeds a value: one comma may follow the last element, the first of two
// equal keys is kept, and a number keeps the form Foundation prints it in.
type jsonReader struct {
	s  string
	at int
}

func (p *jsonReader) space() {
	for p.at < len(p.s) {
		switch p.s[p.at] {
		case ' ', '\t', '\n', '\r':
			p.at++
		default:
			return
		}
	}
}

func (p *jsonReader) peek() byte {
	if p.at < len(p.s) {
		return p.s[p.at]
	}
	return 0
}

func (p *jsonReader) word(w string) bool {
	if strings.HasPrefix(p.s[p.at:], w) {
		p.at += len(w)
		return true
	}
	return false
}

func (p *jsonReader) value(depth int) (jsonNode, bool) {
	if depth > jsonMaxDepth {
		return jsonNode{}, false
	}
	switch c := p.peek(); {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		text, ok := p.str()
		return jsonNode{kind: 's', text: text}, ok
	case c == '-' || (c >= '0' && c <= '9'):
		text, ok := p.number()
		return jsonNode{kind: 'n', text: text}, ok
	case p.word("true"):
		return jsonNode{kind: 'n', text: "true"}, true
	case p.word("false"):
		return jsonNode{kind: 'n', text: "false"}, true
	case p.word("null"):
		return jsonNode{kind: 'n', text: "null"}, true
	}
	return jsonNode{}, false
}

func (p *jsonReader) array(depth int) (jsonNode, bool) {
	node := jsonNode{kind: 'a'}
	p.at++ // the bracket
	for {
		p.space()
		if p.peek() == ']' { // an empty array, or the one comma allowed after the last element
			p.at++
			return node, true
		}
		item, ok := p.value(depth + 1)
		if !ok {
			return node, false
		}
		node.items = append(node.items, item)
		p.space()
		switch p.peek() {
		case ',':
			p.at++
		case ']':
			p.at++
			return node, true
		default:
			return node, false
		}
	}
}

func (p *jsonReader) object(depth int) (jsonNode, bool) {
	node := jsonNode{kind: 'o'}
	seen := map[string]bool{}
	p.at++ // the brace
	for {
		p.space()
		if p.peek() == '}' {
			p.at++
			return node, true
		}
		if p.peek() != '"' {
			return node, false
		}
		key, ok := p.str()
		if !ok {
			return node, false
		}
		if p.space(); p.peek() != ':' {
			return node, false
		}
		p.at++
		p.space()
		item, ok := p.value(depth + 1)
		if !ok {
			return node, false
		}
		if !seen[key] {
			seen[key] = true
			node.keys, node.items = append(node.keys, key), append(node.items, item)
		}
		p.space()
		switch p.peek() {
		case ',':
			p.at++
		case '}':
			p.at++
			return node, true
		default:
			return node, false
		}
	}
}

// str reads a string. A control character written as it is, an escape JSON doesn't have and
// half of a surrogate pair all fail, as they do in Foundation.
func (p *jsonReader) str() (string, bool) {
	p.at++ // the quote
	var b strings.Builder
	for p.at < len(p.s) {
		c := p.s[p.at]
		switch {
		case c == '"':
			p.at++
			return b.String(), true
		case c < 0x20:
			return "", false
		case c != '\\':
			_, size := utf8.DecodeRuneInString(p.s[p.at:])
			b.WriteString(p.s[p.at : p.at+size])
			p.at += size
			continue
		}
		if p.at+1 >= len(p.s) {
			return "", false
		}
		esc := p.s[p.at+1]
		p.at += 2
		switch esc {
		case '"', '\\', '/':
			b.WriteByte(esc)
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			r, ok := p.hex4()
			if !ok {
				return "", false
			}
			switch {
			case r >= 0xd800 && r < 0xdc00:
				if !p.word(`\u`) {
					return "", false
				}
				low, ok := p.hex4()
				if !ok || low < 0xdc00 || low > 0xdfff {
					return "", false
				}
				r = 0x10000 + (r-0xd800)<<10 + (low - 0xdc00)
			case r >= 0xdc00 && r <= 0xdfff:
				return "", false
			}
			b.WriteRune(r)
		default:
			return "", false
		}
	}
	return "", false
}

func (p *jsonReader) hex4() (rune, bool) {
	if p.at+4 > len(p.s) {
		return 0, false
	}
	n, err := strconv.ParseUint(p.s[p.at:p.at+4], 16, 16)
	if err != nil {
		return 0, false
	}
	p.at += 4
	return rune(n), true
}

// number reads a number and returns it as Foundation prints it back.
func (p *jsonReader) number() (string, bool) {
	start := p.at
	digits := func() string {
		from := p.at
		for p.at < len(p.s) && p.s[p.at] >= '0' && p.s[p.at] <= '9' {
			p.at++
		}
		return p.s[from:p.at]
	}
	neg := p.peek() == '-'
	if neg {
		p.at++
	}
	whole := digits()
	if whole == "" || (len(whole) > 1 && whole[0] == '0') {
		return "", false
	}
	frac, exp, hasFrac, hasExp := "", "", false, false
	if p.peek() == '.' {
		p.at++
		if frac, hasFrac = digits(), true; frac == "" {
			return "", false
		}
	}
	if c := p.peek(); c == 'e' || c == 'E' {
		p.at++
		expStart := p.at
		if c := p.peek(); c == '+' || c == '-' {
			p.at++
		}
		if digits() == "" {
			return "", false
		}
		exp, hasExp = p.s[expStart:p.at], true
	}
	return foundationNumber(p.s[start:p.at], neg, whole, frac, exp, hasFrac || hasExp)
}

// maxDecimalMantissa is the largest mantissa an NSDecimalNumber holds: 2^128 - 1.
var maxDecimalMantissa = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))

// foundationNumber is a JSON number as JSONSerialization writes it after reading it. An
// integer is written as it was read. A number with a fraction or an exponent is read as a
// double and written with 17 significant digits ("0.1" comes back as "0.10000000000000001"),
// unless it has 18 digits or more: then it is read as a decimal number and written in plain
// form, without an exponent. ok is false for a number Foundation fails to read.
func foundationNumber(literal string, neg bool, whole, frac, exp string, fractional bool) (string, bool) {
	sign := ""
	if neg {
		sign = "-"
	}
	count := len(whole) + len(frac)
	if whole == "0" {
		count--
	}
	switch {
	case !fractional && whole == "0":
		return "0", true
	case !fractional && count < 18:
		return sign + whole, true
	case fractional && count < 18:
		if len(exp) > 18 {
			return "", false
		}
		f, err := strconv.ParseFloat(literal, 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return "", false
		}
		return strconv.FormatFloat(f, 'g', 17, 64), true
	}

	// The decimal form: a mantissa of up to 128 bits (further digits are dropped) and a power
	// of ten from -128 to 127.
	power := 0
	if exp != "" {
		if len(exp) > 18 {
			return "", false
		}
		n, err := strconv.Atoi(exp)
		if err != nil || n > 1<<20 || n < -(1<<20) {
			return "", false
		}
		power = n
	}
	power -= len(frac)
	if power < -128 {
		return "", false
	}
	mantissa, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok {
		return "", false
	}
	ten := big.NewInt(10)
	for mantissa.Cmp(maxDecimalMantissa) > 0 {
		mantissa.Quo(mantissa, ten)
		power++
	}
	if power > 127 {
		return "", false
	}
	if mantissa.Sign() == 0 {
		return "0", true
	}
	text := mantissa.String()
	for power < 0 && strings.HasSuffix(text, "0") {
		text = text[:len(text)-1]
		power++
	}
	switch {
	case power >= 0:
		return sign + text + strings.Repeat("0", power), true
	case -power < len(text):
		return sign + text[:len(text)+power] + "." + text[len(text)+power:], true
	}
	return sign + "0." + strings.Repeat("0", -power-len(text)) + text, true
}

// write prints the node as JSONSerialization does with .prettyPrinted and .sortedKeys: two
// spaces a level, " : " after a key, and a blank line inside an empty object or array.
func (n jsonNode) write(b *strings.Builder, indent string) {
	switch n.kind {
	case 's':
		b.WriteString(foundationQuote(n.text))
		return
	case 'n':
		b.WriteString(n.text)
		return
	}
	open, shut := "[", "]"
	if n.kind == 'o' {
		open, shut = "{", "}"
	}
	b.WriteString(open)
	if len(n.items) == 0 {
		b.WriteString("\n\n" + indent + shut)
		return
	}
	order := make([]int, len(n.items))
	for i := range order {
		order[i] = i
	}
	if n.kind == 'o' && len(n.keys) == len(n.items) {
		sort.SliceStable(order, func(i, j int) bool { return foundationKeyLess(n.keys[order[i]], n.keys[order[j]]) })
	}
	inner := indent + "  "
	for at, i := range order {
		if at > 0 {
			b.WriteString(",")
		}
		b.WriteString("\n" + inner)
		if n.kind == 'o' && i < len(n.keys) {
			b.WriteString(foundationQuote(n.keys[i]) + " : ")
		}
		n.items[i].write(b, inner)
	}
	b.WriteString("\n" + indent + shut)
}

// foundationQuote is a string as JSONSerialization writes it: the slash is escaped too, and a
// control character has its short escape or \u00xx.
func foundationQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '/':
			b.WriteString(`\/`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if r < 0x20 {
				const hex = "0123456789abcdef"
				b.WriteString(`\u00` + string(hex[r>>4]) + string(hex[r&0xf]))
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// foundationPunctuation is the order .sortedKeys puts ASCII punctuation in, all of it before
// the digits and the letters.
const foundationPunctuation = " _-,;:!?.'\"()[]{}@*/\\&#%`^+<=>|~$"

// The ranks of a key's parts when keys are compared: punctuation, other symbols, a run of
// digits (compared as a number), then the letters.
const (
	rankSymbol = 1 << 8
	rankDigits = 1 << 24
	rankLetter = rankDigits + 1
	rankOther  = rankLetter + 1<<8
)

// keyPart is one part of a key for sorting: its rank, the number when it is a run of digits,
// whether a letter had an accent and whether it was a capital.
type keyPart struct {
	rank            int
	number          string
	accent, capital bool
}

// latinBase is the unaccented letters a Latin-1 letter sorts as.
var latinBase = map[rune]string{
	'à': "a", 'á': "a", 'â': "a", 'ã': "a", 'ä': "a", 'å': "a", 'æ': "ae", 'ç': "c",
	'è': "e", 'é': "e", 'ê': "e", 'ë': "e", 'ì': "i", 'í': "i", 'î': "i", 'ï': "i",
	'ñ': "n", 'ò': "o", 'ó': "o", 'ô': "o", 'õ': "o", 'ö': "o", 'ø': "o",
	'ù': "u", 'ú': "u", 'û': "u", 'ü': "u", 'ý': "y", 'ÿ': "y", 'ß': "ss",
}

func keyParts(s string) []keyPart {
	var parts []keyPart
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r >= '0' && r <= '9':
			j := i
			for j < len(rs) && rs[j] >= '0' && rs[j] <= '9' {
				j++
			}
			number := strings.TrimLeft(string(rs[i:j]), "0")
			parts = append(parts, keyPart{rank: rankDigits, number: number})
			i = j - 1
		case r < 0x80 && unicode.IsLetter(r):
			parts = append(parts, keyPart{rank: rankLetter + int(unicode.ToLower(r)-'a'), capital: unicode.IsUpper(r)})
		case r < 0x80:
			parts = append(parts, keyPart{rank: strings.IndexRune(foundationPunctuation, r) + 1})
		default:
			lower := unicode.ToLower(r)
			if base, ok := latinBase[lower]; ok {
				for _, c := range base {
					parts = append(parts, keyPart{rank: rankLetter + int(c-'a'), accent: true, capital: lower != r})
				}
			} else if unicode.IsLetter(r) {
				parts = append(parts, keyPart{rank: rankOther + int(lower), capital: lower != r})
			} else {
				parts = append(parts, keyPart{rank: rankSymbol + int(r)})
			}
		}
	}
	return parts
}

// foundationKeyLess is the order .sortedKeys writes an object's keys in: punctuation, digits
// and letters in that order, a run of digits by its value ("a9" before "a10"), and capitals
// and accents only to settle keys that are otherwise the same ("a" before "A" before "ä").
// It matches Foundation for ASCII keys and Latin-1 letters; other scripts are put in code
// point order after them.
func foundationKeyLess(a, b string) bool {
	pa, pb := keyParts(a), keyParts(b)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, y := pa[i], pb[i]
		if x.rank != y.rank {
			return x.rank < y.rank
		}
		if x.number != y.number {
			if len(x.number) != len(y.number) {
				return len(x.number) < len(y.number)
			}
			return x.number < y.number
		}
	}
	if len(pa) != len(pb) {
		return len(pa) < len(pb)
	}
	for i := range pa {
		if pa[i].accent != pb[i].accent {
			return pb[i].accent
		}
	}
	for i := range pa {
		if pa[i].capital != pb[i].capital {
			return pb[i].capital
		}
	}
	if len(a) != len(b) {
		return len(a) < len(b) // fewer leading zeros first
	}
	return a < b
}

// dbIsReadOnly is DatabaseService.isReadOnly: after the leading whitespace and comments, the
// statement starts with select, with, pragma or explain, in any case. jacad opens the snapshot
// read-only whatever this says; it is the check the app makes before it runs a statement.
func dbIsReadOnly(sql string) bool {
	rest := dbStripLeadingComments(sql)
	for _, word := range []string{"select", "with", "pragma", "explain"} {
		if len(rest) < len(word) || !strings.EqualFold(rest[:len(word)], word) {
			continue
		}
		// Swift compares whole characters: a letter with a combining mark after it is another one.
		next, _ := utf8.DecodeRuneInString(rest[len(word):])
		if !unicode.Is(unicode.M, next) && next != 0x200d {
			return true
		}
	}
	return false
}

// dbStripLeadingComments is DatabaseService.stripLeadingComments: it drops the whitespace and
// the SQL comments ("-- line" and "/* block */") a statement opens with. As in Swift, where a
// carriage return and the line feed after it are one character, that pair is neither
// whitespace nor the end of a line comment.
func dbStripLeadingComments(sql string) string {
	crlf := func(s string, i int) bool { return s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n' }
	s := sql
	for {
		before := s
		i := 0
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') && !crlf(s, i) {
			i++
		}
		s = s[i:]
		switch {
		case strings.HasPrefix(s, "--"):
			end := len(s)
			for i := 0; i < len(s); i++ {
				if s[i] == '\n' && (i == 0 || s[i-1] != '\r') {
					end = i + 1
					break
				}
			}
			s = s[end:]
		case strings.HasPrefix(s, "/*"):
			if end := strings.Index(s, "*/"); end >= 0 {
				s = s[end+2:]
			} else {
				s = ""
			}
		}
		if s == before {
			return s
		}
	}
}

// dbHead is the start of a value that lines of cells can show: at most that many characters,
// cut between two of them.
func dbHead(s string, cells int) string {
	limit := max(0, cells) * utf8.UTFMax
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

// dbCellText is a value cut for a grid cell w cells wide: made safe to draw, on one line, and
// ending in "…" when there is more. Only the start of a long value is read, so a huge cell
// costs no more to draw than a short one.
func dbCellText(s string, w int) string {
	if w <= 0 {
		return ""
	}
	cut := false
	if limit := 8*w + 64; len(s) > limit {
		for limit > 0 && !utf8.RuneStart(s[limit]) {
			limit--
		}
		s, cut = s[:limit], true
	}
	text := sanitize(s)
	if cut && cellWidth(text) <= w {
		text += "…"
	}
	return fit(text, w)
}

// dbDropUnsafe removes from a drawn row the characters that could restyle or reorder the pane
// and that the text editor's view lets through: C1 controls and direction marks.
func dbDropUnsafe(row string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 0x80 && r <= 0x9f) || isBidiControl(r) {
			return -1
		}
		return r
	}, row)
}
