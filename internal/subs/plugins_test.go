package subs

import "testing"

func TestBalanceText(t *testing.T) {
	for in, want := range map[any]string{nil: "", "余额 3 credits": "余额 3 credits", 120.5: "120.5"} {
		if got := balanceText(in); got != want {
			t.Errorf("balanceText(%v) = %q，应为 %q", in, got, want)
		}
	}
}
