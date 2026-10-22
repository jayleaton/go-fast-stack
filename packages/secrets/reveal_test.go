package secrets_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jayleaton/ai-patterns-go/packages/secrets"
	"github.com/jayleaton/ai-patterns-go/packages/secrets/reveal"
)

func TestRevealValue(t *testing.T) {
	if got := reveal.Value(secrets.NewSecret("re_round_trip")); got != "re_round_trip" {
		t.Fatalf("round trip: %q", got)
	}
	if got := reveal.Value(secrets.Secret{}); got != "" {
		t.Fatalf("zero secret: %q", got)
	}
	if got := reveal.Value(secrets.NewSecret("")); got != "" {
		t.Fatalf("empty secret: %q", got)
	}
}

// TestNoMethodReturnsTheValue keeps reveal.Value and reveal.Keyring the only
// ways to read a secret: a method returning the value (of a Secret or a
// Keyring) could be called through an interface, outside the reach of
// depguard.
func TestNoMethodReturnsTheValue(t *testing.T) {
	const value = "re_method_probe"
	sec := secrets.NewSecret(value)
	for _, s := range []reflect.Value{
		reflect.ValueOf(sec),
		reflect.ValueOf(secrets.Keyring{Current: sec, Previous: []secrets.Secret{sec}, Revoked: []secrets.Secret{sec}}),
	} {
		checkMethods(t, s, value)
	}
	if cur, prev, rev := reveal.Keyring(secrets.Keyring{Current: sec, Previous: []secrets.Secret{sec}}); cur != value || len(prev) != 1 || prev[0] != value || rev != nil {
		t.Fatalf("reveal.Keyring = %q %q %q", cur, prev, rev)
	}
}

func checkMethods(t *testing.T, s reflect.Value, value string) {
	t.Helper()
	for _, v := range []reflect.Value{s, reflect.New(s.Type())} {
		if v.Kind() == reflect.Pointer {
			v.Elem().Set(s)
		}
		for i := range v.NumMethod() {
			m, name := v.Method(i), v.Type().Method(i).Name
			if m.Type().NumIn() != 0 {
				continue
			}
			for _, out := range m.Call(nil) {
				var got string
				switch out.Kind() {
				case reflect.String:
					got = out.String()
				case reflect.Slice:
					switch out.Type().Elem().Kind() {
					case reflect.Uint8:
						got = string(out.Bytes())
					case reflect.String:
						got = fmt.Sprint(out.Interface())
					}
				}
				if strings.Contains(got, value) {
					t.Errorf("%s.%s returns the value", v.Type(), name)
				}
			}
		}
	}
}
