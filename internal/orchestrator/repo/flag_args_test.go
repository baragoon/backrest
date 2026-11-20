package repo

import (
	"reflect"
	"testing"
)

func TestParseFlagArgsCompatibility(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  []string
	}{
		{`--tag 日本語 "café"`, []string{"--tag", "日本語", "café"}},
		{"foo \\\n# comment\nbar", []string{"foo", "bar"}},
		{"foo\\\n#literal", []string{"foo#literal"}},
		{"foo\\\r\nbar", []string{"foobar"}},
		{"foo \\\r\n# comment\r\nbar", []string{"foo", "bar"}},
		{"\"a\rb\"", []string{"a\rb"}},
		{`  first   "two\ words"`, []string{"first", "two words"}},
		{`""  ''  "two\ words"`, []string{"", "", "two words"}},
		{`"two  spaces"  "three\ words"`, []string{"two  spaces", "three words"}},
		{`\ #literal`, []string{" #literal"}},
		{`foo \ #literal`, []string{"foo", " #literal"}},
		{`\x#literal`, []string{"x#literal"}},
		// Edge cases from review comments
		{`--config=/path#1.conf`, []string{`--config=/path#1.conf`}}, // embedded hash not at word boundary
		{`'single\ text'"double"`, []string{`single\ textdouble`}},   // mixed quotes preserve single-quoted space
		// Windows paths with backslashes (not escapable in POSIX double quotes)
		{`"C:\Users\backup"`, []string{`C:\Users\backup`}},
		{`--path="C:\Program Files\App"`, []string{`--path=C:\Program Files\App`}},
		{`"\\\\server\\share"`, []string{`\\server\share`}},
	} {
		got, err := ParseFlagArgs(tc.input)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseFlagArgs(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
}

func TestParseFlagArgsUnfinishedSyntax(t *testing.T) {
	for _, input := range []string{`"`, `'`, `\`, `foo "bar`, `foo 'bar`, `foo\`, `""\`, `ok '' "`} {
		if got, err := ParseFlagArgs(input); err == nil {
			t.Errorf("ParseFlagArgs(%q) = %q without error", input, got)
		}
	}
}
