package config

import "testing"

func TestOperatorAllowlistValidation(t *testing.T) {
	for _, s := range []string{"invalid", "api.example.com", "10.0.0.1", "::1/128", "10.0.0.0/99"} {
		if validate(&Config{Common: &CommonConf{OperatorAllowOut: []string{s}}}) == nil {
			t.Errorf("accepted %q", s)
		}
	}
	c, err := preHandle(&Config{Common: &CommonConf{OperatorAllowOut: []string{" 10.204.0.10/32 ", "", "10.20.0.0/24"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = validate(c); err != nil {
		t.Fatal(err)
	}
	if len(c.Common.OperatorAllowOut) != 2 {
		t.Fatal(c.Common.OperatorAllowOut)
	}
}
