// Package command owns the shared flags and process lifetime for both builds.
// A nil UI callback serves Web/API/DAV only; a non-nil callback additionally opens
// the native client on the caller's OS thread, after the HTTP service is ready.
package command

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"guludrive/app"
)

// Run parses commands without exiting the process. It opens one service on the
// configured fixed port and calls openUI synchronously, never in a goroutine.
// The callback must return when ctx is canceled by a signal or serving failure.
// Returning from the callback also closes HTTP, drains the app and releases its
// database and instance lock before Run returns.
func Run(args []string, version string, openUI func(context.Context, string) error) (result error) {
	if openUI != nil {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}
	flags := flag.NewFlagSet("guludrive", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = printHelp
	versionOnly := flags.Bool("version", false, "Print the build version")
	if err := flags.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return errors.New("invalid flags; use guludrive -help")
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments; use guludrive -help")
	}
	if *versionOnly {
		fmt.Fprintln(os.Stdout, version)
		return nil
	}
	cfg := app.Config{Listen: "127.0.0.1:8787"}

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return errors.New("cannot listen; check address and port availability")
	}
	defer listener.Close()
	var uiURL string
	if openUI != nil {
		uiURL, err = loopbackURL(listener.Addr())
		if err != nil {
			return err
		}
	}
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	lifetime, cancel := context.WithCancel(signals)
	defer cancel()
	service, err := app.Open(lifetime, cfg)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler: service.Handler(), ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 90 * time.Second, MaxHeaderBytes: 32 << 10,
		// Transfers have no whole-request timeout. App shutdown cancels reads;
		// public deployments should set their trusted reverse proxy limits.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	ended := make(chan error, 1)
	go func() {
		ended <- server.Serve(listener)
		cancel() // Serving failure must also release a running native window.
	}()
	defer func() {
		cancel()
		shutdown, done := context.WithTimeout(context.Background(), 20*time.Second)
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
		done()
		// Close cancels and drains store workers/readers before closing the one
		// shared DB. Wait for the instance lock rather than exiting mid-cleanup.
		result = errors.Join(result, service.Close(context.Background()))
		if err := <-ended; !errors.Is(err, http.ErrServerClosed) {
			result = errors.Join(result, errors.New("HTTP serving stopped unexpectedly"))
		}
	}()
	log.Printf("GuluDrive %s listening on %s (Google Photos)", version, listener.Addr())
	if address, ok := listener.Addr().(*net.TCPAddr); ok && !address.IP.IsLoopback() {
		log.Print("WARNING: non-loopback HTTP listener; use a trusted TLS reverse proxy or SSH tunnel. Never send credentials over plaintext networks.")
	}
	if openUI == nil {
		<-lifetime.Done()
		return nil
	}
	err = openUI(lifetime, uiURL)
	if errors.Is(err, context.Canceled) && lifetime.Err() != nil {
		return nil
	}
	return err
}

func printHelp() {
	fmt.Fprint(os.Stdout, `GuluDrive
Usage: guludrive [-help] [-version]

Start without arguments to serve Web, API and WebDAV at 127.0.0.1:8787.
The default build also opens the native window; no-ui only omits that window.
On first use, initialize the administrator in either UI, then sign in.
Private state (store.db, temp/, instance.lock) stays beside the executable.
Both builds include the Web UI; no external assets or configuration are needed.
`)
}

func loopbackURL(address net.Addr) (string, error) {
	tcp, ok := address.(*net.TCPAddr)
	if !ok || tcp.Port < 1 {
		return "", errors.New("cannot determine the fixed service address")
	}
	host := tcp.IP.String()
	if tcp.IP.IsUnspecified() {
		if tcp.IP.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	} else if !tcp.IP.IsLoopback() {
		return "", errors.New("native UI requires a loopback or wildcard listen address")
	}
	return "http://" + net.JoinHostPort(host, fmt.Sprint(tcp.Port)), nil
}
