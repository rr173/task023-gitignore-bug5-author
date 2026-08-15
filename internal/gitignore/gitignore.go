// Package gitignore 实现 .gitignore 风格的忽略规则解析与路径匹配。
//
// 求值遵循「最后命中决定」：规则按出现顺序自上而下逐条判断是否命中某路径，
// 取最后一条命中规则决定结果——普通规则命中则路径被忽略，取反规则(! 前缀)命中
// 则路径被保留，无任何规则命中则路径被保留。
//
// 通配符：* 匹配除路径分隔符 / 外的任意字符序列；? 匹配除 / 外的单个字符；
// ** 匹配任意层级（含零层）的目录段。锚定：规则以 / 开头或含分隔符 /（除行尾 /
// 外）时锚定到根；否则按基名匹配任意层级。目录规则：以 / 结尾只匹配目录，
// 命中某段为目录时其下全部路径一并被忽略。
package gitignore

import (
	"fmt"
	"regexp"
	"strings"
)

// Pattern 是一条已解析的忽略规则。
type Pattern struct {
	Source   string // 规则原文（不含换行，含 ! 前缀与行尾 /）
	Line     int    // 1-based 行号
	Negated  bool   // 是否以 ! 取反
	DirOnly  bool   // 是否以 / 结尾（只匹配目录）
	Anchored bool   // 是否锚定到根
	matchAll bool   // glob 为 **，匹配任意路径
	re       *regexp.Regexp
}

// Match 判断 path 是否被该规则命中。
func (p Pattern) Match(path string) bool {
	if p.matchAll {
		return path != ""
	}
	if p.re == nil {
		return false
	}
	return p.re.MatchString(path)
}

// Result 是单条路径的判定结果。
type Result struct {
	Path    string `json:"path"`
	Ignored bool   `json:"ignored"`
	Rule    string `json:"rule"`
	Line    int    `json:"line"`
	Negated bool   `json:"negated"`
}

// Parse 把规则文本解析为有序规则切片。空行与注释行不产生规则。
func Parse(text string) ([]Pattern, error) {
	var patterns []Pattern
	for i, raw := range strings.Split(text, "\n") {
		raw = strings.TrimRight(raw, "\r")
		p, ok, err := parseLine(raw)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行: %w", i+1, err)
		}
		if !ok {
			continue
		}
		p.Line = i + 1
		patterns = append(patterns, p)
	}
	return patterns, nil
}

// parseLine 解析单行。返回 (pattern, ok, err)；ok=false 表示空行或注释。
func parseLine(line string) (Pattern, bool, error) {
	// 1. 修剪行尾未转义空格（\ 保留，由段翻译转为字面空格）。
	line = rtrimUnescapedSpaces(line)
	if line == "" {
		return Pattern{}, false, nil
	}
	// 2. 未转义 # 开头为注释。
	if line[0] == '#' {
		return Pattern{}, false, nil
	}
	p := Pattern{Source: line}
	rest := line
	// 3. 取反前缀（\! 不在此处理，留给段翻译为字面 !）。
	if rest[0] == '!' {
		p.Negated = true
		rest = rest[1:]
	}
	// 4. 锚定：行首 / 。
	if rest != "" && rest[0] == '/' {
		p.Anchored = true
		rest = rest[1:]
	}
	// 5. 目录规则：行尾 / 。
	if rest[len(rest)-1] == '/' {
		p.DirOnly = true
		rest = rest[:len(rest)-1]
	}
	if rest == "" {
		// 例如 "/" 或 "!/" —— 无有效 glob。
		return Pattern{}, false, nil
	}
	// 6. 全匹配 ** 。
	if rest == "**" {
		p.matchAll = true
		return p, true, nil
	}
	// 7. 含内部 /（非行首/行尾）时锚定。
	if !p.Anchored && strings.Contains(rest, "/") {
		p.Anchored = true
	}
	// 8. 构造正则。
	re, err := buildRegex(rest, p.Anchored, p.DirOnly)
	if err != nil {
		return Pattern{}, false, err
	}
	p.re = re
	return p, true, nil
}

