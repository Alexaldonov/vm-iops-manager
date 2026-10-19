package main

import (
	"errors"
	"testing"
)

func TestIsSessionExpired(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("some other error"), false},
		{errors.New("ServerFaultCode: NotAuthenticated"), true},
		{errors.New("The session is not authenticated for the operation attempted."), true},
		{errors.New("connection refused"), false},
	}
	for _, c := range cases {
		if got := isSessionExpired(c.err); got != c.want {
			t.Errorf("isSessionExpired(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
