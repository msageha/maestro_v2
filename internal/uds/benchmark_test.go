package uds

import (
	"net"
	"testing"
)

func BenchmarkWriteReadFrame(b *testing.B) {
	benchmarks := []struct {
		name    string
		payload *Request
	}{
		{
			name: "small",
			payload: &Request{
				ProtocolVersion: 1,
				Command:         "result.write",
			},
		},
		{
			name: "medium",
			payload: func() *Request {
				r, _ := NewRequest("task.dispatch", map[string]string{
					"agent_id":   "worker1",
					"task_id":    "task_123456",
					"command_id": "cmd_789012",
					"content":    "Implement feature X with detailed instructions that span multiple lines",
				})
				return r
			}(),
		},
	}

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()

			errCh := make(chan error, 1)
			go func() {
				for {
					var req Request
					if err := ReadFrame(server, &req); err != nil {
						errCh <- err
						return
					}
				}
			}()

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := WriteFrame(client, bm.payload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkReadFrame(b *testing.B) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	req := &Request{
		ProtocolVersion: 1,
		Command:         "status",
	}

	go func() {
		for {
			if err := WriteFrame(client, req); err != nil {
				return
			}
		}
	}()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var r Request
		if err := ReadFrame(server, &r); err != nil {
			b.Fatal(err)
		}
	}
}
