package types

import (
	"reflect"
	"testing"
)

func TestSplitShebang(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  []string
	}{
		{`sh "two\ words"`, []string{"sh", "two words"}},
		{`sh two\ words`, []string{"sh", "two words"}},
		{`sh 'two\ words'`, []string{"sh", `two\ words`}},
		{`sh "two\\ words"`, []string{"sh", `two\ words`}},
		{`sh "C:\\path"`, []string{"sh", `C:\path`}},
	} {
		got, err := splitShebang(tc.input)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitShebang(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
}
