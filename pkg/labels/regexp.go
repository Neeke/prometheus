// Copyright 2020 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package labels

import (
	"regexp"
	"regexp/syntax"
	"strings"
)

const maxSetMatches = 256

type FastRegexMatcher struct {
	re *regexp.Regexp

	setMatches []string
	prefix     string
	suffix     string
	contains   string
}

func NewFastRegexMatcher(v string) (*FastRegexMatcher, error) {
	parsed, err := syntax.Parse(v, syntax.Perl)
	if err != nil {
		return nil, err
	}
	// Simplify the syntax tree to run faster.
	parsed = parsed.Simplify()
	re, err := regexp.Compile("^(?:" + parsed.String() + ")$")
	if err != nil {
		return nil, err
	}
	m := &FastRegexMatcher{
		re:         re,
		setMatches: findSetMatches(parsed, ""),
	}

	if parsed.Op == syntax.OpConcat {
		m.prefix, m.suffix, m.contains = optimizeConcatRegex(parsed)
	}

	return m, nil
}

// findSetMatches extract equality matches from a regexp.
// Returns nil if we can't replace the regexp by only equality matchers.
func findSetMatches(re *syntax.Regexp, base string) []string {
	// Matches are case sensitive, if we find a case insensitive regexp.
	// We have to abort.
	if isCaseInsensitive(re) {
		return nil
	}
	switch re.Op {
	case syntax.OpLiteral:
		return []string{base + string(re.Rune)}
	case syntax.OpEmptyMatch:
		if base != "" {
			return []string{base}
		}
	case syntax.OpAlternate:
		return findSetMatchesFromAlternate(re, base)
	case syntax.OpCapture:
		clearCapture(re)
		return findSetMatches(re, base)
	case syntax.OpConcat:
		return findSetMatchesFromConcat(re, base)
	case syntax.OpCharClass:
		if len(re.Rune)%2 != 0 {
			return nil
		}
		var matches []string
		var totalSet int
		for i := 0; i+1 < len(re.Rune); i = i + 2 {
			totalSet += int(re.Rune[i+1]-re.Rune[i]) + 1
		}
		// limits the total characters that can be used to create matches.
		// In some case like negation [^0-9] a lot of possibilities exists and that
		// can create thousands of possible matches at which points we're better off using regexp.
		if totalSet > maxSetMatches {
			return nil
		}
		for i := 0; i+1 < len(re.Rune); i = i + 2 {
			lo, hi := re.Rune[i], re.Rune[i+1]
			for c := lo; c <= hi; c++ {
				matches = append(matches, base+string(c))
			}

		}
		return matches
	default:
		return nil
	}
	return nil
}

func findSetMatchesFromConcat(re *syntax.Regexp, base string) []string {
	if len(re.Sub) == 0 {
		return nil
	}
	clearBeginEndText(re)
	clearCapture(re.Sub...)
	matches := []string{base}

	for i := 0; i < len(re.Sub); i++ {
		var newMatches []string
		for _, b := range matches {
			m := findSetMatches(re.Sub[i], b)
			if m == nil {
				return nil
			}
			if tooManyMatches(newMatches, m...) {
				return nil
			}
			newMatches = append(newMatches, m...)
		}
		matches = newMatches
	}

	return matches
}

func findSetMatchesFromAlternate(re *syntax.Regexp, base string) []string {
	var setMatches []string
	for _, sub := range re.Sub {
		found := findSetMatches(sub, base)
		if found == nil {
			return nil
		}
		if tooManyMatches(setMatches, found...) {
			return nil
		}
		setMatches = append(setMatches, found...)
	}
	return setMatches
}

// clearCapture removes capture operation as they are not used for matching.
func clearCapture(regs ...*syntax.Regexp) {
	for _, r := range regs {
		if r.Op == syntax.OpCapture {
			*r = *r.Sub[0]
		}
	}
}

// clearBeginEndText removes the begin and end text from the regexp. Prometheus regexp are anchored to the beginning and end of the string.
func clearBeginEndText(re *syntax.Regexp) {
	if len(re.Sub) == 0 {
		return
	}
	if len(re.Sub) == 1 {
		if re.Sub[0].Op == syntax.OpBeginText || re.Sub[0].Op == syntax.OpEndText {
			re.Sub = nil
			return
		}
	}
	if re.Sub[0].Op == syntax.OpBeginText {
		re.Sub = re.Sub[1:]
	}
	if re.Sub[len(re.Sub)-1].Op == syntax.OpEndText {
		re.Sub = re.Sub[:len(re.Sub)-1]
	}
}

// isCaseInsensitive tells if a regexp is case insensitive.
// The flag should be check at each level of the syntax tree.
func isCaseInsensitive(reg *syntax.Regexp) bool {
	return (reg.Flags & syntax.FoldCase) != 0
}

// tooManyMatches guards against creating too many set matches
func tooManyMatches(matches []string, new ...string) bool {
	return len(matches)+len(new) > maxSetMatches
}

func (m *FastRegexMatcher) MatchString(s string) bool {
	if len(m.setMatches) != 0 {
		for _, match := range m.setMatches {
			if match == s {
				return true
			}
		}
		return false
	}
	if m.prefix != "" && !strings.HasPrefix(s, m.prefix) {
		return false
	}
	if m.suffix != "" && !strings.HasSuffix(s, m.suffix) {
		return false
	}
	if m.contains != "" && !strings.Contains(s, m.contains) {
		return false
	}
	return m.re.MatchString(s)
}

func (m *FastRegexMatcher) SetMatches() []string {
	return m.setMatches
}

