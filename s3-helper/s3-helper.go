// s3-helper is used to assist nginx with various AWS related tasks
package main

import (
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path"
	"runtime"
	// "strings"
	"syscall"
	"time"

	"github.com/crunchyroll/go-aws-auth"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Default config file
const configFileDefault = "/etc/s3-helper.yml"

// Config holds the global config
type Config struct {
	Listen string `yaml:"listen"`

	Concurrency int `optional:"true"`

	S3Timeout time.Duration `yaml:"s3_timeout"`
	S3Retries int           `yaml:"s3_retries"`

	S3Region string `yaml:"s3_region"`
	S3Bucket string `yaml:"s3_bucket"`
	S3Path   string `yaml:"s3_prefix" optional:"true"`
	LogLevel string `optional:"true"`
}

const defaultConfValues = `
    listen: "127.0.0.1:8080"
    loglevel: "error"
    s3_timeout:  5s
    s3_retries:  5
    concurrency:   0
`

var conf Config
var progName string
var statRate float32 = 1

// List of headers to forward in response
var headerForward = map[string]bool{
	"Date":           true,
	"Content-Length": true,
	"Content-Range":  true,
	"Content-Type":   true,
	"Last-Modified":  true,
	"ETag":           true,
}

const serverName = "VOD S3 Helper"

// Initialize process runtime
func initRuntime() {
	ncpus := runtime.NumCPU()
	log.Info().Msg(fmt.Sprintf("System has %d CPUs", ncpus))

	conc := ncpus
	if conf.Concurrency != 0 {
		conc = conf.Concurrency
	}
	log.Info().Msg(fmt.Sprintf("Setting thread concurrency to %d", conc))
	runtime.GOMAXPROCS(conc)

}

func forwardToS3(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", serverName)

	if r.Method != "GET" && r.Method != "HEAD" {
		w.WriteHeader(405)
		return
	}

	// Make sure that RemoteAddr is 127.0.0.1 so it comes off a local proxy
	// a := strings.SplitN(r.RemoteAddr, ":", 2)
	// if len(a) != 2 || a[0] != "127.0.0.1" {
	// 	w.WriteHeader(403)
	// 	return
	// }

	upath := r.URL.Path
	byterange := r.Header.Get("Range")
	logger := log.With().
		Str("object", upath).
		Str("range", byterange).
		Str("method", r.Method).
		Logger()
	s3url := fmt.Sprintf("http://s3.%s.amazonaws.com/%s%s%s", conf.S3Region, conf.S3Bucket, conf.S3Path, upath)
	r2, err := http.NewRequest(r.Method, s3url, nil)
	if err != nil {
		w.WriteHeader(403)
		logger.Error().
			Str("error", err.Error()).
			Str("url", s3url).
			Msg("Failed to create GET request")
		return
	}

	r2 = awsauth.SignForRegion(r2, conf.S3Region, "s3")

	logger.Info().
		Str("RawQuery", r2.URL.RawQuery).
		Msg("Received request")

	url := r2.URL.String()
	logger.Info().
		Str("url", url).
		Msg("Received request")

	var bodySize int64
	r2.Header.Set("Host", r2.URL.Host)
	// parse the byterange request header to derive the content-length requested
	// so we know how much data we need to xfer from s3 to the client.
	if byterange != "" {
		r2.Header.Set("Range", byterange)
	}

	nretries := 0

	var resp *http.Response

	// setup client outside of for loop since we don't
	// need to define it multiple times and failures
	// shouldn't need a new client
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   conf.S3Timeout,
				KeepAlive: 1 * time.Second,
			}).DialContext,
			IdleConnTimeout:   conf.S3Timeout,
			DisableKeepAlives: true, // terminates open connections
		}}

	for {
		resp, err = client.Do(r2)
		if err == nil {
			break
		}

		// Bail out on non-timeout error, or too many timeouts.
		netErr, ok := err.(net.Error)
		isTimeout := ok && netErr.Timeout()

		if nretries >= conf.S3Retries || !isTimeout {
			logger.Error().
				Str("error", err.Error()).
				Msg(fmt.Sprintf("Connection failed after #%d retries", conf.S3Retries))
			w.WriteHeader(500)
			return
		}

		logger.Error().
			Str("error", err.Error()).
			Msg(fmt.Sprintf("Connection timeout: retry #%d", nretries))
		nretries++
	}

	defer resp.Body.Close()

	header := resp.Header
	for name, hflag := range headerForward {
		if hflag {
			if v := header.Get(name); v != "" {
				w.Header().Set(name, v)
			}
		}
	}

	// we can't buffer in ram or to disk so write the body
	// directly to the return body buffer and stream out
	// to the client. if we have a failure, we can't notify
	// the client, this is a poor design with potential
	// silent truncation of the output.
	//
	w.WriteHeader(resp.StatusCode)
	var bytes int64
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		if r2.Method != "HEAD" {
			logger.Info().
				Int64("content-length", bodySize).
				Msg(fmt.Sprintf("Begin data transfer of #%d bytes", bodySize))
			bytes, err = io.Copy(w, resp.Body)
			if err != nil {
				// we failed copying the body yet already sent the http header so can't tell
				// the client that it failed.
				logger.Error().
					Str("error", err.Error()).
					Int64("content-length", bodySize).
					Int64("recv", bytes).
					Msg("Failed to copy body")
			} else {
				logger.Info().
					Int64("content-length", bodySize).
					Int64("recv", bytes).
					Msg("Success copying body")
			}
		}
	} else {
		logger.Error().
			Str("error", fmt.Sprintf("Response Status Code: %d", resp.StatusCode)).
			Int("statuscode", resp.StatusCode).
			Int64("content-length", bodySize).
			Int64("recv", bytes).
			Msg("Bad connection status response code")
	}
}

