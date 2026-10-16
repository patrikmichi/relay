package evals

import (
	"math"
	"sort"
)

// Rand is a seeded generator whose sequence is the same on every platform and
// in the gateway's implementation (mulberry32), so a seed reproduces a
// bootstrap anywhere.
type Rand struct{ state uint32 }

// NewRand returns a generator for seed.
func NewRand(seed uint32) *Rand { return &Rand{state: seed} }

// Float64 returns the next value in [0, 1).
func (r *Rand) Float64() float64 {
	r.state += 0x6D2B79F5
	t := r.state
	t = (t ^ (t >> 15)) * (t | 1)
	t = (t + (t^(t>>7))*(t|61)) ^ t
	return float64(t^(t>>14)) / 4294967296
}

// Intn returns a value in [0, n).
func (r *Rand) Intn(n int) int { return int(r.Float64() * float64(n)) }

// Mean is the arithmetic mean, 0 for no values.
func Mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// StdDev is the sample standard deviation, 0 below two values.
func StdDev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := Mean(xs)
	var s float64
	for _, x := range xs {
		s += (x - m) * (x - m)
	}
	return math.Sqrt(s / float64(len(xs)-1))
}

const z95 = 1.959963984540054

// Wilson is the 95 % Wilson score interval of pass out of n.
func Wilson(pass, n int) (lo, hi float64) {
	if n <= 0 {
		return 0, 0
	}
	p := float64(pass) / float64(n)
	nf := float64(n)
	den := 1 + z95*z95/nf
	centre := (p + z95*z95/(2*nf)) / den
	half := z95 * math.Sqrt(p*(1-p)/nf+z95*z95/(4*nf*nf)) / den
	return math.Max(0, centre-half), math.Min(1, centre+half)
}

func logChoose(n, k int) float64 {
	a, _ := math.Lgamma(float64(n + 1))
	b, _ := math.Lgamma(float64(k + 1))
	c, _ := math.Lgamma(float64(n - k + 1))
	return a - b - c
}

// PassAtK is the unbiased chance that at least one of k attempts passes, given
// c passes among n attempts.
func PassAtK(n, c, k int) float64 {
	if n <= 0 || k <= 0 || k > n {
		return 0
	}
	if n-c < k {
		return 1
	}
	return 1 - math.Exp(logChoose(n-c, k)-logChoose(n, k))
}

// PassPowK is the unbiased chance that k attempts all pass, given c passes
// among n attempts.
func PassPowK(n, c, k int) float64 {
	if n <= 0 || k <= 0 || k > n || c < k {
		return 0
	}
	return math.Exp(logChoose(c, k) - logChoose(n, k))
}

// Flakiness is the share of repeats that disagree with the majority outcome.
func Flakiness(pass, n int) float64 {
	if n <= 0 {
		return 0
	}
	minority := pass
	if n-pass < minority {
		minority = n - pass
	}
	return float64(minority) / float64(n)
}

// McNemarExact is the two-sided exact p-value for b cases that only A passes
// and c cases that only B passes.
func McNemarExact(b, c int) float64 {
	n := b + c
	if n == 0 {
		return 1
	}
	lo := b
	if c < lo {
		lo = c
	}
	var tail float64
	for i := 0; i <= lo; i++ {
		tail += math.Exp(logChoose(n, i) - float64(n)*math.Ln2)
	}
	return math.Min(1, 2*tail)
}

// Holm applies the Holm-Bonferroni correction to p-values, returning adjusted
// values in the input order.
func Holm(ps []float64) []float64 {
	m := len(ps)
	idx := make([]int, m)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return ps[idx[a]] < ps[idx[b]] })
	out := make([]float64, m)
	running := 0.0
	for rank, i := range idx {
		running = math.Max(running, math.Min(1, float64(m-rank)*ps[i]))
		out[i] = running
	}
	return out
}

// BootstrapResult is a paired comparison of score deltas.
type BootstrapResult struct {
	MeanDelta float64
	CILow     float64
	CIHigh    float64
	PValue    float64
}

// Bootstrap resamples paired score deltas: a 95 % percentile interval for the
// mean and a two-sided p-value from the share of resamples on the far side of
// zero.
func Bootstrap(deltas []float64, iterations int, seed uint32) BootstrapResult {
	res := BootstrapResult{MeanDelta: Mean(deltas)}
	n := len(deltas)
	if n == 0 {
		res.PValue = 1
		return res
	}
	r := NewRand(seed)
	means := make([]float64, iterations)
	for i := range means {
		var s float64
		for j := 0; j < n; j++ {
			s += deltas[r.Intn(n)]
		}
		means[i] = s / float64(n)
	}
	sort.Float64s(means)
	res.CILow = means[int(0.025*float64(iterations))]
	res.CIHigh = means[int(0.975*float64(iterations))-1]
	var below, above int
	for _, m := range means {
		if m <= 0 {
			below++
		}
		if m >= 0 {
			above++
		}
	}
	res.PValue = math.Min(1, 2*math.Min(float64(below), float64(above))/float64(iterations))
	return res
}

// PowerHint is how many paired cases detect a 5-point change at α 0.05 and
// power 0.8, given the spread of the observed deltas.
func PowerHint(sd float64) int {
	const zBeta, effect = 0.8416212335729143, 0.05
	n := (z95 + zBeta) * sd / effect
	n *= n
	if n < 2 {
		return 2
	}
	return int(math.Ceil(n))
}
