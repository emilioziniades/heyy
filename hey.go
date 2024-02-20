// Copyright 2014 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command hey is an HTTP load generator.
package main

import (
	"flag"
	"fmt"
	"math"
	"net/http"
	gourl "net/url"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/rakyll/hey/requester"
)

const (
	headerRegexp = `^([\w-]+):\s*(.+)`
	authRegexp   = `^(.+):([^\s].+)`
	heyUA        = "hey/0.0.1"
)

var usage = `Usage: hey [options...] <url>

Options:
  -n  Number of requests to run. Default is 200.
  -c  Number of workers to run concurrently. Total number of requests cannot
      be smaller than the concurrency level. Default is 50.
  -q  Rate limit, in queries per second (QPS) per worker. Default is no rate limit.
  -z  Duration of application to send requests. When duration is reached,
      application stops and exits. If duration is specified, n is ignored.
      Examples: -z 10s -z 3m.
  -o  Output type. If none provided, a summary is printed.
      "csv" is the only supported alternative. Dumps the response
      metrics in comma-separated values format.

  -m  HTTP method, one of GET, POST, PUT, DELETE, HEAD, OPTIONS.
  -H  Custom HTTP header. You can specify as many as needed by repeating the flag.
      For example, -H "Accept: text/html" -H "Content-Type: application/xml" .
  -t  Timeout for each request in seconds. Default is 20, use 0 for infinite.
  -A  HTTP Accept header.
  -d  HTTP request body.
  -D  HTTP request body from file. For example, /home/user/file.txt or ./file.txt.
  -T  Content-type, defaults to "text/html".
  -U  User-Agent, defaults to version "hey/0.0.1".
  -a  Basic authentication, username:password.
  -x  HTTP Proxy address as host:port.
  -h2 Enable HTTP/2.

  -host	HTTP Host header.

  -disable-compression  Disable compression.
  -disable-keepalive    Disable keep-alive, prevents re-use of TCP
                        connections between different HTTP requests.
  -disable-redirects    Disable following of HTTP redirects
  -cpus                 Number of used cpu cores.
                        (default for current machine is %d cores)
`

type options struct {
	method             *string
	headers            *headerSlice
	body               *string
	bodyFile           *string
	accept             *string
	contentType        *string
	authHeader         *string
	hostHeader         *string
	userAgent          *string
	output             *string
	concurrentWorkers  *int
	nRequests          *int
	queriesPerSecond   *float64
	timoutSeconds      *int
	duration           *time.Duration
	http2              *bool
	cpus               *int
	disableCompression *bool
	disableKeepAlives  *bool
	disableRedirects   *bool
	proxyAddr          *string
}

