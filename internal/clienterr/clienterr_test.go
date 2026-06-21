package clienterr

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestStatusClassification(t *testing.T) {
	cases := []struct {
		code int
		want error // nil means "no error for this status"
	}{
		{200, nil}, {201, nil}, {204, nil},
		{404, ErrNotFound},
		{408, ErrTransient}, {429, ErrTransient},
		{500, ErrTransient}, {502, ErrTransient}, {503, ErrTransient},
		{400, ErrRejected}, {401, ErrRejected}, {403, ErrRejected}, {409, ErrRejected}, {422, ErrRejected},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%d", tc.code), func(t *testing.T) {
			got := Status("svc", tc.code, []byte("body text"))
			if tc.want == nil {
				if got != nil {
					t.Fatalf("Status(%d) = %v, want nil", tc.code, got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("Status(%d) = %v, want errors.Is %v", tc.code, got, tc.want)
			}
			// A status error is exactly one class.
			for _, other := range []error{ErrTransient, ErrNotFound, ErrRejected} {
				if other != tc.want && errors.Is(got, other) {
					t.Fatalf("Status(%d) also matched %v", tc.code, other)
				}
			}
		})
	}
}

func TestTransportIsTransientAndPreservesOriginal(t *testing.T) {
	if got := Transport("svc", nil); got != nil {
		t.Fatalf("Transport(nil) = %v, want nil", got)
	}
	got := Transport("prowlarr search", context.DeadlineExceeded)
	if !errors.Is(got, ErrTransient) {
		t.Errorf("Transport not classified ErrTransient: %v", got)
	}
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("Transport lost the original error: %v", got)
	}
}

func TestStatusBodyTruncated(t *testing.T) {
	big := make([]byte, 5000)
	for i := range big {
		big[i] = 'x'
	}
	got := Status("svc", 500, big)
	if len(got.Error()) > 700 { // 512 cap + label/code/sentinel overhead
		t.Fatalf("error message not truncated: len=%d", len(got.Error()))
	}
}
