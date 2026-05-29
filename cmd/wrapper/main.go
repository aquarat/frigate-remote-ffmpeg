// wrapper is a drop-in ffmpeg replacement that runs inside a Docker container.
// It forwards every invocation to the frigate-coordinator daemon running on
// the macOS host via TCP, proxying stdin, stdout, stderr, signals, and the
// final exit code back transparently.
//
// Configuration (environment variables):
//
//	FRIGATE_IPC_ADDR   host:port of the coordinator (default: host.docker.internal:17346)
package main

import (
	"bufio"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
	"syscall"

	"github.com/aquarat/frigate-ffmpeg-proxy/internal/proto"
)

const defaultAddr = "host.docker.internal:17346"

// bufSize is the read buffer size for copying stdout/stderr frames.
// 64 KiB strikes a balance between syscall overhead and latency.
const bufSize = 64 * 1024

func main() {
	addr := os.Getenv("FRIGATE_IPC_ADDR")
	if addr == "" {
		addr = defaultAddr
	}

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		log.Fatalf("wrapper: connect to coordinator at %s: %v", addr, err)
	}
	defer conn.Close()

	// Build the exec request.
	req := proto.ExecRequest{
		BinaryName: filepath.Base(os.Args[0]),
		Args:       os.Args[1:],
		Env:        captureEnv(),
		Cwd:        mustGetwd(),
	}

	// Use a buffered writer for the outbound direction so that small control
	// frames (signal, stdin EOF) don't each trigger a syscall.
	bw := bufio.NewWriterSize(conn, bufSize)

	if err := proto.WriteExecReq(bw, req); err != nil {
		log.Fatalf("wrapper: send exec request: %v", err)
	}
	if err := bw.Flush(); err != nil {
		log.Fatalf("wrapper: flush exec request: %v", err)
	}

	// exitCh receives the exit code from the coordinator.
	exitCh := make(chan int, 1)
	// errCh receives the first fatal error from any goroutine.
	errCh := make(chan error, 4)

	// Hold segment files open so Frigate's maintainer (which uses psutil to
	// check open files on ffmpeg processes inside the container) does not treat
	// actively-written segments as complete and discard them. The real ffmpeg
	// runs on the host via the coordinator; only this wrapper process is visible
	// to psutil inside the container.
	go holdSegmentFiles(req.Args)

	// --- goroutine: receive frames from coordinator → write to local stdout/stderr ---
	go func() {
		for {
			t, payload, err := proto.ReadFrame(conn)
			if err != nil {
				// Connection closed after EXIT frame is normal.
				errCh <- err
				return
			}
			switch t {
			case proto.MsgStdout:
				if _, err := os.Stdout.Write(payload); err != nil {
					errCh <- err
					return
				}
			case proto.MsgStderr:
				if _, err := os.Stderr.Write(payload); err != nil {
					errCh <- err
					return
				}
			case proto.MsgExit:
				exitCh <- proto.ReadExitCode(payload)
				return
			default:
				// Unknown frame types are silently ignored for forward compatibility.
			}
		}
	}()

	// --- goroutine: pump local stdin → coordinator ---
	go func() {
		// If stdin is not a pipe (e.g. a terminal or /dev/null), send EOF
		// immediately so the coordinator can close ffmpeg's stdin.
		if !isPipe(os.Stdin) {
			if err := flushFrame(conn, proto.MsgStdinEOF, nil); err != nil {
				errCh <- err
			}
			return
		}

		buf := make([]byte, bufSize)
		for {
			n, readErr := os.Stdin.Read(buf)
			if n > 0 {
				if err := flushFrame(conn, proto.MsgStdin, buf[:n]); err != nil {
					errCh <- err
					return
				}
			}
			if readErr == io.EOF {
				if err := flushFrame(conn, proto.MsgStdinEOF, nil); err != nil {
					errCh <- err
				}
				return
			}
			if readErr != nil {
				errCh <- readErr
				return
			}
		}
	}()

	// --- goroutine: forward OS signals to coordinator ---
	sigCh := make(chan os.Signal, 8)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		for s := range sigCh {
			signum := signalNumber(s)
			if err := flushFrame(conn, proto.MsgSignal, []byte{signum}); err != nil {
				// Best-effort; don't kill the wrapper over a signal forward failure.
				log.Printf("wrapper: forward signal %v: %v", s, err)
			}
		}
	}()

	// Wait for exit code or a fatal error.
	select {
	case code := <-exitCh:
		os.Exit(code)
	case err := <-errCh:
		// If we already have an exit code waiting, use it.
		select {
		case code := <-exitCh:
			os.Exit(code)
		default:
		}
		log.Fatalf("wrapper: connection error: %v", err)
	}
}

