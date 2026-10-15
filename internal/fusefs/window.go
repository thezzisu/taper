package fusefs

// PlanFillWindow never crosses a cached boundary or an archive boundary. A gap
// shorter than m is acquired whole; no part of a missing gap shorter than m is
// left behind. Endpoints are exclusive and arithmetic is overflow-safe.
func PlanFillWindow(has func(uint64) bool, bStart, bEnd, x, m uint64) (s, e uint64) {
	if bStart >= bEnd || x < bStart || x >= bEnd || has(x) {
		return x, x
	}
	if m == 0 {
		m = 1
	}
	left, right := x, x+1
	for left > bStart && !has(left-1) {
		left--
	}
	for right < bEnd && !has(right) {
		right++
	}
	width := min(m, right-left)
	s = x
	e = x + min(width, right-x)
	if e-s < width {
		s = e - width
	}
	if s > left && s-left < m {
		s = left
	}
	if right > e && right-e < m {
		e = right
	}
	return
}
