// Command bench is the load generator of the popgres benchmarks (see bench/run.sh).
//
// It works with any PocketBase compatible server and measures:
//   - reads: random record views and list pages;
//   - writes: concurrent record creates;
//   - realtime: the delivery latency of the record events to many SSE clients.
//
// The results are printed as JSON.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const collectionName = "bench_posts"

var client = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        2000,
		MaxIdleConnsPerHost: 2000,
		IdleConnTimeout:     90 * time.Second,
	},
	Timeout: 30 * time.Second,
}

// Result is the result of a single scenario.
type Result struct {
	Target      string  `json:"target"`
	Scenario    string  `json:"scenario"`
	Concurrency int     `json:"concurrency"`
	Duration    float64 `json:"durationSeconds"`
	Requests    int64   `json:"requests"`
	Errors      int64   `json:"errors"`
	RPS         float64 `json:"rps"`
	P50         float64 `json:"p50ms"`
	P95         float64 `json:"p95ms"`
	P99         float64 `json:"p99ms"`

	// realtime only
	Clients   int   `json:"clients,omitempty"`
	Events    int   `json:"events,omitempty"`
	Delivered int64 `json:"delivered,omitempty"`
	Missing   int64 `json:"missing,omitempty"`
}

func main() {
	var (
		target      = flag.String("target", "", "the target name in the results")
		urls        = flag.String("url", "http://127.0.0.1:8090", "the API url (comma separated urls of multiple instances are used round-robin)")
		email       = flag.String("email", "", "the superuser email (for the setup)")
		password    = flag.String("password", "", "the superuser password (for the setup)")
		scenario    = flag.String("scenario", "", "setup, reads, writes or realtime")
		concurrency = flag.Int("c", 50, "the concurrent workers")
		duration    = flag.Duration("d", 20*time.Second, "the duration of the reads and writes scenarios")
		records     = flag.Int("records", 10000, "the records created by the setup")
		clients     = flag.Int("clients", 500, "the realtime clients")
		events      = flag.Int("events", 200, "the realtime events")
		rate        = flag.Int("rate", 20, "the realtime events per second")
		in          = flag.String("in", "", "the results file of the report scenario")
	)
	flag.Parse()

	targets := strings.Split(*urls, ",")

	var (
		result *Result
		err    error
	)

	switch *scenario {
	case "setup":
		err = setup(targets[0], *email, *password, *records, *concurrency)
	case "reads":
		result, err = reads(targets, *concurrency, *duration)
	case "writes":
		result, err = writes(targets, *concurrency, *duration)
	case "realtime":
		result, err = realtime(targets, *clients, *events, *rate)
	case "report":
		err = report(*in)
	default:
		err = fmt.Errorf("unknown scenario %q", *scenario)
	}
	if err != nil {
		log.Fatal(err)
	}

	if result != nil {
		result.Target = *target
		raw, _ := json.Marshal(result)
		fmt.Println(string(raw))
	}
}

// ---------------------------------------------------------------------------

func request(method string, url string, token string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", token)
	}

	res, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(res.Body)

	return res.StatusCode, raw, err
}

