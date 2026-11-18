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
	} {
		got, err := parseFlagArgs(tc.input)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseFlagArgs(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
}
