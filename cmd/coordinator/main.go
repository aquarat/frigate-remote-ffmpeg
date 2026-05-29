// coordinator is a persistent macOS daemon that receives ffmpeg invocations
// from the frigate-wrapper running inside a Docker container and executes them
// natively on the host, enabling access to VideoToolbox hardware acceleration.
//
// Each connection corresponds to one ffmpeg process. Stdin, stdout, stderr,
// signals, and exit codes are all proxied transparently over TCP.
// The wrapper connects via host.docker.internal, so no volume share is needed
// for the transport — only the media/config directories are volume-mounted.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/aquarat/frigate-ffmpeg-proxy/internal/pathmap"
	"github.com/aquarat/frigate-ffmpeg-proxy/internal/proto"
)

// Config is the coordinator configuration file structure.
type Config struct {
	// FFmpegPath is the absolute path to the native ffmpeg binary on the host.
	FFmpegPath string `yaml:"ffmpeg_path"`
	// FFprobePath is the absolute path to the native ffprobe binary on the host.
	// Defaults to ffprobe in the same directory as ffmpeg_path if not set.
	FFprobePath string `yaml:"ffprobe_path"`
	// ListenAddr is the TCP address the coordinator binds to.
	// Bind to loopback (127.0.0.1) to avoid network exposure.
	ListenAddr string `yaml:"listen_addr"`
	// LogLevel controls verbosity: "debug", "info", "warn", "error".
	LogLevel string `yaml:"log_level"`
	// PathMappings maps container-side path prefixes to host-side prefixes.
	PathMappings []pathmap.Mapping `yaml:"path_mappings"`
}

var defaultConfig = Config{
	FFmpegPath:  "/usr/local/bin/ffmpeg",
	FFprobePath: "/usr/local/bin/ffprobe",
	ListenAddr:  "127.0.0.1:17346",
	LogLevel:    "info",
}

// bufSize is the I/O buffer used when relaying stdout/stderr chunks.
const bufSize = 64 * 1024

// parseLogLevel converts a log level string to the corresponding slog.Level.
// Unknown values default to slog.LevelInfo.
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func main() {
	cfgPath := flag.String("config", "", "path to coordinator.yaml (optional)")
	flag.Parse()

	cfg := defaultConfig
	if *cfgPath != "" {
		if err := loadConfig(*cfgPath, &cfg); err != nil {
			fmt.Fprintf(os.Stderr, "coordinator: load config: %v\n", err)
			os.Exit(1)
		}
	}

	// Set up structured logging with the configured level.
	// Logs always go to stderr; redirect at the OS or shell level if needed.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLogLevel(cfg.LogLevel),
	})))

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		slog.Error("coordinator: listen failed", "addr", cfg.ListenAddr, "err", err)
		os.Exit(1)
	}

	slog.Info("coordinator: listening", "addr", cfg.ListenAddr)
	slog.Info("coordinator: ffmpeg binary", "path", cfg.FFmpegPath)
	if cfg.FFprobePath == "" {
		cfg.FFprobePath = filepath.Join(filepath.Dir(cfg.FFmpegPath), "ffprobe")
	}
	slog.Info("coordinator: ffprobe binary", "path", cfg.FFprobePath)

	mapper := pathmap.New(cfg.PathMappings)

	// SIGINT (Ctrl+C) exits immediately; SIGTERM drains in-flight connections.
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sigCh
		if s == syscall.SIGINT {
			slog.Info("coordinator: interrupted, exiting")
			os.Exit(0)
		}
		slog.Info("coordinator: draining connections", "signal", s)
		cancel()
		ln.Close()
	}()

	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				done := make(chan struct{})
				go func() { wg.Wait(); close(done) }()
				select {
				case <-done:
				case <-time.After(30 * time.Second):
					slog.Warn("coordinator: timed out waiting for connections to close")
				}
				slog.Info("coordinator: exited cleanly")
				return
			default:
				slog.Error("coordinator: accept error", "err", err)
				continue
			}
		}

		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			handleConnection(c, cfg, mapper)
		}(conn)
	}
}

