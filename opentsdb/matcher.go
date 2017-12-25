package opentsdb

import (
	"regexp"
)

type seriesMatcher []*LabelMatcher

func (s seriesMatcher) Match(tags map[string]TagValue) bool {
	if tags == nil {
		tags = make(map[string]TagValue)
	}
	for _, m := range s {
		if !m.Match(string(tags[m.Name])) {
			return false
		}
	}
	return true
}

// NewLabelMatcher returns a LabelMatcher object ready to use.
func NewLabelMatcher(matchType MatchType, name string, value string) (*LabelMatcher, error) {
	m := &LabelMatcher{
		Type:  matchType,
		Name:  name,
		Value: value,
	}
	if matchType == RegexMatch || matchType == RegexNoMatch {
		re, err := regexp.Compile("^(?:" + string(value) + ")$")
		if err != nil {
			return nil, err
		}
		m.re = re
	}
	return m, nil
}

// LabelMatcher models the matching of a label. Create with NewLabelMatcher.
type LabelMatcher struct {
	Type  MatchType
	Name  string
	Value string
	re    *regexp.Regexp
}

// MatchType is an enum for label matching types.
type MatchType int

// Possible MatchTypes.
const (
	Equal MatchType = iota
	NotEqual
	RegexMatch
	RegexNoMatch
)

func (m MatchType) String() string {
	typeToStr := map[MatchType]string{
		Equal:        "=",
		NotEqual:     "!=",
		RegexMatch:   "=~",
		RegexNoMatch: "!~",
	}
	if str, ok := typeToStr[m]; ok {
		return str
	}
	panic("unknown match type")
}

// Match returns true if the label matcher matches the supplied label value.
func (m *LabelMatcher) Match(v string) bool {
	switch m.Type {
	case Equal:
		return m.Value == v
	case NotEqual:
		return m.Value != v
	case RegexMatch:
		return m.re.MatchString(string(v))
	case RegexNoMatch:
		return !m.re.MatchString(string(v))
	default:
		panic("invalid match type")
	}
}
