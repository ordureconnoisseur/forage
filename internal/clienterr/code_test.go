package clienterr

import (
	"errors"
	"fmt"
	"testing"
)

// TestStatusCodeReadable pins the Code accessor and, more importantly, that
// carrying the code did not change Status's classification or message shape
// — errors.Is behaviour is load-bearing across the poller.
func TestStatusCodeReadable(t *testing.T) {
	cases := []struct {
		code     int
		sentinel error
	}{
		{404, ErrNotFound},
		{429, ErrTransient},
		{500, ErrTransient},
		{403, ErrRejected},
	}
	for _, c := range cases {
		err := Status("stashdb graphql", c.code, []byte("nope"))
		if !errors.Is(err, c.sentinel) {
			t.Errorf("Status(%d) not errors.Is %v: %v", c.code, c.sentinel, err)
		}
		if Code(err) != c.code {
			t.Errorf("Code(Status(%d)) = %d", c.code, Code(err))
		}
		// The code survives another layer of wrapping.
		if wrapped := fmt.Errorf("watch loop: %w", err); Code(wrapped) != c.code {
			t.Errorf("Code(wrapped %d) = %d", c.code, Code(wrapped))
		}
	}
	if Code(nil) != 0 || Code(errors.New("plain")) != 0 || Code(Transport("x", errors.New("y"))) != 0 {
		t.Error("Code must be 0 for nil / plain / transport errors")
	}
	if Status("x", 204, nil) != nil {
		t.Error("2xx must stay nil")
	}
}
