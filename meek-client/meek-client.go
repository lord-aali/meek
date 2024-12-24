// meek-client is the client transport plugin for the meek pluggable transport.
//
// Sample usage in torrc:
//
//	Bridge meek 0.0.2.0:1 url=https://forbidden.example/ front=allowed.example
//	ClientTransportPlugin meek exec ./meek-client
//
// The transport ignores the bridge address 0.0.2.0:1 and instead connects to
// the URL given by url=. When front= is given, the domain in the URL is
// replaced by the front domain for the purpose of the DNS lookup, TCP
// connection, and TLS SNI, but the HTTP Host header in the request will be the
// one in url=.
//
// Most user configuration can happen either through SOCKS args (i.e., args on a
// Bridge line) or through command line options. SOCKS args take precedence
// per-connection over command line options. For example, this configuration
// using SOCKS args:
//
//	Bridge meek 0.0.2.0:1 url=https://forbidden.example/ front=allowed.example
//	ClientTransportPlugin meek exec ./meek-client
//
// is the same as this one using command line options:
//
//	Bridge meek 0.0.2.0:1
//	ClientTransportPlugin meek exec ./meek-client --url=https://forbidden.example/ --front=allowed.example
//
// The command-line configuration interface is for compatibility with tor 0.2.4
// and older, which doesn't support parameters on Bridge lines.
//
// The --helper option prevents this program from doing any network operations
// itself. Rather, it will send all requests through a browser extension that
// makes HTTP requests.
package main

import (
	"../lib/goptlib"
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	ptMethodName = "meek"
	// A session ID is a randomly generated string that identifies a
	// long-lived session. We split a TCP stream across multiple HTTP
	// requests, and those with the same session ID belong to the same
	// stream.
	sessionIDLength = 8
	// The size of the largest chunk of data we will read from the SOCKS
	// port before forwarding it in a request, and the maximum size of a
	// body we are willing to handle in a reply.
	maxPayloadLength = 0x10000
	// We must poll the server to see if it has anything to send; there is
	// no way for the server to push data back to us until we send an HTTP
	// request. When a timer expires, we send a request even if it has an
	// empty body. The interval starts at this value and then grows.
	initPollInterval = 100 * time.Millisecond
	// Maximum polling interval.
	maxPollInterval = 5 * time.Second
	// Geometric increase in the polling interval each time we fail to read
	// data.
	pollIntervalMultiplier = 1.5
	// Try an HTTP roundtrip at most this many times.
	maxTries = 10
	// Wait this long between retries.
	retryDelay = 30 * time.Second
	// Safety limits on interaction with the HTTP helper.
	maxHelperResponseLength = 10000000
	helperReadTimeout       = 60 * time.Second
	helperWriteTimeout      = 2 * time.Second
)

// We use this RoundTripper to make all our requests when neither --helper nor
// utls is in effect. We use the defaults, except we take control of the Proxy
// setting (notably, disabling the default ProxyFromEnvironment).
var httpRoundTripper *http.Transport = http.DefaultTransport.(*http.Transport).Clone()

// We use this RoundTripper when --helper is in effect.
var helperRoundTripper = &HelperRoundTripper{
	ReadTimeout:  helperReadTimeout,
	WriteTimeout: helperWriteTimeout,
}

// Store for command line options.
var options struct {
	URL       string
	Front     string
	ProxyURL  *url.URL
	UseHelper bool
	UTLSName  string
}

// RequestInfo encapsulates all the configuration used for a request–response
// roundtrip, including variables that may come from SOCKS args or from the
// command line.
type RequestInfo struct {
	// What to put in the X-Session-ID header.
	SessionID string
	// The URL to request.
	URL *url.URL
	// The Host header to put in the HTTP request (optional and may be
	// different from the host name in URL).
	Host string
	// The RoundTripper to use to send requests. This may vary depending on
	// the value of global options like --helper.
	RoundTripper http.RoundTripper
}