func main() {
	zerolog.TimeFieldFormat = ""
	rand.Seed(time.Now().UnixNano())

	progName = path.Base(os.Args[0])

	// configFile := flag.String("config", configFileDefault, "config file to use")
	pprofFlag := flag.Bool("pprof", false, "enable pprof")
	flag.Parse()

	// conf.LogLevel = "error"
	conf.Listen = "0.0.0.0:8080"
	conf.S3Region = os.Getenv("S3_REGION")
	conf.S3Bucket = os.Getenv("S3_BUCKET")
	conf.S3Timeout, _ = time.ParseDuration("5s")
	conf.S3Retries =  5
	conf.Concurrency =  0
	conf.LogLevel = os.Getenv("S3_LOGLEVEL")

	log.Info().Msg("Starting up")
	defer log.Info().Msg("Shutting down")

	log.Info().Msg(fmt.Sprintf("S3Region: %s", conf.S3Region))
	log.Info().Msg(fmt.Sprintf("S3Bucket: %s", conf.S3Bucket))
	log.Info().Msg(fmt.Sprintf("LogLevel: %s", conf.LogLevel))

	initRuntime()

	// nr := newrelic.NewNewRelic(&conf.NewRelic)
	mux := http.NewServeMux()

	// mux.Handle(nr.MonitorHandler("/", http.HandlerFunc(forwardToS3)))
	mux.Handle("/", http.HandlerFunc(forwardToS3))

	if *pprofFlag {
		mux.Handle("/debug/pprof/", http.HandlerFunc(pprof.Index))
		mux.Handle("/debug/pprof/symbol", http.HandlerFunc(pprof.Symbol))
		mux.Handle("/debug/pprof/cmdline", http.HandlerFunc(pprof.Cmdline))
		mux.Handle("/debug/pprof/profile", http.HandlerFunc(pprof.Profile))
		log.Info().Msg("pprof is enabled")
	}

	log.Info().Msg(fmt.Sprintf("Accepting connections on %v", conf.Listen))

	go func() {
		errLNS := http.ListenAndServe(conf.Listen, mux)
		if errLNS != nil {
			log.Error().Msg(fmt.Sprintf("Failure starting up %v", errLNS))
			os.Exit(1)
		}
	}()

	stopSignals := make(chan os.Signal, 1)
	signal.Notify(stopSignals, syscall.SIGINT, syscall.SIGHUP, syscall.SIGTERM)
	<-stopSignals
}