// optimizeConcatRegex returns literal prefix/suffix text that can be safely
// checked against the label value before running the regexp matcher.
func optimizeConcatRegex(r *syntax.Regexp) (prefix, suffix, contains string) {
	sub := r.Sub

	// We can safely remove begin and end text matchers respectively
	// at the beginning and end of the regexp.
	if len(sub) > 0 && sub[0].Op == syntax.OpBeginText {
		sub = sub[1:]
	}
	if len(sub) > 0 && sub[len(sub)-1].Op == syntax.OpEndText {
		sub = sub[:len(sub)-1]
	}

	if len(sub) == 0 {
		return
	}

	// Given Prometheus regex matchers are always anchored to the begin/end
	// of the text, if the first/last operations are literals, we can safely
	// treat them as prefix/suffix.
	if sub[0].Op == syntax.OpLiteral && (sub[0].Flags&syntax.FoldCase) == 0 {
		prefix = string(sub[0].Rune)
	}
	if last := len(sub) - 1; sub[last].Op == syntax.OpLiteral && (sub[last].Flags&syntax.FoldCase) == 0 {
		suffix = string(sub[last].Rune)
	}

	// If contains any literal which is not a prefix/suffix, we keep the
	// 1st one. We do not keep the whole list of literals to simplify the
	// fast path.
	for i := 1; i < len(sub)-1; i++ {
		if sub[i].Op == syntax.OpLiteral && (sub[i].Flags&syntax.FoldCase) == 0 {
			contains = string(sub[i].Rune)
			break
		}
	}

	return
}

type StringMatcher interface {
	Matches(s string) bool
}

func stringMatcherFromRegexp(re *syntax.Regexp) StringMatcher {
	clearCapture(re)
	clearBeginEndText(re)
	switch re.Op {
	case syntax.OpStar:
		return anyStringMatcher{
			allowEmpty: true,
			matchNL:    re.Flags&syntax.DotNL != 0,
		}
	case syntax.OpEmptyMatch:
		return emptyStringMatcher{}
	case syntax.OpPlus:
		return anyStringMatcher{
			allowEmpty: false,
			matchNL:    re.Flags&syntax.DotNL != 0,
		}
	case syntax.OpLiteral:
		return equalStringMatcher{
			s:             string(re.Rune),
			caseSensitive: !isCaseInsensitive(re),
		}
	case syntax.OpAlternate:
		or := make([]StringMatcher, 0, len(re.Sub))
		for _, sub := range re.Sub {
			m := stringMatcherFromRegexp(sub)
			if m == nil {
				return nil
			}
			or = append(or, m)
		}
		return orStringMatcher(or)
	case syntax.OpConcat:
		clearCapture(re.Sub...)
		if len(re.Sub) == 0 {
			return emptyStringMatcher{}
		}
		if len(re.Sub) == 1 {
			return stringMatcherFromRegexp(re.Sub[0])
		}
		var left, right StringMatcher

		if re.Sub[0].Op == syntax.OpPlus || re.Sub[0].Op == syntax.OpStar {
			left = stringMatcherFromRegexp(re.Sub[0])
			if left == nil {
				return nil
			}
			re.Sub = re.Sub[1:]
		}
		if re.Sub[len(re.Sub)-1].Op == syntax.OpPlus || re.Sub[len(re.Sub)-1].Op == syntax.OpStar {
			right = stringMatcherFromRegexp(re.Sub[len(re.Sub)-1])
			if right == nil {
				return nil
			}
			re.Sub = re.Sub[:len(re.Sub)-1]
		}
		matches := findSetMatches(re, "")
		if left == nil && right == nil {
			if len(matches) > 0 {
				var or []StringMatcher
				for _, match := range matches {
					or = append(or, equalStringMatcher{
						s:             match,
						caseSensitive: true,
					})
				}
				return orStringMatcher(or)
			}
		}
		if len(matches) > 0 {
			return containsStringMatcher{
				substr: matches,
				left:   left,
				right:  right,
			}
		}
	}
	return nil
}

type containsStringMatcher struct {
	substr []string
	left   StringMatcher
	right  StringMatcher
}

func (m containsStringMatcher) Matches(s string) bool {
	var pos int
	for _, substr := range m.substr {
		pos = strings.Index(s, substr)
		if pos < 0 {
			continue
		}
		if m.right != nil && m.left != nil {
			if m.left.Matches(s[:pos]) && m.right.Matches(s[pos+len(m.substr):]) {
				return true
			}
			continue
		}
		if m.left != nil {
			if m.left.Matches(s[:pos]) {
				return true
			}
			continue
		}
		if m.right != nil {
			if m.right.Matches(s[pos+len(m.substr):]) {
				return true
			}
			continue
		}
	}
	return false
}

type emptyStringMatcher struct{}

func (m emptyStringMatcher) Matches(s string) bool {
	return len(s) == 0
}

type orStringMatcher []StringMatcher

func (m orStringMatcher) Matches(s string) bool {
	for _, matcher := range m {
		if matcher.Matches(s) {
			return true
		}
	}
	return false
}

type equalStringMatcher struct {
	s             string
	caseSensitive bool
}

func (m equalStringMatcher) Matches(s string) bool {
	if !m.caseSensitive {
		return m.s == s
	}
	return strings.EqualFold(m.s, s)
}

type anyStringMatcher struct {
	allowEmpty bool
	matchNL    bool
}

func (m anyStringMatcher) Matches(s string) bool {
	if !m.matchNL && strings.ContainsRune(s, '\n') {
		return false
	}
	if !m.allowEmpty && len(s) == 0 {
		return false
	}
	return true
}
