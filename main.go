// main.go: the forge binary's entrypoint. It binds a port (0 = any),
// registers with the env when MAKEFUN_ENV_URL is set, serves until the
// env reports done, then exits 0. Without the env URL (local runs) it
// listens on a port and serves until killed.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	log.SetFlags(0)
	if v := os.Getenv("MAKEFUN_TRIAL_NOW"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			trialNow = n
		}
	}
	store := newStore()
	srv := newServer(store, os.Getenv("MAKEFUN_SHOP_TOKEN"))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	log.Printf("mini-forge-v2 listening on 127.0.0.1:%d", port)

	// Serve in the background: the env lifecycle below runs beside it.
	go func() {
		if err := http.Serve(ln, srv); err != nil {
			log.Fatalf("serve: %v", err)
		}
	}()

	envURL := os.Getenv("MAKEFUN_ENV_URL")
	if envURL == "" {
		// Local run: serve forever on the chosen port.
		select {}
	}
	if err := register(envURL, port); err != nil {
		log.Fatalf("register: %v", err)
	}
	log.Printf("registered with env at %s", envURL)
	// Poll /done until the suite finishes the trial.
	for {
		done, err := checkDone(envURL)
		if err == nil && done {
			log.Printf("env reports done; exiting")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// register POSTs {"port": N} to the env's /register endpoint, retrying
// briefly: the env may not be listening yet when the forge starts.
func register(envURL string, port int) error {
	body, _ := json.Marshal(map[string]any{"port": port})
	var lastErr error
	for attempt := 0; attempt < 50; attempt++ {
		resp, err := http.Post(envURL+"/register", "application/json", bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			lastErr = fmt.Errorf("register answered HTTP %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return lastErr
}

// checkDone GETs the env's /done endpoint and reports its done flag.
func checkDone(envURL string) (bool, error) {
	resp, err := http.Get(envURL + "/done")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var result struct {
		Done bool `json:"done"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}
	return result.Done, nil
}