// handleConnection manages the full lifecycle of one ffmpeg invocation.
func handleConnection(conn net.Conn, cfg Config, mapper *pathmap.Mapper) {
	defer conn.Close()

	t, payload, err := proto.ReadFrame(conn)
	if err != nil {
		slog.Error("coordinator: read exec request", "err", err)
		return
	}
	if t != proto.MsgExecReq {
		slog.Error("coordinator: unexpected message type", "want", "MsgExecReq", "got", fmt.Sprintf("0x%02x", t))
		return
	}

	req, err := proto.ReadExecReq(payload)
	if err != nil {
		slog.Error("coordinator: decode exec request", "err", err)
		return
	}

	remappedArgs := mapper.RewriteArgs(req.Args)

	binaryPath := cfg.FFmpegPath
	if req.BinaryName == "ffprobe" {
		binaryPath = cfg.FFprobePath
	}
	slog.Debug("coordinator: exec", "binary", binaryPath, "args", remappedArgs)

	cmd := exec.Command(binaryPath, remappedArgs...)
	cmd.Env = buildEnv(req.Env)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		slog.Error("coordinator: stdin pipe", "err", err)
		return
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		slog.Error("coordinator: stdout pipe", "err", err)
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		slog.Error("coordinator: stderr pipe", "err", err)
		return
	}

	if err := cmd.Start(); err != nil {
		slog.Error("coordinator: start ffmpeg", "err", err)
		_ = proto.WriteExit(conn, 1)
		return
	}

	// connMu serialises writes to conn from concurrent stdout/stderr goroutines.
	var connMu sync.Mutex
	safeWrite := func(t proto.MsgType, p []byte) error {
		connMu.Lock()
		defer connMu.Unlock()
		return proto.WriteFrame(conn, t, p)
	}

	var ioWg sync.WaitGroup

	ioWg.Add(1)
	go func() {
		defer ioWg.Done()
		pumpOutput(stdoutPipe, proto.MsgStdout, safeWrite)
	}()

	ioWg.Add(1)
	go func() {
		defer ioWg.Done()
		pumpOutput(stderrPipe, proto.MsgStderr, safeWrite)
	}()

	// readIncoming runs independently for the full lifetime of the ffmpeg
	// process, forwarding stdin and signals. It is NOT part of ioWg: including
	// it would deadlock — the wrapper blocks waiting for EXIT, and readIncoming
	// blocks waiting for the wrapper to send something, so ioWg.Wait() would
	// never return. Instead, readIncoming exits naturally when conn is closed
	// by handleConnection's deferred conn.Close() after EXIT is sent.
	go readIncoming(conn, stdinPipe, cmd)

	// Wait only for the output pumps: ffmpeg's stdout and stderr are done.
	ioWg.Wait()

	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	slog.Info("coordinator: ffmpeg exited", "exit_code", exitCode)

	connMu.Lock()
	_ = proto.WriteExit(conn, exitCode)
	connMu.Unlock()
	// conn.Close() via defer will cause readIncoming to get a read error and exit.
}

// pumpOutput reads from r and sends its bytes as framed messages of type t.
func pumpOutput(r io.Reader, t proto.MsgType, write func(proto.MsgType, []byte) error) {
	buf := make([]byte, bufSize)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			if err := write(t, buf[:n]); err != nil {
				return
			}
		}
		if readErr != nil {
			return
		}
	}
}

// readIncoming processes frames from the wrapper: stdin data, stdin EOF, signals.
func readIncoming(conn net.Conn, stdinPipe io.WriteCloser, cmd *exec.Cmd) {
	stdinClosed := false
	for {
		t, payload, err := proto.ReadFrame(conn)
		if err != nil {
			if !stdinClosed {
				stdinPipe.Close()
			}
			return
		}

		switch t {
		case proto.MsgStdin:
			if !stdinClosed && len(payload) > 0 {
				if _, err := stdinPipe.Write(payload); err != nil {
					slog.Warn("coordinator: write to ffmpeg stdin", "err", err)
					stdinPipe.Close()
					stdinClosed = true
				}
			}

		case proto.MsgStdinEOF:
			if !stdinClosed {
				stdinPipe.Close()
				stdinClosed = true
			}

		case proto.MsgSignal:
			if len(payload) < 1 {
				continue
			}
			sig := syscall.Signal(payload[0])
			if cmd.Process != nil {
				if err := cmd.Process.Signal(sig); err != nil {
					slog.Warn("coordinator: forward signal", "signal", sig, "err", err)
				}
			}
		}
	}
}

// buildEnv constructs the environment for the ffmpeg subprocess from the
// coordinator's own environment, overlaying non-conflicting wrapper values.
func buildEnv(received map[string]string) []string {
	env := os.Environ()

	hostKeys := make(map[string]bool, len(env))
	for _, kv := range env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				hostKeys[kv[:i]] = true
				break
			}
		}
	}
	for k, v := range received {
		if !hostKeys[k] {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// loadConfig reads a YAML config file into cfg, merging over the defaults.
func loadConfig(path string, cfg *Config) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return yaml.NewDecoder(f).Decode(cfg)
}
