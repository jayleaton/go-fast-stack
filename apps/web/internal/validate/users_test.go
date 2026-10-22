package validate

import (
	"errors"
	"strings"
	"testing"
)

func TestDecodeUpdateUserSettings(t *testing.T) {
	cases := []struct {
		body      string
		wantField string // empty: valid
	}{
		{`{"name":"  Ada  "}`, ""},
		{`{"image":null}`, ""},
		{`{"image":"https://example.com/a.png"}`, ""},
		{`{}`, ""},
		{`{"name":""}`, "name"},
		{`{"name":"` + strings.Repeat("x", 256) + `"}`, "name"},
		{`{"image":"javascript:alert(1)"}`, "image"},
		{`{"email":"a@b.c"}`, "body"},
		{`{"name":1}`, "body"},
		{``, "body"},
		{`{} {}`, "body"},
	}
	for _, c := range cases {
		in, err := Decode[UpdateUserSettings](strings.NewReader(c.body))
		var verr *Error
		switch {
		case c.wantField == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.body, err)
		case c.wantField != "" && (!errors.As(err, &verr) || verr.Fields[c.wantField] == ""):
			t.Errorf("%s: want error on %q, got %v", c.body, c.wantField, err)
		}
		if c.body == `{"name":"  Ada  "}` && *in.Name != "Ada" {
			t.Errorf("name not trimmed: %q", *in.Name)
		}
		if c.body == `{"image":null}` && (!in.Image.Set || in.Image.Value != nil) {
			t.Errorf("explicit null not recorded: %+v", in.Image)
		}
	}
}
