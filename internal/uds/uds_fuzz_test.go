package uds

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"testing"
	"time"
)

func FuzzReadFrame(f *testing.F) {
	addValidFrame := func(payload string) {
		p := []byte(payload)
		frame := make([]byte, 4+len(p))
		binary.BigEndian.PutUint32(frame[:4], uint32(len(p)))
		copy(frame[4:], p)
		f.Add(frame)
	}

	// Valid frames
	addValidFrame(`{"protocol_version":1,"command":"ping"}`)
	addValidFrame(`{"success":true}`)
	addValidFrame(`{"success":false,"error":{"code":"INTERNAL_ERROR","message":"fail"}}`)

	// Edge cases
	f.Add([]byte{})                                    // empty
	f.Add([]byte{0x00})                                // too short for length
	f.Add([]byte{0x00, 0x00, 0x00, 0x00})              // zero length
	f.Add([]byte{0x00, 0x00, 0x00, 0x01, '{'})         // invalid JSON
	f.Add([]byte{0x00, 0x00, 0x00, 0x05, '{', '}'})    // length mismatch
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF})               // max uint32 length

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxFrameSize+4 {
			t.Skip()
		}

		// Create an in-memory connection pair with deadlines to prevent stalls
		client, server := net.Pipe()
		deadline := time.Now().Add(5 * time.Second)
		client.SetDeadline(deadline)
		server.SetDeadline(deadline)
		defer client.Close()
		defer server.Close()

		done := make(chan struct{})
		go func() {
			defer close(done)
			client.Write(data)
			client.Close()
		}()

		// Attempt to read a frame — should never panic
		var req Request
		err := ReadFrame(server, &req)

		// Wait for writer goroutine to finish
		<-done

		if err != nil {
			return
		}

		// If read succeeded, verify that the request can be re-marshalled
		_, marshalErr := json.Marshal(&req)
		if marshalErr != nil {
			t.Fatalf("successful ReadFrame produced un-marshallable result: %v", marshalErr)
		}
	})
}