func main() {
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, fmt.Sprintf(usage, runtime.NumCPU()))
	}

	defaults := defaultOpts()

	var opts = options{
		method:             flag.String("m", *defaults.method, ""),
		headers:            defaults.headers,
		body:               flag.String("d", *defaults.body, ""),
		bodyFile:           flag.String("D", *defaults.bodyFile, ""),
		accept:             flag.String("A", *defaults.accept, ""),
		contentType:        flag.String("T", *defaults.contentType, ""),
		authHeader:         flag.String("a", *defaults.authHeader, ""),
		hostHeader:         flag.String("host", *defaults.hostHeader, ""),
		userAgent:          flag.String("U", *defaults.userAgent, ""),
		concurrentWorkers:  flag.Int("c", *defaults.concurrentWorkers, ""),
		nRequests:          flag.Int("n", *defaults.nRequests, ""),
		queriesPerSecond:   flag.Float64("q", *defaults.queriesPerSecond, ""),
		timoutSeconds:      flag.Int("t", *defaults.timoutSeconds, ""),
		duration:           flag.Duration("z", *defaults.duration, ""),
		http2:              flag.Bool("h2", *defaults.http2, ""),
		cpus:               flag.Int("cpus", *defaults.cpus, ""),
		disableCompression: flag.Bool("disable-compression", *defaults.disableCompression, ""),
		disableKeepAlives:  flag.Bool("disable-keepalive", *defaults.disableKeepAlives, ""),
		disableRedirects:   flag.Bool("disable-redirects", *defaults.disableRedirects, ""),
		proxyAddr:          flag.String("x", *defaults.proxyAddr, ""),
	}

	flag.Var(opts.headers, "H", "")

	flag.Parse()
	if flag.NArg() < 1 {
		usageAndExit("")
	}

	runtime.GOMAXPROCS(*opts.cpus)
	num := *opts.nRequests
	conc := *opts.concurrentWorkers
	q := *opts.queriesPerSecond
	dur := *opts.duration

	if dur > 0 {
		num = math.MaxInt32
		if conc <= 0 {
			usageAndExit("-c cannot be smaller than 1.")
		}
	} else {
		if num <= 0 || conc <= 0 {
			usageAndExit("-n and -c cannot be smaller than 1.")
		}

		if num < conc {
			usageAndExit("-n cannot be less than -c.")
		}
	}

	url := flag.Args()[0]

	// set content-type
	header := make(http.Header)
	header.Set("Content-Type", *opts.contentType)
	// set any other additional repeatable headers
	for _, h := range *opts.headers {
		match, err := parseInputWithRegexp(h, headerRegexp)
		if err != nil {
			usageAndExit(err.Error())
		}
		header.Set(match[1], match[2])
	}

	if *opts.accept != "" {
		header.Set("Accept", *opts.accept)
	}

	// set basic auth if set
	var username, password string
	if *opts.authHeader != "" {
		match, err := parseInputWithRegexp(*opts.authHeader, authRegexp)
		if err != nil {
			usageAndExit(err.Error())
		}
		username, password = match[1], match[2]
	}

	var bodyAll []byte
	if *opts.body != "" {
		bodyAll = []byte(*opts.body)
	}
	if *opts.bodyFile != "" {
		slurp, err := os.ReadFile(*opts.bodyFile)
		if err != nil {
			errAndExit(err.Error())
		}
		bodyAll = slurp
	}

	var proxyURL *gourl.URL
	if *opts.proxyAddr != "" {
		var err error
		proxyURL, err = gourl.Parse(*opts.proxyAddr)
		if err != nil {
			usageAndExit(err.Error())
		}
	}

	method := strings.ToUpper(*opts.method)
	req, err := http.NewRequest(strings.ToUpper(method), url, nil)
	if err != nil {
		usageAndExit(err.Error())
	}
	req.ContentLength = int64(len(bodyAll))
	if username != "" || password != "" {
		req.SetBasicAuth(username, password)
	}

	// set host header if set
	if *opts.hostHeader != "" {
		req.Host = *opts.hostHeader
	}

	ua := header.Get("User-Agent")
	if ua == "" {
		ua = heyUA
	} else {
		ua += " " + heyUA
	}
	header.Set("User-Agent", ua)

	// set userAgent header if set
	if *opts.userAgent != "" {
		ua = *opts.userAgent + " " + heyUA
		header.Set("User-Agent", ua)
	}

	req.Header = header

	w := &requester.Work{
		Request:            req,
		RequestBody:        bodyAll,
		N:                  num,
		C:                  conc,
		QPS:                q,
		Timeout:            *opts.timoutSeconds,
		DisableCompression: *opts.disableCompression,
		DisableKeepAlives:  *opts.disableKeepAlives,
		DisableRedirects:   *opts.disableRedirects,
		H2:                 *opts.http2,
		ProxyAddr:          proxyURL,
		Output:             *opts.output,
	}
	w.Init()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		w.Stop()
	}()
	if dur > 0 {
		go func() {
			time.Sleep(dur)
			w.Stop()
		}()
	}
	w.Run()
}

func defaultOpts() options {
	return options{
		method:             ref("GET"),
		headers:            new(headerSlice),
		body:               ref(""),
		bodyFile:           ref(""),
		accept:             ref(""),
		contentType:        ref("text/html"),
		authHeader:         ref(""),
		hostHeader:         ref(""),
		userAgent:          ref(""),
		concurrentWorkers:  ref(50),
		nRequests:          ref(200),
		queriesPerSecond:   ref(float64(0)),
		timoutSeconds:      ref(20),
		duration:           ref(time.Duration(0)),
		http2:              ref(false),
		cpus:               ref(runtime.GOMAXPROCS(-1)),
		disableCompression: ref(false),
		disableKeepAlives:  ref(false),
		disableRedirects:   ref(false),
		proxyAddr:          ref(""),
	}
}

func ref[T any](t T) *T {
	return &t
}

func errAndExit(msg string) {
	fmt.Fprintf(os.Stderr, msg)
	fmt.Fprintf(os.Stderr, "\n")
	os.Exit(1)
}

func usageAndExit(msg string) {
	if msg != "" {
		fmt.Fprintf(os.Stderr, msg)
		fmt.Fprintf(os.Stderr, "\n\n")
	}
	flag.Usage()
	fmt.Fprintf(os.Stderr, "\n")
	os.Exit(1)
}

func parseInputWithRegexp(input, regx string) ([]string, error) {
	re := regexp.MustCompile(regx)
	matches := re.FindStringSubmatch(input)
	if len(matches) < 1 {
		return nil, fmt.Errorf("could not parse the provided input; input = %v", input)
	}
	return matches, nil
}

type headerSlice []string

func (h *headerSlice) String() string {
	return fmt.Sprintf("%s", *h)
}

func (h *headerSlice) Set(value string) error {
	*h = append(*h, value)
	return nil
}