// Make an http.Request from the payload data in buf and the request metadata in
// info.
func makeRequest(buf []byte, info *RequestInfo) (*http.Request, error) {
	var body io.Reader
	if len(buf) > 0 {
		// Leave body == nil when buf is empty. A nil body is an
		// explicit signal that the body is empty. An empty
		// *bytes.Reader or the magic value http.NoBody are supposed to
		// be equivalent ways to signal an empty body, but in Go 1.8 the
		// HTTP/2 code only understands nil. Not leaving body == nil
		// causes the Content-Length header to be omitted from HTTP/2
		// requests, which in some cases can cause the server to return
		// a 411 "Length Required" error. See
		// https://bugs.torproject.org/22865.
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequest("POST", info.URL.String(), body)
	if err != nil {
		return nil, err
	}
	// Prevent Content-Type sniffing by net/http and middleboxes.
	req.Header.Set("Content-Type", "application/octet-stream")
	if info.Host != "" {
		req.Host = info.Host
	}
	req.Header.Set("X-Session-Id", info.SessionID)
	return req, nil
}

// Do a roundtrip, trying at most limit times if there is an HTTP status other
// than 200. In case all tries result in error, returns the last error seen.
//
// Retrying the request immediately is a bit bogus, because we don't know if the
// remote server received our bytes or not, so we may be sending duplicates,
// which will cause the connection to die. The alternative, though, is to just
// kill the connection immediately. A better solution would be a system of
// acknowledgements so we know what to resend after an error.
func roundTripRetries(rt http.RoundTripper, req *http.Request, limit int) (*http.Response, error) {
	var resp *http.Response
	var err error
again:
	limit--
	resp, err = rt.RoundTrip(req)
	// Retry only if the HTTP roundtrip completed without error, but
	// returned a status other than 200. Other kinds of errors and success
	// with 200 always return immediately.
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("status code was %d, not %d", resp.StatusCode, http.StatusOK)
		if limit > 0 {
			log.Printf("%s; trying again after %.f seconds (%d)", err, retryDelay.Seconds(), limit)
			time.Sleep(retryDelay)
			goto again
		}
	}
	return resp, err
}

// Send the data in buf to the remote URL, wait for a reply, and feed the reply
// body back into conn.
func sendRecv(buf []byte, conn net.Conn, info *RequestInfo) (int64, error) {
	req, err := makeRequest(buf, info)
	if err != nil {
		return 0, err
	}
	resp, err := roundTripRetries(info.RoundTripper, req, maxTries)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return io.Copy(conn, io.LimitReader(resp.Body, maxPayloadLength))
}

// Repeatedly read from conn, issue HTTP requests, and write the responses back
// to conn.
func copyLoop(conn net.Conn, info *RequestInfo) error {
	var interval time.Duration

	ch := make(chan []byte)

	// Read from the Conn and send byte slices on the channel.
	go func() {
		var buf [maxPayloadLength]byte
		r := bufio.NewReader(conn)
		for {
			n, err := r.Read(buf[:])
			b := make([]byte, n)
			copy(b, buf[:n])
			// log.Printf("read from local: %q", b)
			ch <- b
			if err != nil {
				if err != io.EOF {
					log.Printf("error reading from local: %s", err)
				}
				break
			}
		}
		close(ch)
	}()

	interval = initPollInterval
loop:
	for {
		var buf []byte
		var ok bool

		// log.Printf("waiting up to %.2f s", interval.Seconds())
		// start := time.Now()
		select {
		case buf, ok = <-ch:
			if !ok {
				break loop
			}
			// log.Printf("read %d bytes from local after %.2f s", len(buf), time.Since(start).Seconds())
		case <-time.After(interval):
			// log.Printf("read nothing from local after %.2f s", time.Since(start).Seconds())
			buf = nil
		}

		nw, err := sendRecv(buf, conn, info)
		if err != nil {
			return err
		}
		/*
			if nw > 0 {
				log.Printf("got %d bytes from remote", nw)
			} else {
				log.Printf("got nothing from remote")
			}
		*/

		if nw > 0 || len(buf) > 0 {
			// If we sent or received anything, poll again
			// immediately.
			interval = 0
		} else if interval == 0 {
			// The first time we don't send or receive anything,
			// wait a while.
			interval = initPollInterval
		} else {
			// After that, wait a little longer.
			interval = time.Duration(float64(interval) * pollIntervalMultiplier)
		}
		if interval > maxPollInterval {
			interval = maxPollInterval
		}
	}

	return nil
}

