package telegram

import (
	"strings"
	"testing"

	"github.com/gotd/td/tg"
)

func TestSamePhone(t *testing.T) {
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
		if got := samePhone(tt.a, tt.b); got != tt.want {
			t.Errorf("samePhone(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestDescribeDelivery(t *testing.T) {
	tests := []struct {
		name string
		in   tg.AuthSentCodeTypeClass
		want string
	}{
		{"app", &tg.AuthSentCodeTypeApp{Length: 5}, "Telegram app"},
		{"sms", &tg.AuthSentCodeTypeSMS{Length: 5}, "by SMS"},
		{"sms word", &tg.AuthSentCodeTypeSMSWord{}, "by SMS"},
		{"call", &tg.AuthSentCodeTypeCall{Length: 5}, "phone call"},
		{"missed call", &tg.AuthSentCodeTypeMissedCall{Prefix: "+7900", Length: 4}, "+7900"},
		{"email", &tg.AuthSentCodeTypeEmailCode{EmailPattern: "a***@x.com", Length: 6}, "a***@x.com"},
		{"fragment", &tg.AuthSentCodeTypeFragmentSMS{URL: "https://fragment.com/x", Length: 5}, "fragment.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := describeDelivery(tt.in)
			if !strings.Contains(got, tt.want) {
				t.Errorf("describeDelivery(%T) = %q, want it to contain %q", tt.in, got, tt.want)
			}
		})
	}
}
