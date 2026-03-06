package plan

import (
	"fmt"
	"testing"
)

func BenchmarkValidateTaskDAG(b *testing.B) {
	benchmarks := []struct {
		name      string
		names     []string
		blockedBy map[string][]string
	}{
		{
			name:      "linear_5",
			names:     []string{"A", "B", "C", "D", "E"},
			blockedBy: map[string][]string{"B": {"A"}, "C": {"B"}, "D": {"C"}, "E": {"D"}},
		},
		{
			name:  "wide_10",
			names: func() []string { return genNames(10) }(),
			blockedBy: func() map[string][]string {
				// All depend on first node
				m := make(map[string][]string)
				names := genNames(10)
				for _, n := range names[1:] {
					m[n] = []string{names[0]}
				}
				return m
			}(),
		},
		{
			name:  "diamond_20",
			names: func() []string { return genNames(20) }(),
			blockedBy: func() map[string][]string {
				names := genNames(20)
				m := make(map[string][]string)
				// Diamond: first 5 are roots, middle 10 depend on 2 roots each, last 5 depend on all middle
				for i := 5; i < 15; i++ {
					m[names[i]] = []string{names[i%5], names[(i+1)%5]}
				}
				for i := 15; i < 20; i++ {
					deps := make([]string, 0, 10)
					for j := 5; j < 15; j++ {
						deps = append(deps, names[j])
					}
					m[names[i]] = deps
				}
				return m
			}(),
		},
		{
			name:  "linear_100",
			names: func() []string { return genNames(100) }(),
			blockedBy: func() map[string][]string {
				names := genNames(100)
				m := make(map[string][]string)
				for i := 1; i < 100; i++ {
					m[names[i]] = []string{names[i-1]}
				}
				return m
			}(),
		},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, err := ValidateTaskDAG(bm.names, bm.blockedBy)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func genNames(n int) []string {
	names := make([]string, n)
	for i := 0; i < n; i++ {
		names[i] = fmt.Sprintf("task_%03d", i)
	}
	return names
}