// setup creates the bench collection (with public rules) and the records of the reads scenario.
func setup(url string, email string, password string, records int, concurrency int) error {
	status, raw, err := request(http.MethodPost, url+"/api/collections/_superusers/auth-with-password", "", map[string]any{
		"identity": email,
		"password": password,
	})
	if err != nil || status != 200 {
		return fmt.Errorf("superuser auth failed: %d %s %v", status, raw, err)
	}

	auth := struct {
		Token string `json:"token"`
	}{}
	if err := json.Unmarshal(raw, &auth); err != nil {
		return err
	}

	_, _, _ = request(http.MethodDelete, url+"/api/collections/"+collectionName, auth.Token, nil)

	status, raw, err = request(http.MethodPost, url+"/api/collections", auth.Token, map[string]any{
		"name":       collectionName,
		"type":       "base",
		"listRule":   "",
		"viewRule":   "",
		"createRule": "",
		"fields": []map[string]any{
			{"name": "title", "type": "text"},
			{"name": "body", "type": "text"},
			{"name": "n", "type": "number"},
			{"name": "published", "type": "bool"},
			{"name": "sent", "type": "number"},
			{"name": "created", "type": "autodate", "onCreate": true},
		},
		"indexes": []string{"CREATE INDEX idx_bench_posts_n ON bench_posts (n)"},
	})
	if err != nil || status != 200 {
		return fmt.Errorf("failed to create the collection: %d %s %v", status, raw, err)
	}

	var created atomic.Int64
	jobs := make(chan int)
	var wg sync.WaitGroup
	var firstErr atomic.Value

	for range concurrency {
		wg.Go(func() {
			for i := range jobs {
				status, raw, err := request(http.MethodPost, url+"/api/collections/"+collectionName+"/records", "", newPost(i))
				if err != nil || status != 200 {
					firstErr.CompareAndSwap(nil, fmt.Errorf("create %d: %d %s %v", i, status, raw, err))
					continue
				}
				created.Add(1)
			}
		})
	}

	for i := range records {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	if err, _ := firstErr.Load().(error); err != nil {
		return err
	}

	log.Printf("setup: %d records created", created.Load())

	return nil
}

func newPost(i int) map[string]any {
	return map[string]any{
		"title":     fmt.Sprintf("Post %d", i),
		"body":      strings.Repeat("lorem ipsum dolor sit amet ", 10),
		"n":         i,
		"published": i%2 == 0,
	}
}

// recordIds loads the ids of the records (for the reads scenario).
func recordIds(url string) ([]string, error) {
	ids := []string{}

	for page := 1; ; page++ {
		status, raw, err := request(http.MethodGet, fmt.Sprintf("%s/api/collections/%s/records?page=%d&perPage=1000&fields=id&skipTotal=1", url, collectionName, page), "", nil)
		if err != nil || status != 200 {
			return nil, fmt.Errorf("failed to list the records: %d %v", status, err)
		}

		list := struct {
			Items []struct {
				Id string `json:"id"`
			} `json:"items"`
		}{}
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}

		for _, item := range list.Items {
			ids = append(ids, item.Id)
		}

		if len(list.Items) < 1000 {
			return ids, nil
		}
	}
}

// load runs fn with the provided concurrency for the duration and collects the latencies.
func load(concurrency int, duration time.Duration, fn func(worker int) error) *Result {
	latencies := make([][]time.Duration, concurrency)
	var errs atomic.Int64

	deadline := time.Now().Add(duration)
	start := time.Now()

	var wg sync.WaitGroup
	for w := range concurrency {
		wg.Go(func() {
			for time.Now().Before(deadline) {
				t := time.Now()
				if err := fn(w); err != nil {
					errs.Add(1)
					continue
				}
				latencies[w] = append(latencies[w], time.Since(t))
			}
		})
	}
	wg.Wait()

	elapsed := time.Since(start)

	all := slices.Concat(latencies...)
	slices.Sort(all)

	result := &Result{
		Concurrency: concurrency,
		Duration:    elapsed.Seconds(),
		Requests:    int64(len(all)),
		Errors:      errs.Load(),
		RPS:         float64(len(all)) / elapsed.Seconds(),
	}
	result.P50, result.P95, result.P99 = percentile(all, 50), percentile(all, 95), percentile(all, 99)

	return result
}

func percentile(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p / 100)
	return float64(sorted[i].Microseconds()) / 1000
}

// reads alternates random record views and random list pages.
func reads(urls []string, concurrency int, duration time.Duration) (*Result, error) {
	ids, err := recordIds(urls[0])
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, errors.New("no records (run the setup first)")
	}

	result := load(concurrency, duration, func(worker int) error {
		// the workers are spread over the instances (client side load balancing)
		url := urls[worker%len(urls)]

		var target string
		if rand.IntN(2) == 0 {
			target = fmt.Sprintf("%s/api/collections/%s/records/%s", url, collectionName, ids[rand.IntN(len(ids))])
		} else {
			target = fmt.Sprintf("%s/api/collections/%s/records?perPage=20&skipTotal=1&sort=-n&filter=%s", url, collectionName,
				urlQuery(fmt.Sprintf("n < %d && published = true", rand.IntN(len(ids)))))
		}

		status, _, err := request(http.MethodGet, target, "", nil)
		if err == nil && status != 200 {
			err = fmt.Errorf("status %d", status)
		}
		return err
	})
	result.Scenario = "reads"

	return result, nil
}

