// Command load sends requests to Honey for a while, and reports how fast
// they were answered, and how the cache handled them: for benchmarking
// Honey in front of a real WordPress (see bench/run.sh).
//
// Usage:
//
//	go run ./bench/load -url http://127.0.0.1:8080 -host localhost:8080 -paths /,/hello-world/ -c 50 -d 20s
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

func main() {
	target := flag.String("url", "http://127.0.0.1:8080", "Honey's address")
	host := flag.String("host", "", "the Host header to send (default: the URL's)")
	paths := flag.String("paths", "/", "comma separated paths to request, at random")
	concurrency := flag.Int("c", 50, "requests at once")
	duration := flag.Duration("d", 20*time.Second, "how long to send requests for")
	cookie := flag.String("cookie", "", "a Cookie header to send (e.g. to be logged in)")
	encoding := flag.String("encoding", "gzip, deflate, br", "the Accept-Encoding header to send")
	uncached := flag.Float64("uncached", 0, "the share of requests (0-1) for made-up search pages, which the cache can't answer")
	purgeEvery := flag.Duration("purge-every", 0, "soft purge everything this often (needs -secret)")
	secret := flag.String("secret", "", "control.secret, for purging")
	flag.Parse()

	list := strings.Split(*paths, ",")
	transport := &http.Transport{
		MaxIdleConns:        *concurrency * 2,
		MaxIdleConnsPerHost: *concurrency * 2,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	if *purgeEvery > 0 {
		go purge(ctx, client, *target, *host, *secret, *purgeEvery)
	}

	results := make([]result, *concurrency)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func(res *result) {
			defer wg.Done()
			res.statuses, res.cache = map[int]int{}, map[string]int{}
			for ctx.Err() == nil {
				path := list[rand.IntN(len(list))]
				if *uncached > 0 && rand.Float64() < *uncached {
					path = fmt.Sprintf("/?s=load-%d", rand.Int64())
				}
				req, _ := http.NewRequest(http.MethodGet, *target+path, nil)
				if *host != "" {
					req.Host = *host
				}
				req.Header.Set("Accept", "text/html,application/xhtml+xml")
				req.Header.Set("Accept-Encoding", *encoding)
				req.Header.Set("User-Agent", "Mozilla/5.0 (honey load test)")
				if *cookie != "" {
					req.Header.Set("Cookie", *cookie)
				}
				began := time.Now()
				resp, err := client.Do(req)
				if err != nil {
					if ctx.Err() == nil {
						res.errors++
					}
					continue
				}
				n, _ := io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				res.latencies = append(res.latencies, time.Since(began))
				res.bytes += n
				res.statuses[resp.StatusCode]++
				res.cache[resp.Header.Get("X-Honey-Cache")]++
			}
		}(&results[i])
	}
	wg.Wait()
	elapsed := time.Since(start)
	report(results, elapsed)
}

type result struct {
	latencies []time.Duration
	bytes     int64
	errors    int
	statuses  map[int]int
	cache     map[string]int
}

// purge soft purges everything every interval, as editing a post does
func purge(ctx context.Context, client *http.Client, target, host, secret string, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Everything (/*), softly
			req, _ := http.NewRequest("PURGE", target+"/*", nil)
			if host != "" {
				req.Host = host
			}
			req.Header.Set("X-Honey-Secret", secret)
			req.Header.Set("X-Honey-Purge", "soft")
			if resp, err := client.Do(req); err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}
	}
}

func report(results []result, elapsed time.Duration) {
	var all []time.Duration
	var bytes int64
	errors := 0
	statuses, cache := map[int]int{}, map[string]int{}
	for _, r := range results {
		all = append(all, r.latencies...)
		bytes += r.bytes
		errors += r.errors
		for k, v := range r.statuses {
			statuses[k] += v
		}
		for k, v := range r.cache {
			cache[k] += v
		}
	}
	if len(all) == 0 {
		fmt.Println("no responses")
		os.Exit(1)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	pct := func(p float64) time.Duration { return all[int(float64(len(all)-1)*p)] }
	fmt.Printf("requests  %d in %s: %.0f/s, %.1f MB/s, %d errors\n", len(all), elapsed.Round(time.Millisecond),
		float64(len(all))/elapsed.Seconds(), float64(bytes)/elapsed.Seconds()/1e6, errors)
	fmt.Printf("latency   p50 %s  p90 %s  p99 %s  max %s\n", round(pct(0.5)), round(pct(0.9)), round(pct(0.99)), round(all[len(all)-1]))
	fmt.Printf("statuses  %s\n", counts(statuses))
	fmt.Printf("cache     %s\n", counts(cache))
}

func round(d time.Duration) time.Duration {
	switch {
	case d < time.Millisecond:
		return d.Round(time.Microsecond)
	default:
		return d.Round(10 * time.Microsecond)
	}
}

func counts[K comparable](m map[K]int) string {
	var parts []string
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%v: %d", k, v))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