func genSessionID() string {
	buf := make([]byte, sessionIDLength)
	_, err := rand.Read(buf)
	if err != nil {
		panic(err.Error())
	}
	return strings.TrimRight(base64.StdEncoding.EncodeToString(buf), "=")
}

// Callback for new SOCKS requests.
func handleSOCKS(conn *pt.SocksConn) error {
	defer conn.Close()
	err := conn.Grant(&net.TCPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return err
	}

	var info RequestInfo
	info.SessionID = genSessionID()

	// First check url= SOCKS arg, then --url option.
	urlArg, ok := conn.Req.Args.Get("url")
	if ok {
	} else if options.URL != "" {
		urlArg = options.URL
	} else {
		return fmt.Errorf("no URL for SOCKS request")
	}
	info.URL, err = url.Parse(urlArg)
	if err != nil {
		return err
	}

	// First check front= SOCKS arg, then --front option.
	front, ok := conn.Req.Args.Get("front")
	if ok {
	} else if options.Front != "" {
		front = options.Front
		ok = true
	}
	if ok {
		info.Host = info.URL.Host
		info.URL.Host = front
	}

	// First check utls= SOCKS arg, then --utls option.
	utlsName, utlsOK := conn.Req.Args.Get("utls")
	if utlsOK {
	} else if options.UTLSName != "" {
		utlsName = options.UTLSName
		utlsOK = true
	}

	// First we check --helper: if it was specified, then we always use the
	// helper, and utls is disallowed. Otherwise, we use utls if requested;
	// or else fall back to native net/http.
	if options.UseHelper {
		if utlsOK {
			return fmt.Errorf("cannot use utls with --helper")
		}
		info.RoundTripper = helperRoundTripper
	} else if utlsOK {
		info.RoundTripper, err = NewUTLSRoundTripper(utlsName, nil, options.ProxyURL)
		if err != nil {
			return err
		}
	} else {
		info.RoundTripper = httpRoundTripper
	}

	return copyLoop(conn, &info)
}

func acceptSOCKS(ln *pt.SocksListener) error {
	defer ln.Close()
	for {
		conn, err := ln.AcceptSocks()
		if err != nil {
			log.Printf("error in AcceptSocks: %s", err)
			if e, ok := err.(net.Error); ok && e.Temporary() {
				continue
			}
			return err
		}
		go func() {
			err := handleSOCKS(conn)
			if err != nil {
				log.Printf("error in handling request: %s", err)
			}
		}()
	}
	return nil
}

// Return an error if this proxy URL doesn't work with the rest of the
// configuration.
func checkProxyURL(u *url.URL) error {
	if !options.UseHelper {
		// Without the helper, we use net/http's built-in proxy support,
		// which allows "http", "https", and "socks5".
		// socks5 requires go1.9: https://golang.org/doc/go1.9#net/http
		// https requires go1.10: https://golang.org/doc/go1.10#net/http
		// If using an older version of Go, the proxy won't be bypassed;
		// you'll just get an error at connection time rather than
		// TOR_PT_PROXY time.
		switch u.Scheme {
		case "http", "https", "socks5":
		default:
			return fmt.Errorf("don't understand proxy URL scheme %q", u.Scheme)
		}
	} else {
		// With the helper we can use HTTP and SOCKS (because it is the
		// browser that does the proxying, not us).
		// For the HTTP proxy with the Firefox helper: versions of
		// Firefox before 32, and Tor Browser before 3.6.2, leak the
		// covert Host header in HTTP proxy CONNECT requests. Using an
		// HTTP proxy cannot provide effective obfuscation without such
		// a patched Firefox.
		// https://bugs.torproject.org/12146
		// https://gitweb.torproject.org/tor-browser.git/commit/?id=e08b91c78d919f66dd5161561ca1ad7bcec9a563
		// https://bugzilla.mozilla.org/show_bug.cgi?id=1017769
		// https://hg.mozilla.org/mozilla-central/rev/a1f6458800d4
		switch u.Scheme {
		case "http", "socks5", "socks4a":
		default:
			return fmt.Errorf("don't understand proxy URL scheme %q", u.Scheme)
		}
		if u.User != nil {
			return fmt.Errorf("a proxy URL with a username or password can't be used with --helper")
		}
	}
	return nil
}

