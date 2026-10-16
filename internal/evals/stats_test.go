package evals

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func TestConformanceStatistics(t *testing.T) {
	raw, err := os.ReadFile("testdata/conformance/stats.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Wilson []struct {
			Pass, N   int
			Low, High float64
		}
		Mulberry32 []struct {
			Seed   uint32
			Values []float64
		}
		PassAtK []struct {
			N, C, K int
			Value   float64
		} `json:"pass_at_k"`
		PassPowK []struct {
			N, C, K int
			Value   float64
		} `json:"pass_pow_k"`
		McNemar []struct {
			B, C int
			P    float64
		}
		Holm []struct{ P, Adjusted []float64 }
		Flak []struct {
			Pass, N int
			Value   float64
		} `json:"flakiness"`
		Bootstrap []struct {
			Deltas             []float64
			Seed               uint32
			Iterations         int
			Mean, Low, High, P float64
		}
		Power []struct {
			SD float64 `json:"sd"`
			N  int     `json:"n"`
		} `json:"power_hint"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	for _, w := range v.Wilson {
		lo, hi := Wilson(w.Pass, w.N)
		near(t, "wilson low", lo, w.Low)
		near(t, "wilson high", hi, w.High)
	}
	for _, m := range v.Mulberry32 {
		r := NewRand(m.Seed)
		for i, want := range m.Values {
			near(t, "mulberry32", r.Float64(), want)
			_ = i
		}
	}
	for _, p := range v.PassAtK {
		near(t, "pass@k", PassAtK(p.N, p.C, p.K), p.Value)
	}
	for _, p := range v.PassPowK {
		near(t, "pass^k", PassPowK(p.N, p.C, p.K), p.Value)
	}
	for _, m := range v.McNemar {
		near(t, "mcnemar", McNemarExact(m.B, m.C), m.P)
	}
	for _, h := range v.Holm {
		for i, got := range Holm(h.P) {
			near(t, "holm", got, h.Adjusted[i])
		}
	}
	for _, f := range v.Flak {
		near(t, "flakiness", Flakiness(f.Pass, f.N), f.Value)
	}
	for _, b := range v.Bootstrap {
		got := Bootstrap(b.Deltas, b.Iterations, b.Seed)
		near(t, "bootstrap mean", got.MeanDelta, b.Mean)
		near(t, "bootstrap low", got.CILow, b.Low)
		near(t, "bootstrap high", got.CIHigh, b.High)
		near(t, "bootstrap p", got.PValue, b.P)
	}
	for _, p := range v.Power {
		if got := PowerHint(p.SD); got != p.N {
			t.Errorf("power hint(%v) = %d, want %d", p.SD, got, p.N)
		}
	}
}

func TestStatisticsEdges(t *testing.T) {
	if Mean(nil) != 0 || StdDev([]float64{1}) != 0 {
		t.Error("empty inputs")
	}
	near(t, "stddev", StdDev([]float64{1, 2, 3, 4}), 1.2909944487358056)
	if lo, hi := Wilson(0, 0); lo != 0 || hi != 0 {
		t.Error("wilson of nothing")
	}
	if PassAtK(0, 0, 1) != 0 || PassAtK(3, 1, 5) != 0 || PassPowK(3, 3, 0) != 0 || Flakiness(0, 0) != 0 {
		t.Error("degenerate pass@k")
	}
	if b := Bootstrap(nil, 10, 1); b.PValue != 1 {
		t.Errorf("bootstrap of nothing = %+v", b)
	}
	if PowerHint(0) != 2 {
		t.Errorf("power hint floor = %d", PowerHint(0))
	}
	r := NewRand(5)
	for i := 0; i < 100; i++ {
		if n := r.Intn(7); n < 0 || n > 6 {
			t.Fatalf("Intn out of range: %d", n)
		}
	}
}
