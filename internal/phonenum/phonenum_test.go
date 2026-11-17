package phonenum

import "testing"

func TestNormalize(t *testing.T) {
	valid := map[string]string{
		"+79001234567":        "+79001234567",
		"79001234567":         "+79001234567",
		" +7 (900) 123-45-67": "+79001234567",
		"+7.900.123.45.67":    "+79001234567",
	}
	for in, want := range valid {
		got, err := Normalize(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	for _, in := range []string{"", "+", "work", "../x", "a/b", "+7900", "+7900123456789012", "+7900abc4567"} {
		if got, err := Normalize(in); err == nil {
			t.Errorf("Normalize(%q) = %q, want an error", in, got)
		}
	}
}

func TestSame(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"79001234567", "+79001234567", true},
		{"79001234567", "+7 (900) 123-45-67", true},
		{"79001234567", "+79001234568", false},
		{"79001234567", "work", false},
		{"", "", false},
	}
	for _, tt := range tests {
		if got := Same(tt.a, tt.b); got != tt.want {
			t.Errorf("Same(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}
