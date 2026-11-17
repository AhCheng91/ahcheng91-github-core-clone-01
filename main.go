// The entry <-> env lifecycle, as env/src/wire.ts settles it: bind an ephemeral port, tell the env
// the port it bound, then poll until the env says the trial is over and exit 0. This file only holds
// the harness's side of the contract; app.go builds the server this lifecycle drives.
//
//   1. $MAKEFUN_ENV_URL names the env's origin (http://store:8080 in a trial container). Without
//      it there is nothing to talk to, and the process exits 1 at once.
//   2. Create the app and listen(0): the kernel picks a free port. Never bind 8080 — the env
//      owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The env remembers the request's address plus N
//      as the entry's base URL and dials it from then on. The first register wins; a 409 means
//      another entry already registered, which is not this process's to fix.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. The env answers at once
//      ({"done": false} while the suite runs), so this is a poll, not a long-held request. A failed
//      or slow poll is retried with a short exponential backoff; the process never exits before the
//      env says done.
//   5. Close the listener and exit 0. The harness then stops the env, which writes the export the
//      scorer reads.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

/** How long one register or done request may take before it is abandoned and retried. */
const requestTimeout = 30 * time.Second

/** The first wait between retries, and the longest. */
const retryInitialMs = 250
const retryMaxMs = 2000

/** The wait after `attempt` failed requests: exponential from retryInitialMs, capped. */
func retryDelay(attempt int) time.Duration {
	if attempt > 8 {
		attempt = 8
	}
	delay := retryInitialMs
	for i := 0; i < attempt; i++ {
		delay *= 2
	}
	if delay > retryMaxMs {
		delay = retryMaxMs
	}
	return time.Duration(delay) * time.Millisecond
}

func envClient() *http.Client {
	return &http.Client{Timeout: requestTimeout}
}

/**
 * POST the bound port until the env accepts it. 200 is the normal answer; 409 means a first
 * register already won, which is success too. 400 and 405 are contract errors and end the program;
 * a network error or any other status is retried, since the env may still be starting.
 */
func register(envURL string, port int) error {
	payload, err := json.Marshal(struct {
		Port int `json:"port"`
	}{port})
	if err != nil {
		return err
	}
	client := envClient()
	for attempt := 0; ; attempt++ {
		res, err := client.Post(envURL+"/register", "application/json", bytes.NewReader(payload))
		if err == nil {
			body, rerr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
			res.Body.Close()
			status := res.StatusCode
			if status == 200 || status == 409 {
				_ = rerr
				return nil
			}
			if status == 400 || status == 405 {
				return fmt.Errorf("register answered HTTP %d: %s", status, string(body))
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

/** Poll GET /done until it answers {"done":true}; never return before that. */
func waitForDone(envURL string) {
	client := envClient()
	for attempt := 0; ; attempt++ {
		res, err := client.Get(envURL + "/done")
		if err == nil {
			body, rerr := io.ReadAll(io.LimitReader(res.Body, 1<<20))
			res.Body.Close()
			if rerr == nil && res.StatusCode >= 200 && res.StatusCode < 300 {
				var parsed struct {
					Done bool `json:"done"`
				}
				if json.Unmarshal(body, &parsed) == nil && parsed.Done {
					return
				}
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

/** Bind the port, register it, then wait for the env to call the trial finished. */
func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		os.Stderr.WriteString("[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n")
		os.Exit(1)
	}

	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %s\n", err)
		os.Exit(1)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	server := &http.Server{
		Handler:           newAppHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	go func() {
		_ = server.Serve(listener)
	}()
	fmt.Printf("[entry] listening on port %d\n", port)

	if err := register(envURL, port); err != nil {
		fmt.Fprintf(os.Stderr, "[entry] %s\n", err)
		_ = server.Close()
		os.Exit(1)
	}
	fmt.Println("[entry] registered; waiting for the env to say done")

	waitForDone(envURL)
	_ = server.Close()
	fmt.Println("[entry] done")
}