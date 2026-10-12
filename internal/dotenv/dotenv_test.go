package dotenv

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	src := `# comment
GREETING=hello from .env
export TOKEN="abc\"def"
SINGLE='raw $x'
EMPTY=
INLINE=value # note
not valid
1BAD=x
`
	got, bad := Parse(src)
	want := []Entry{
		{"GREETING", "hello from .env", 2},
		{"TOKEN", `abc"def`, 3},
		{"SINGLE", "raw $x", 4},
		{"EMPTY", "", 5},
		{"INLINE", "value", 6},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
	if !reflect.DeepEqual(bad, []int{7, 8}) {
		t.Errorf("bad lines %v", bad)
	}
}

func TestReferenced(t *testing.T) {
	src := `image: nginx:${TAG:-1}
environment:
  A: $FOO
  B: "$${NOT_A_VAR}"
  C: ${TAG}
  D: ${BAR?required}`
	if got := Referenced(src); !reflect.DeepEqual(got, []string{"TAG", "FOO", "BAR"}) {
		t.Errorf("%v", got)
	}
}
