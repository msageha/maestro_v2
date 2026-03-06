package yaml

import (
	"path/filepath"
	"testing"
)

func BenchmarkAtomicWrite(b *testing.B) {
	benchmarks := []struct {
		name string
		data any
	}{
		{
			name: "small_map",
			data: map[string]string{"status": "completed", "worker": "worker1"},
		},
		{
			name: "medium_struct",
			data: struct {
				Name    string   `yaml:"name"`
				Version int      `yaml:"version"`
				Tasks   []string `yaml:"tasks"`
			}{
				Name:    "test-command",
				Version: 2,
				Tasks:   []string{"task1", "task2", "task3", "task4", "task5"},
			},
		},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			dir := b.TempDir()
			path := filepath.Join(dir, "bench.yaml")

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := AtomicWrite(path, bm.data); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