func main() {
	var helperAddr string
	var logFilename string
	var proxy string
	var socksPort string
	var err error

	os.Setenv("TOR_PT_MANAGED_TRANSPORT_VER", "1")
	os.Setenv("TOR_PT_CLIENT_TRANSPORTS", "meek")

	flag.StringVar(&options.Front, "front", "", "front domain name if no front= SOCKS arg")
	flag.StringVar(&helperAddr, "helper", "", "address of HTTP helper (browser extension)")
	flag.StringVar(&logFilename, "log", "", "name of log file")
	flag.StringVar(&proxy, "proxy", "", "proxy URL")
	flag.StringVar(&socksPort, "port", "4455", "listening socks port")
	flag.StringVar(&options.URL, "url", "", "URL to request if no url= SOCKS arg")
	flag.StringVar(&options.UTLSName, "utls", "", "uTLS Client Hello ID")
	flag.Parse()

	ptInfo, err := pt.ClientSetup(nil)
	if err != nil {
		log.Fatalf("error in ClientSetup: %s", err)
	}

	log.SetFlags(log.LstdFlags | log.LUTC)
	if logFilename != "" {
		f, err := os.OpenFile(logFilename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			// If we fail to open the log, emit a message that will
			// appear in tor's log.
			pt.CmethodError(ptMethodName, fmt.Sprintf("error opening log file: %s", err))
			log.Fatalf("error opening log file: %s", err)
		}
		defer f.Close()
		log.SetOutput(f)
	}

	if helperAddr != "" {
		options.UseHelper = true
		helperRoundTripper.HelperAddr, err = net.ResolveTCPAddr("tcp", helperAddr)
		if err != nil {
			log.Fatalf("can't resolve helper address: %s", err)
		}
		log.Printf("using helper on %s", helperRoundTripper.HelperAddr)
	}

	if proxy != "" {
		options.ProxyURL, err = url.Parse(proxy)
		if err != nil {
			log.Fatalf("can't parse proxy URL: %s", err)
		}
	}

	// Disable the default ProxyFromEnvironment setting.
	// httpRoundTripper.Proxy is overridden below if options.ProxyURL is
	// set.
	httpRoundTripper.Proxy = nil

	// Command-line proxy overrides managed configuration.
	if options.ProxyURL == nil {
		options.ProxyURL = ptInfo.ProxyURL
	}
	// Check whether we support this kind of proxy.
	if options.ProxyURL != nil {
		err = checkProxyURL(options.ProxyURL)
		if err != nil {
			pt.ProxyError(err.Error())
			log.Fatal(fmt.Sprintf("proxy error: %s", err))
		}
		log.Printf("using proxy %s", options.ProxyURL.String())
		httpRoundTripper.Proxy = http.ProxyURL(options.ProxyURL)
		if options.UseHelper {
			err = helperRoundTripper.SetProxy(options.ProxyURL)
			if err != nil {
				pt.ProxyError(err.Error())
				log.Fatal(fmt.Sprintf("proxy error: %s", err))
			}
		}
		if ptInfo.ProxyURL != nil {
			pt.ProxyDone()
		}
	}

	listeners := make([]net.Listener, 0)
	for _, methodName := range ptInfo.MethodNames {
		switch methodName {
		case ptMethodName:
			ln, err := pt.ListenSocks("tcp", "127.0.0.1:"+socksPort)
			if err != nil {
				pt.CmethodError(methodName, err.Error())
				break
			}
			go acceptSOCKS(ln)
			pt.Cmethod(methodName, ln.Version(), ln.Addr())
			log.Printf("listening on %s", ln.Addr())
			listeners = append(listeners, ln)
		default:
			pt.CmethodError(methodName, "no such method")
		}
	}
	pt.CmethodsDone()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM)

	if os.Getenv("TOR_PT_EXIT_ON_STDIN_CLOSE") == "1" {
		// This environment variable means we should treat EOF on stdin
		// just like SIGTERM: https://bugs.torproject.org/15435.
		go func() {
			io.Copy(ioutil.Discard, os.Stdin)
			log.Printf("synthesizing SIGTERM because of stdin close")
			sigChan <- syscall.SIGTERM
		}()
	}

	// Wait for a signal.
	sig := <-sigChan
	log.Printf("got signal %s", sig)

	for _, ln := range listeners {
		ln.Close()
	}

	log.Printf("done")
}
