package main

import "testing"

func TestConfirmYes(t *testing.T) {
	cases := map[string]bool{
		"y":           true,
		"Y":           true,
		"yes":         true,
		"YES":         true,
		" yes \n":     true,
		"n":           false,
		"no":          false,
		"":            false,
		"\n":          false,
		"yeah":        false,
		"1":           false,
		"yolo":        false,
		" yes indeed": false,
	}
	for input, want := range cases {
		if got := confirmYes(input); got != want {
			t.Errorf("confirmYes(%q) = %v, want %v", input, got, want)
		}
	}
}
