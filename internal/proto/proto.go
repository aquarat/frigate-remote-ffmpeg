// Package proto implements the wire protocol for frigate-ffmpeg-proxy.
//
// Frame format:
//
//	┌──────────────┬──────────────────┬───────────────┐
//	│  Type (1B)   │  Length (4B BE)  │  Payload      │
//	└──────────────┴──────────────────┴───────────────┘
//
// One TCP-like connection is created per ffmpeg invocation. Stdout and stderr
// from the remote ffmpeg process are multiplexed back over this single
// connection using typed frames, keeping the connection count low and the
// implementation simple.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// MsgType identifies the kind of message being sent.
type MsgType uint8

const (
	// Wrapper → Coordinator
	MsgExecReq  MsgType = 0x01 // JSON ExecRequest
	MsgStdin    MsgType = 0x02 // raw bytes from stdin
	MsgStdinEOF MsgType = 0x03 // signals stdin is closed (empty payload)
	MsgSignal   MsgType = 0x04 // 1-byte OS signal number

	// Coordinator → Wrapper
	MsgStdout MsgType = 0x10 // raw bytes for stdout
	MsgStderr MsgType = 0x11 // raw bytes for stderr
	MsgExit   MsgType = 0x12 // 4-byte int32 exit code (big-endian)
)

// headerSize is the number of bytes in each frame header (type + length).
const headerSize = 5

// maxPayload is the maximum payload size accepted; protects against runaway
// allocations from a malformed or malicious frame.
const maxPayload = 16 * 1024 * 1024 // 16 MiB

// ExecRequest is the JSON payload sent in an MsgExecReq frame.
type ExecRequest struct {
	// BinaryName is the basename of the wrapper binary (e.g. "ffmpeg" or "ffprobe").
	// The coordinator uses this to select the appropriate host binary.
	BinaryName string `json:"binary_name,omitempty"`
	// Args are the raw arguments passed to the wrapper (os.Args[1:]).
	Args []string `json:"args"`
	// Env is a small set of environment variables the coordinator may need
	// (e.g. HOME, TERM). Full environment forwarding is intentionally avoided.
	Env map[string]string `json:"env,omitempty"`
	// Cwd is the working directory of the wrapper process.
	Cwd string `json:"cwd"`
}

// WriteFrame writes a single framed message to w.
func WriteFrame(w io.Writer, t MsgType, payload []byte) error {
	header := [headerSize]byte{}
	header[0] = byte(t)
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))

	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("proto: write header: %w", err)
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return fmt.Errorf("proto: write payload: %w", err)
		}
	}
	return nil
}

// ReadFrame reads one frame from r. The returned slice is freshly allocated.
func ReadFrame(r io.Reader) (MsgType, []byte, error) {
	header := [headerSize]byte{}
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, fmt.Errorf("proto: read header: %w", err)
	}

	t := MsgType(header[0])
	length := binary.BigEndian.Uint32(header[1:])

	if length > maxPayload {
		return 0, nil, fmt.Errorf("proto: payload length %d exceeds maximum %d", length, maxPayload)
	}

	if length == 0 {
		return t, nil, nil
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, fmt.Errorf("proto: read payload: %w", err)
	}
	return t, payload, nil
}

// WriteExecReq encodes and sends an ExecRequest frame.
func WriteExecReq(w io.Writer, req ExecRequest) error {
	b, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("proto: marshal exec request: %w", err)
	}
	return WriteFrame(w, MsgExecReq, b)
}

// ReadExecReq decodes an ExecRequest from a raw payload.
func ReadExecReq(payload []byte) (ExecRequest, error) {
	var req ExecRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return ExecRequest{}, fmt.Errorf("proto: unmarshal exec request: %w", err)
	}
	return req, nil
}

// WriteExit sends an exit-code frame.
func WriteExit(w io.Writer, code int) error {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(int32(code)))
	return WriteFrame(w, MsgExit, b)
}

// ReadExitCode decodes the exit code from an MsgExit payload.
func ReadExitCode(payload []byte) int {
	if len(payload) < 4 {
		return -1
	}
	return int(int32(binary.BigEndian.Uint32(payload)))
}

// WriteSignal sends a signal number to the remote side.
func WriteSignal(w io.Writer, signum uint8) error {
	return WriteFrame(w, MsgSignal, []byte{signum})
}
