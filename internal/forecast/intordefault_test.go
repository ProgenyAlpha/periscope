package forecast

import "testing"

func TestIntOrDefaultRounds(t *testing.T) {
	cases := []struct {
		in   any
		want int
	}{
		{9.6, 10}, {9.4, 9}, {0.5, 1}, {-0.6, -1}, {12.0, 12}, {7, 7}, {"x", -1}, {nil, -1},
	}
	for _, c := range cases {
		if got := IntOrDefault(c.in, -1); got != c.want {
			t.Errorf("IntOrDefault(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}