func urlQuery(s string) string {
	return strings.NewReplacer(" ", "%20", "&", "%26", "<", "%3C", "=", "%3D", "'", "%27").Replace(s)
}

// writes creates records concurrently.
func writes(urls []string, concurrency int, duration time.Duration) (*Result, error) {
	var counter atomic.Int64

	result := load(concurrency, duration, func(worker int) error {
		url := urls[worker%len(urls)]
		status, _, err := request(http.MethodPost, url+"/api/collections/"+collectionName+"/records", "", newPost(int(counter.Add(1))))
		if err == nil && status != 200 {
			err = fmt.Errorf("status %d", status)
		}
		return err
	})
	result.Scenario = "writes"

	return result, nil
}

// ---------------------------------------------------------------------------

type sseClient struct {
	res       *http.Response
	latencies []time.Duration
	received  int64
}

// connect opens a realtime connection and subscribes to the bench collection.
func connect(url string) (*sseClient, error) {
	req, err := http.NewRequest(http.MethodGet, url+"/api/realtime", nil)
	if err != nil {
		return nil, err
	}

	// no timeout for the long lived connection
	res, err := (&http.Client{Transport: client.Transport}).Do(req)
	if err != nil {
		return nil, err
	}

	reader := bufio.NewReader(res.Body)

	// PB_CONNECT
	clientId := ""
	for clientId == "" {
		line, err := reader.ReadString('\n')
		if err != nil {
			res.Body.Close()
			return nil, err
		}
		if strings.HasPrefix(line, "data:") {
			data := struct {
				ClientId string `json:"clientId"`
			}{}
			_ = json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &data)
			clientId = data.ClientId
		}
	}

	status, raw, err := request(http.MethodPost, url+"/api/realtime", "", map[string]any{
		"clientId":      clientId,
		"subscriptions": []string{collectionName},
	})
	if err != nil || status != 204 {
		res.Body.Close()
		return nil, fmt.Errorf("subscribe failed: %d %s %v", status, raw, err)
	}

	c := &sseClient{res: res}
	c.res.Body = struct {
		io.Reader
		io.Closer
	}{reader, res.Body}

	return c, nil
}

// listen reads the record events and stores their delivery latencies
// (from the "sent" field of the record set by the writer).
func (c *sseClient) listen(done <-chan struct{}) {
	reader := c.res.Body.(interface{ Read([]byte) (int, error) })
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		event := struct {
			Action string `json:"action"`
			Record struct {
				Sent float64 `json:"sent"`
			} `json:"record"`
		}{}
		// only the events created by the realtime scenario (with a "sent" time)
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil || event.Action != "create" || event.Record.Sent == 0 {
			continue
		}

		latency := time.Since(time.Unix(0, int64(event.Record.Sent)))
		c.latencies = append(c.latencies, latency)
		atomic.AddInt64(&c.received, 1)

		select {
		case <-done:
			return
		default:
		}
	}
}

