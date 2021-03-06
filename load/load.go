package main

import (
	"encoding/json"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

var (
	minTimeBetweenReqsMs = getIntEnv("MIN_REQ_TIME", 100)
	minTimeBetweenReqs   = time.Duration(int64(minTimeBetweenReqsMs)) * time.Millisecond
	clientTimeout        = time.Duration(int64(getIntEnv("CLIENT_TIMEOUT", minTimeBetweenReqsMs))) * time.Millisecond
	testTime             = time.Duration(int64(getIntEnv("TEST_TIME", 20))) * time.Second
	concurrencyFactor    = getIntEnv("CONCURRENCY_FACTOR", 1)
	targetUrl            = getStringEnv("TARGET_URL", "http://localhost:8080")
	pushGatewayAddress   = getStringEnv("PUSH_GATEWAY", "")
	metricsOutputFile    = getStringEnv("METRICS_FILE", "")

	requestDuration = prometheus.NewSummary(
		prometheus.SummaryOpts{
			Name:       "http_request_duration_microseconds",
			Help:       "Time spent on HTTP requests",
			Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		},
	)
	requestDurationHist = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_hist_microseconds",
			Help:    "Time spent on HTTP requests",
			Buckets: prometheus.LinearBuckets(0, 10000, 200),
		},
	)
	httpRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Number of HTTP requests",
		},
		[]string{"code"},
	)
	httpErrors = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "http_errors_total",
			Help: "Number of failed HTTP requests",
		},
	)
)

func getIntEnv(envKey string, alternative int) int {
	envStr := os.Getenv(envKey)
	i, err := strconv.Atoi(envStr)

	if err != nil {
		return alternative
	}

	return i
}

func getStringEnv(envKey string, alternative string) string {
	envStr := os.Getenv(envKey)
	if envStr == "" {
		return alternative
	}
	return envStr
}

func statusCodeLabel(status int) prometheus.Labels {
	return prometheus.Labels{"code": fmt.Sprintf("%d", status)}
}

func requestDurationTrack(start time.Time) {
	elapsed := float64(time.Since(start) / time.Microsecond)
	requestDuration.Observe(elapsed)
	requestDurationHist.Observe(elapsed)
}

func httpTest(httpClient *http.Client) {
	now := time.Now()
	resp, err := httpClient.Get(targetUrl)
	if err == nil {
		defer requestDurationTrack(now)
		httpRequests.With(statusCodeLabel(resp.StatusCode)).Inc()
		io.Copy(ioutil.Discard, resp.Body)
	} else {
		log.Printf("Failed HTTP request: %s\n", err)
		httpErrors.Inc()
	}
}

func runTest(testFunc func(), ticks chan time.Time) {
	for _ = range ticks {
		testFunc()
	}
}

func startTicking(tickers []chan time.Time) {
	timeout := time.After(testTime)
	tick := time.Tick(minTimeBetweenReqs)
	for {
		select {
		case <-timeout:
			return
		case t := <-tick:
			for _, ticker := range tickers {
				ticker <- t
			}
		}
	}
}

func dumpMetricsAsJson(filepath string, registry *prometheus.Registry) (err error) {
	family, err := registry.Gather()
	if err != nil {
		return
	}

	bytes, err := json.Marshal(family)
	if err != nil {
		return
	}

	err = ioutil.WriteFile(filepath, bytes, 0644)

	return
}

func main() {
	// Init Prometheus
	registry := prometheus.NewRegistry()
	registry.MustRegister(requestDuration, requestDurationHist, httpRequests, httpErrors)

	// Init HTTP transport and client
	defaultRoundTripper := http.DefaultTransport
	defaultTransportPointer, ok := defaultRoundTripper.(*http.Transport)
	if !ok {
		panic("defaultRoundTripper not an *http.Transport")
	}
	defaultTransport := *defaultTransportPointer
	defaultTransport.MaxIdleConns = 100
	defaultTransport.MaxIdleConnsPerHost = 100
	httpClient := &http.Client{
		Transport: &defaultTransport,
		Timeout:   clientTimeout,
	}

	// Init tickers for each VU
	tickers := make([]chan time.Time, concurrencyFactor)
	for i := 0; i < concurrencyFactor; i++ {
		tickers[i] = make(chan time.Time)
	}

	// Launch testers
	testFunc := func() {
		httpTest(httpClient)
	}
	for _, ticker := range tickers {
		go runTest(testFunc, ticker)
	}

	// Start the test
	log.Println("Test started")
	startTicking(tickers)
	log.Println("Test ended")

	// Push to gateway
	if pushGatewayAddress != "" {
		log.Println("Pushing metrics")
		if err := push.AddFromGatherer(
			"load_test", nil,
			pushGatewayAddress,
			registry,
		); err != nil {
			log.Panic(err)
		}
		log.Println("Metrics pushed")
	}

	// Dump metrics to file
	if metricsOutputFile != "" {
		log.Printf("Dumping metrics to %s\n", metricsOutputFile)
		if err := dumpMetricsAsJson(metricsOutputFile, registry); err != nil {
			log.Panic(err)
		}
	}

	log.Println("Exiting")
}
