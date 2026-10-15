package fusefs

import "testing"

func TestPlanFillWindowMinimumAndShortTails(t *testing.T) {
	has := func(p uint64) bool { return p < 40 || p >= 70 }
	if a, b := PlanFillWindow(has, 0, 100, 45, 16); a != 40 || b != 70 {
		t.Fatalf("window=%d,%d", a, b)
	}
	if a, b := PlanFillWindow(has, 40, 70, 65, 16); a != 40 || b != 70 {
		t.Fatalf("tail=%d,%d", a, b)
	}
	has2 := func(p uint64) bool { return p < 12 || p >= 20 }
	if a, b := PlanFillWindow(has2, 0, 32, 15, 16); a != 12 || b != 20 {
		t.Fatalf("short gap=%d,%d", a, b)
	}
	if a, b := PlanFillWindow(has2, 0, 32, 2, 16); a != 2 || b != 2 {
		t.Fatalf("cache hit=%d,%d", a, b)
	}
}

func TestFillWindowExhaustive(t *testing.T) {
	for n := uint64(1); n <= 10; n++ {
		for bitmap := uint64(0); bitmap < 1<<n; bitmap++ {
			has := func(p uint64) bool { return bitmap&(1<<p) != 0 }
			for x := uint64(0); x < n; x++ {
				for _, m := range []uint64{1, 2, 4, 8, 32, ^uint64(0)} {
					s, e := PlanFillWindow(has, 0, n, x, m)
					if has(x) {
						if s != x || e != x {
							t.Fatal("hit expanded")
						}
						continue
					}
					left, right := x, x+1
					for left > 0 && !has(left-1) {
						left--
					}
					for right < n && !has(right) {
						right++
					}
					if s < left || e > right || s > x || e <= x || e-s < min(m, right-left) || s-left > 0 && s-left < m || right-e > 0 && right-e < m {
						t.Fatal(n, bitmap, x, m, s, e, left, right)
					}
				}
			}
		}
	}
}