// realtime connects the clients (round-robin to the urls), creates the events
// at the provided rate and measures the delivery latencies.
func realtime(urls []string, clients int, events int, rate int) (*Result, error) {
	conns := make([]*sseClient, clients)

	var wg sync.WaitGroup
	var connectErr atomic.Value
	sem := make(chan struct{}, 50)

	for i := range clients {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()

			c, err := connect(urls[i%len(urls)])
			if err != nil {
				connectErr.CompareAndSwap(nil, err)
				return
			}
			conns[i] = c
		})
	}
	wg.Wait()

	if err, _ := connectErr.Load().(error); err != nil {
		return nil, fmt.Errorf("failed to connect the realtime clients: %w", err)
	}

	done := make(chan struct{})
	var listeners sync.WaitGroup
	for _, c := range conns {
		listeners.Go(func() { c.listen(done) })
	}

	// the clients subscriptions are ready
	time.Sleep(time.Second)

	start := time.Now()
	interval := time.Second / time.Duration(rate)
	var writeErrs atomic.Int64
	var writers sync.WaitGroup

	for i := range events {
		writers.Go(func() {
			post := newPost(i)
			post["sent"] = time.Now().UnixNano()
			status, _, err := request(http.MethodPost, urls[i%len(urls)]+"/api/collections/"+collectionName+"/records", "", post)
			if err != nil || status != 200 {
				writeErrs.Add(1)
			}
		})
		time.Sleep(interval)
	}
	writers.Wait()

	// wait for the deliveries (up to 30s)
	expected := int64(clients) * int64(events-int(writeErrs.Load()))
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var total int64
		for _, c := range conns {
			total += atomic.LoadInt64(&c.received)
		}
		if total >= expected {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	elapsed := time.Since(start)

	close(done)
	var all []time.Duration
	var delivered int64
	for _, c := range conns {
		c.res.Body.Close()
	}
	listeners.Wait()
	for _, c := range conns {
		delivered += atomic.LoadInt64(&c.received)
		all = append(all, c.latencies...)
	}
	slices.Sort(all)

	result := &Result{
		Scenario:  "realtime",
		Duration:  elapsed.Seconds(),
		Requests:  int64(events),
		Errors:    writeErrs.Load(),
		RPS:       float64(delivered) / elapsed.Seconds(),
		Clients:   clients,
		Events:    events,
		Delivered: delivered,
		Missing:   expected - delivered,
	}
	result.P50, result.P95, result.P99 = percentile(all, 50), percentile(all, 95), percentile(all, 99)

	return result, nil
}

func init() {
	log.SetOutput(os.Stderr)
}

// report prints the markdown tables of the results file (the median run of the
// repeated scenarios, by requests per second).
func report(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	type key struct {
		scenario    string
		concurrency int
		target      string
	}

	groups := map[key][]*Result{}
	var keys []key

	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		r := &Result{}
		if err := json.Unmarshal([]byte(line), r); err != nil {
			return err
		}
		k := key{r.Scenario, r.Concurrency, r.Target}
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], r)
	}

	median := func(results []*Result) *Result {
		sorted := slices.Clone(results)
		slices.SortFunc(sorted, func(a, b *Result) int {
			switch {
			case a.RPS < b.RPS:
				return -1
			case a.RPS > b.RPS:
				return 1
			}
			return 0
		})
		return sorted[len(sorted)/2]
	}

	for _, scenario := range []string{"reads", "writes"} {
		fmt.Printf("\n### %s\n\n", scenario)
		fmt.Println("| Target | Concurrency | Requests/s | p50 (ms) | p95 (ms) | p99 (ms) | Errors |")
		fmt.Println("|---|---:|---:|---:|---:|---:|---:|")
		for _, k := range keys {
			if k.scenario != scenario {
				continue
			}
			r := median(groups[k])
			fmt.Printf("| %s | %d | %.0f | %.1f | %.1f | %.1f | %d |\n", r.Target, r.Concurrency, r.RPS, r.P50, r.P95, r.P99, r.Errors)
		}
	}

	var catchUp []string
	for _, k := range keys {
		if strings.HasPrefix(k.scenario, "replication catch up") {
			for _, r := range groups[k] {
				catchUp = append(catchUp, fmt.Sprintf("| %s | %s | %.0f |", r.Target, strings.TrimPrefix(r.Scenario, "replication catch up "), r.Duration))
			}
		}
	}
	if len(catchUp) > 0 {
		fmt.Printf("\n### replication catch up\n\n")
		fmt.Println("| Target | Phase | Seconds |")
		fmt.Println("|---|---|---:|")
		for _, row := range catchUp {
			fmt.Println(row)
		}
	}

	fmt.Printf("\n### realtime\n\n")
	fmt.Println("| Target | Clients | Events | Delivered | Missing | Latency p50 (ms) | p95 (ms) | p99 (ms) |")
	fmt.Println("|---|---:|---:|---:|---:|---:|---:|---:|")
	for _, k := range keys {
		if k.scenario != "realtime" {
			continue
		}
		r := median(groups[k])
		fmt.Printf("| %s | %d | %d | %d | %d | %.1f | %.1f | %.1f |\n", r.Target, r.Clients, r.Events, r.Delivered, r.Missing, r.P50, r.P95, r.P99)
	}

	return nil
}
