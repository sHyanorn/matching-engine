//go:build ignore

// The parts of scripts/pair.sh that the Pod's image has no tool for:
//
//	go run scripts/pair.go probe                 the clock's cost and SO_BUSY_POLL
//	go run scripts/pair.go serve ADDR PATH FILE  serve FILE at /PATH/NAME on ADDR
//
// probe prints the percentiles of an empty timed region, as
// BenchmarkApplyLatency reports them (clock-p50-ns, clock-p99-ns), and
// the result of setting SO_BUSY_POLL on a TCP socket, which an
// unprivileged process may lower but not raise. serve answers GET of
// /PATH/NAME, NAME being FILE's base name, with FILE, and 404 to
// anything else.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"golang.org/x/sys/unix"
)

func main() {
	switch {
	case len(os.Args) == 2 && os.Args[1] == "probe":
		probe()
	case len(os.Args) == 5 && os.Args[1] == "serve":
		file := os.Args[4]
		http.HandleFunc("GET /"+os.Args[3]+"/"+filepath.Base(file), func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, file)
		})
		log.Fatal(http.ListenAndServe(os.Args[2], nil))
	default:
		fmt.Fprintln(os.Stderr, "usage: pair.go probe | pair.go serve ADDR PATH FILE")
		os.Exit(2)
	}
}

func probe() {
	clock := make([]time.Duration, 1_000_000)
	for i := range clock {
		t0 := time.Now()
		clock[i] = time.Since(t0)
	}
	slices.Sort(clock)
	fmt.Printf("empty timed region over %d samples: p50 %v, p99 %v, max %v\n",
		len(clock), clock[len(clock)/2-1], clock[len(clock)*99/100-1], clock[len(clock)-1])

	result := "ok"
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err == nil {
		err = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_BUSY_POLL, 50)
		unix.Close(fd)
	}
	if err != nil {
		result = err.Error()
	}
	fmt.Println("setsockopt(SO_BUSY_POLL, 50 µs):", result)
}