// rtrimUnescapedSpaces 修剪行尾未被反斜杠转义的空格。
func rtrimUnescapedSpaces(s string) string {
	end := len(s)
	for end > 0 && s[end-1] == ' ' {
		if end >= 2 && s[end-2] == '\\' {
			break // \ 为转义空格，停止修剪。
		}
		end--
	}
	return s[:end]
}

// buildRegex 把 glob 编译为路径匹配正则。
func buildRegex(glob string, anchored, dirOnly bool) (*regexp.Regexp, error) {
	body := globToRegex(glob)
	var pattern string
	switch {
	case anchored:
		// 锚定：从根开始；目录与非目录在路径串求值下等价（含前缀目录级联）。
		pattern = "^" + body + "($|/)"
	case dirOnly:
		// 基名目录规则：命中某段为目录（后随 /），或路径恰为该目录（根级）。
		pattern = "(^|/)" + body + "/|^" + body + "$"
	default:
		// 基名规则：任意层级的某一段匹配。
		pattern = "(^|/)" + body + "($|/)"
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("编译规则正则失败 %q: %w", glob, err)
	}
	return re, nil
}

// globToRegex 把 glob（已剥离行首/行尾 /，可能含 **、*、?、转义）翻译为正则体。
// 以 / 切分为段，** 段单独展开为跨任意层级的子表达式。
func globToRegex(glob string) string {
	segs := strings.Split(glob, "/")
	var b strings.Builder
	last := len(segs) - 1
	for i, seg := range segs {
		if i > 0 {
			// 当前段之前插入的分隔符。
			switch {
			case seg == "**" && i == last:
				// 尾随 ** 前插必需 /（foo/** 不应命中 foo 自身）。
				b.WriteString("/")
			case seg == "**":
				// 中间 ** 自带前导 /，不额外插入。
			case segs[i-1] == "**" && i-1 == 0:
				// 前导 ** 已含可选尾随 /，不插入。
			default:
				b.WriteString("/")
			}
		}
		if seg == "**" {
			switch {
			case i == 0 && i == last:
				b.WriteString(".*")
			case i == 0:
				b.WriteString("(?:[^/]*/)?") // **/foo
			case i == last:
				b.WriteString(".*") // foo/**（前导 / 已由 gap 插入）
			default:
				b.WriteString("(?:/.*)?") // a/**/b
			}
			continue
		}
		b.WriteString(segToRegex(seg))
	}
	return b.String()
}

// segToRegex 翻译单段（不含 /）为正则：* → [^/]*，? → [^/]，\X → 字面 X。
func segToRegex(seg string) string {
	var b strings.Builder
	runes := []rune(seg)
	for i := 0; i < len(runes); {
		r := runes[i]
		switch {
		case r == '\\' && i+1 < len(runes):
			b.WriteString(regexp.QuoteMeta(string(runes[i+1])))
			i += 2
		case r == '\\':
			b.WriteString(regexp.QuoteMeta(`\`))
			i++
		case r == '*':
			b.WriteString("[^/]*")
			i++
		case r == '?':
			b.WriteString("[^/]")
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
			i++
		}
	}
	return b.String()
}

// Decide 返回 path 在给定规则下的判定与决定规则（无命中时 matched 为 nil）。
// 忽略与否由最后一条命中规则决定：普通规则→忽略，取反规则→保留。
func Decide(patterns []Pattern, path string) (ignored bool, matched *Pattern) {
	for i := range patterns {
		if patterns[i].Match(path) {
			matched = &patterns[i]
		}
	}
	if matched == nil {
		return false, nil
	}
	return !matched.Negated, matched
}

// Check 对多个路径批量判定，保持输入顺序。
func Check(patterns []Pattern, paths []string) []Result {
	if len(patterns) == 0 {
		return []Result{}
	}
	results := make([]Result, 0, len(paths))
	for _, p := range paths {
		ignored, matched := Decide(patterns, p)
		r := Result{Path: p, Ignored: ignored}
		if matched != nil {
			r.Rule = matched.Source
			r.Line = matched.Line
			r.Negated = matched.Negated
		}
		results = append(results, r)
	}
	return results
}
