package priority

import (
	"testing"
)

func TestFromString(t *testing.T) {
	for _, tc := range []struct {
		s       string
		want    Value
		wantErr bool
	}{
		{s: "default", want: Default},
		{s: "canary", want: Canary},
		{s: "pin", want: Pin},
		{s: "rollback", want: Rollback},
		{s: "100", want: Value(100)},
		{s: "bad", wantErr: true},
	} {
		t.Run(tc.s, func(t *testing.T) {
			got, err := FromString(tc.s)
			if err != nil && !tc.wantErr {
				t.Errorf("FromString(%s) failed: %v", tc.s, err)
			} else if err == nil && tc.wantErr {
				t.Errorf("FromString(%s) got nil error, wanted non-nil", tc.s)
			} else if got != tc.want {
				t.Errorf("FromString(%s) got: %v, want: %v", tc.s, got, tc.want)
			}
		})
	}
}