// flushFrame writes a single frame directly to the connection (unbuffered),
// which is safe because each call is already a complete logical unit.
func flushFrame(w io.Writer, t proto.MsgType, payload []byte) error {
	return proto.WriteFrame(w, t, payload)
}

// captureEnv returns a small set of environment variables that the coordinator
// may need. We deliberately avoid forwarding the full environment.
func captureEnv() map[string]string {
	keys := []string{"HOME", "USER", "TERM", "LANG", "TZ"}
	env := make(map[string]string, len(keys))
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return env
}

func mustGetwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "/"
	}
	return cwd
}

// isPipe reports whether f refers to a named pipe or anonymous pipe.
func isPipe(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeNamedPipe != 0
}

// signalNumber converts an os.Signal to a uint8 signal number for the wire.
func signalNumber(s os.Signal) uint8 {
	if sys, ok := s.(syscall.Signal); ok {
		return uint8(sys)
	}
	return 15 // SIGTERM as fallback
}

// holdSegmentFiles finds the cache-segment output path in the ffmpeg args,
// then polls the cache directory every 200 ms.  It holds a read-only file
// descriptor open on only the NEWEST segment for this camera so that psutil
// (used by Frigate's recording maintainer) sees it as in-use and skips it.
// When a newer segment appears the previous fd is closed, allowing the
// maintainer to validate and move that completed segment to /recordings.
//
// Background: the real ffmpeg runs on the host via the coordinator; only this
// wrapper process is visible to psutil inside the container.  Without holding
// the fd, the maintainer treats every segment (including ones still being
// written) as available, probes them before the moov atom is written, and
// discards them as invalid.
func holdSegmentFiles(args []string) {
	// Find the segment output pattern, e.g. /tmp/cache/camera@%Y%m%d%H%M%S%z.mp4
	var cacheDir, cameraPrefix string
	for _, arg := range args {
		if strings.HasSuffix(arg, ".mp4") {
			base := filepath.Base(arg)
			if idx := strings.Index(base, "@"); idx > 0 {
				cacheDir = filepath.Dir(arg)
				cameraPrefix = base[:idx]
				break
			}
		}
	}
	if cameraPrefix == "" {
		return // not a segmented-recording invocation
	}

	var newestName string
	var newestFile *os.File

	ticker := time.NewTicker(200 * time.Millisecond)
	defer func() {
		ticker.Stop()
		if newestFile != nil {
			newestFile.Close()
		}
	}()

	for range ticker.C {
		entries, err := os.ReadDir(cacheDir)
		if err != nil {
			continue
		}
		// Find the segment with the lexicographically greatest name (the
		// timestamp format YYYYMMDDHHMMSS sorts correctly as a string).
		var latestName string
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasPrefix(name, cameraPrefix+"@") || !strings.HasSuffix(name, ".mp4") {
				continue
			}
			if name > latestName {
				latestName = name
			}
		}
		if latestName == "" || latestName == newestName {
			continue // nothing new
		}
		// A newer segment has appeared — open it and release the old fd.
		f, err := os.Open(filepath.Join(cacheDir, latestName))
		if err != nil {
			continue
		}
		if newestFile != nil {
			newestFile.Close() // release previous; maintainer can now process it
		}
		newestFile = f
		newestName = latestName
	}
}
