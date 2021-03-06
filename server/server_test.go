// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// End-to-end serving tests

package server_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	standardhttptest "net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cloudfoundry/gorouter/server"
	"github.com/cloudfoundry/gorouter/server/httptest"
)

type dummyAddr string
type oneConnListener struct {
	conn net.Conn
}

func (l *oneConnListener) Accept() (c net.Conn, err error) {
	c = l.conn
	if c == nil {
		err = io.EOF
		return
	}
	err = nil
	l.conn = nil
	return
}

func (l *oneConnListener) Close() error {
	return nil
}

func (l *oneConnListener) Addr() net.Addr {
	return dummyAddr("test-address")
}

func (a dummyAddr) Network() string {
	return string(a)
}

func (a dummyAddr) String() string {
	return string(a)
}

type testConn struct {
	readBuf  bytes.Buffer
	writeBuf bytes.Buffer
}

func (c *testConn) Read(b []byte) (int, error) {
	return c.readBuf.Read(b)
}

func (c *testConn) Write(b []byte) (int, error) {
	return c.writeBuf.Write(b)
}

func (c *testConn) Close() error {
	return nil
}

func (c *testConn) LocalAddr() net.Addr {
	return dummyAddr("local-addr")
}

func (c *testConn) RemoteAddr() net.Addr {
	return dummyAddr("remote-addr")
}

func (c *testConn) SetDeadline(t time.Time) error {
	return nil
}

func (c *testConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *testConn) SetWriteDeadline(t time.Time) error {
	return nil
}

type timeoutWriter struct {
	w http.ResponseWriter

	mu          sync.Mutex
	timedOut    bool
	wroteHeader bool
}

func (tw *timeoutWriter) Header() http.Header {
	return tw.w.Header()
}

func (tw *timeoutWriter) Write(p []byte) (int, error) {
	tw.mu.Lock()
	timedOut := tw.timedOut
	tw.mu.Unlock()
	if timedOut {
		return 0, http.ErrHandlerTimeout
	}
	return tw.w.Write(p)
}

func (tw *timeoutWriter) WriteHeader(code int) {
	tw.mu.Lock()
	if tw.timedOut || tw.wroteHeader {
		tw.mu.Unlock()
		return
	}
	tw.wroteHeader = true
	tw.mu.Unlock()
	tw.w.WriteHeader(code)
}

type timeoutHandler struct {
	handler http.Handler
	timeout func() <-chan time.Time // returns channel producing a timeout
	body    string
}

func (h *timeoutHandler) errorBody() string {
	if h.body != "" {
		return h.body
	}
	return "<html><head><title>Timeout</title></head><body><h1>Timeout</h1></body></html>"
}

func (h *timeoutHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	done := make(chan bool)
	tw := &timeoutWriter{w: w}
	go func() {
		h.handler.ServeHTTP(tw, r)
		done <- true
	}()
	select {
	case <-done:
		return
	case <-h.timeout():
		tw.mu.Lock()
		defer tw.mu.Unlock()
		if !tw.wroteHeader {
			tw.w.WriteHeader(http.StatusServiceUnavailable)
			tw.w.Write([]byte(h.errorBody()))
		}
		tw.timedOut = true
	}
}

func NewTestTimeoutHandler(handler http.Handler, ch <-chan time.Time) http.Handler {
	f := func() <-chan time.Time {
		return ch
	}
	return &timeoutHandler{handler, f, ""}
}

func TestConsumingBodyOnNextConn(t *testing.T) {
	conn := new(testConn)
	for i := 0; i < 2; i++ {
		conn.readBuf.Write([]byte(
			"POST / HTTP/1.1\r\n" +
				"Host: test\r\n" +
				"Content-Length: 11\r\n" +
				"\r\n" +
				"foo=1&bar=1"))
	}

	reqNum := 0
	ch := make(chan *http.Request)
	servech := make(chan error)
	listener := &oneConnListener{conn}
	handler := func(res http.ResponseWriter, req *http.Request) {
		reqNum++
		ch <- req
	}

	go func() {
		servech <- http.Serve(listener, http.HandlerFunc(handler))
	}()

	var req *http.Request
	req = <-ch
	if req == nil {
		t.Fatal("Got nil first request.")
	}
	if req.Method != "POST" {
		t.Errorf("For request #1's method, got %q; expected %q",
			req.Method, "POST")
	}

	req = <-ch
	if req == nil {
		t.Fatal("Got nil first request.")
	}
	if req.Method != "POST" {
		t.Errorf("For request #2's method, got %q; expected %q",
			req.Method, "POST")
	}

	if serveerr := <-servech; serveerr != io.EOF {
		t.Errorf("Serve returned %q; expected EOF", serveerr)
	}
}

type stringHandler string

func (s stringHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Result", string(s))
}

var handlers = []struct {
	pattern string
	msg     string
}{
	{"/", "Default"},
	{"/someDir/", "someDir"},
	{"someHost.com/someDir/", "someHost.com/someDir"},
}

var vtests = []struct {
	url      string
	expected string
}{
	{"http://localhost/someDir/apage", "someDir"},
	{"http://localhost/otherDir/apage", "Default"},
	{"http://someHost.com/someDir/apage", "someHost.com/someDir"},
	{"http://otherHost.com/someDir/apage", "someDir"},
	{"http://otherHost.com/aDir/apage", "Default"},
}

func TestHostHandlers(t *testing.T) {
	for _, h := range handlers {
		http.Handle(h.pattern, stringHandler(h.msg))
	}
	ts := httptest.NewServer(nil)
	defer ts.Close()

	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cc := httputil.NewClientConn(conn, nil)
	for _, vt := range vtests {
		var r *http.Response
		var req http.Request
		if req.URL, err = url.Parse(vt.url); err != nil {
			t.Errorf("cannot parse url: %v", err)
			continue
		}
		if err := cc.Write(&req); err != nil {
			t.Errorf("writing request: %v", err)
			continue
		}
		r, err := cc.Read(&req)
		if err != nil {
			t.Errorf("reading response: %v", err)
			continue
		}
		s := r.Header.Get("Result")
		if s != vt.expected {
			t.Errorf("Get(%q) = %q, want %q", vt.url, s, vt.expected)
		}
	}
}

// Tests for http://code.google.com/p/go/issues/detail?id=900
func TestMuxRedirectLeadingSlashes(t *testing.T) {
	paths := []string{"//foo.txt", "///foo.txt", "/../../foo.txt"}
	for _, path := range paths {
		req, err := http.ReadRequest(bufio.NewReader(bytes.NewBufferString("GET " + path + " HTTP/1.1\r\nHost: test\r\n\r\n")))
		if err != nil {
			t.Errorf("%s", err)
		}
		mux := http.NewServeMux()
		resp := standardhttptest.NewRecorder()

		mux.ServeHTTP(resp, req)

		if loc, expected := resp.Header().Get("Location"), "/foo.txt"; loc != expected {
			t.Errorf("Expected Location header set to %q; got %q", expected, loc)
			return
		}

		if code, expected := resp.Code, http.StatusMovedPermanently; code != expected {
			t.Errorf("Expected response code of StatusMovedPermanently; got %d", code)
			return
		}
	}
}

func TestServerTimeouts(t *testing.T) {
	// TODO(bradfitz): convert this to use httptest.Server
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	addr, _ := l.Addr().(*net.TCPAddr)

	reqNum := 0
	handler := http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		reqNum++
		fmt.Fprintf(res, "req=%d", reqNum)
	})

	server := &server.Server{Handler: handler, ReadTimeout: 250 * time.Millisecond, WriteTimeout: 250 * time.Millisecond}
	go server.Serve(l)

	url := fmt.Sprintf("http://%s/", addr)

	// Hit the HTTP server successfully.
	tr := &http.Transport{DisableKeepAlives: true} // they interfere with this test
	c := &http.Client{Transport: tr}
	r, err := c.Get(url)
	if err != nil {
		t.Fatalf("http Get #1: %v", err)
	}
	got, _ := ioutil.ReadAll(r.Body)
	expected := "req=1"
	if string(got) != expected {
		t.Errorf("Unexpected response for request #1; got %q; expected %q",
			string(got), expected)
	}

	// Slow client that should timeout.
	t1 := time.Now()
	conn, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	latency := time.Now().Sub(t1)
	if n != 0 || err != io.EOF {
		t.Errorf("Read = %v, %v, wanted %v, %v", n, err, 0, io.EOF)
	}
	if latency < 200*time.Millisecond /* fudge from 250 ms above */ {
		t.Errorf("got EOF after %s, want >= %s", latency, 200*time.Millisecond)
	}

	// Hit the HTTP server successfully again, verifying that the
	// previous slow connection didn't run our handler.  (that we
	// get "req=2", not "req=3")
	r, err = http.Get(url)
	if err != nil {
		t.Fatalf("http Get #2: %v", err)
	}
	got, _ = ioutil.ReadAll(r.Body)
	expected = "req=2"
	if string(got) != expected {
		t.Errorf("Get #2 got %q, want %q", string(got), expected)
	}

	l.Close()
}

// TestIdentityResponse verifies that a handler can unset
func TestIdentityResponse(t *testing.T) {
	handler := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		rw.Header().Set("Content-Length", "3")
		rw.Header().Set("Transfer-Encoding", req.FormValue("te"))
		switch {
		case req.FormValue("overwrite") == "1":
			_, err := rw.Write([]byte("foo TOO LONG"))
			if err != http.ErrContentLength {
				t.Errorf("expected ErrContentLength; got %v", err)
			}
		case req.FormValue("underwrite") == "1":
			rw.Header().Set("Content-Length", "500")
			rw.Write([]byte("too short"))
		default:
			rw.Write([]byte("foo"))
		}
	})

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// Note: this relies on the assumption (which is true) that
	// Get sends HTTP/1.1 or greater requests.  Otherwise the
	// server wouldn't have the choice to send back chunked
	// responses.
	for _, te := range []string{"", "identity"} {
		url := ts.URL + "/?te=" + te
		res, err := http.Get(url)
		if err != nil {
			t.Fatalf("error with Get of %s: %v", url, err)
		}
		if cl, expected := res.ContentLength, int64(3); cl != expected {
			t.Errorf("for %s expected res.ContentLength of %d; got %d", url, expected, cl)
		}
		if cl, expected := res.Header.Get("Content-Length"), "3"; cl != expected {
			t.Errorf("for %s expected Content-Length header of %q; got %q", url, expected, cl)
		}
		if tl, expected := len(res.TransferEncoding), 0; tl != expected {
			t.Errorf("for %s expected len(res.TransferEncoding) of %d; got %d (%v)",
				url, expected, tl, res.TransferEncoding)
		}
		res.Body.Close()
	}

	// Verify that ErrContentLength is returned
	url := ts.URL + "/?overwrite=1"
	_, err := http.Get(url)
	if err != nil {
		t.Fatalf("error with Get of %s: %v", url, err)
	}
	// Verify that the connection is closed when the declared Content-Length
	// is larger than what the handler wrote.
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("error dialing: %v", err)
	}
	_, err = conn.Write([]byte("GET /?underwrite=1 HTTP/1.1\r\nHost: foo\r\n\r\n"))
	if err != nil {
		t.Fatalf("error writing: %v", err)
	}

	// The ReadAll will hang for a failing test, so use a Timer to
	// fail explicitly.
	goTimeout(t, 2*time.Second, func() {
		got, _ := ioutil.ReadAll(conn)
		expectedSuffix := "\r\n\r\ntoo short"
		if !strings.HasSuffix(string(got), expectedSuffix) {
			t.Errorf("Expected output to end with %q; got response body %q",
				expectedSuffix, string(got))
		}
	})
}

func testTcpConnectionCloses(t *testing.T, req string, h http.Handler) {
	s := httptest.NewServer(h)
	defer s.Close()

	conn, err := net.Dial("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal("dial error:", err)
	}
	defer conn.Close()

	_, err = fmt.Fprint(conn, req)
	if err != nil {
		t.Fatal("print error:", err)
	}

	r := bufio.NewReader(conn)
	res, err := http.ReadResponse(r, &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal("ReadResponse error:", err)
	}

	didReadAll := make(chan bool, 1)
	go func() {
		select {
		case <-time.After(5 * time.Second):
			t.Error("body not closed after 5s")
			return
		case <-didReadAll:
		}
	}()

	_, err = ioutil.ReadAll(r)
	if err != nil {
		t.Fatal("read error:", err)
	}
	didReadAll <- true

	if !res.Close {
		t.Errorf("Response.Close = false; want true")
	}
}

// TestServeHTTP10Close verifies that HTTP/1.0 requests won't be kept alive.
func TestServeHTTP10Close(t *testing.T) {
	testTcpConnectionCloses(t, "GET / HTTP/1.0\r\n\r\n", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "testdata/file")
	}))
}

// TestHandlersCanSetConnectionClose verifies that handlers can force a connection to close,
// even for HTTP/1.1 requests.
func TestHandlersCanSetConnectionClose11(t *testing.T) {
	testTcpConnectionCloses(t, "GET / HTTP/1.1\r\n\r\n", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
	}))
}

func TestHandlersCanSetConnectionClose10(t *testing.T) {
	testTcpConnectionCloses(t, "GET / HTTP/1.0\r\nConnection: keep-alive\r\n\r\n", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
	}))
}

func TestSetsRemoteAddr(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s", r.RemoteAddr)
	}))
	defer ts.Close()

	res, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	ip := string(body)
	if !strings.HasPrefix(ip, "127.0.0.1:") && !strings.HasPrefix(ip, "[::1]:") {
		t.Fatalf("Expected local addr; got %q", ip)
	}
}

func TestChunkedResponseHeaders(t *testing.T) {
	log.SetOutput(ioutil.Discard) // is noisy otherwise
	defer log.SetOutput(os.Stderr)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "intentional gibberish") // we check that this is deleted
		fmt.Fprintf(w, "I am a chunked response.")
	}))
	defer ts.Close()

	res, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("Get error: %v", err)
	}
	if g, e := res.ContentLength, int64(-1); g != e {
		t.Errorf("expected ContentLength of %d; got %d", e, g)
	}
	if g, e := res.TransferEncoding, []string{"chunked"}; !reflect.DeepEqual(g, e) {
		t.Errorf("expected TransferEncoding of %v; got %v", e, g)
	}
	if _, haveCL := res.Header["Content-Length"]; haveCL {
		t.Errorf("Unexpected Content-Length")
	}
}

// Test304Responses verifies that 304s don't declare that they're
// chunking in their response headers and aren't allowed to produce
// output.
func Test304Responses(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
		_, err := w.Write([]byte("illegal body"))
		if err != http.ErrBodyNotAllowed {
			t.Errorf("on Write, expected ErrBodyNotAllowed, got %v", err)
		}
	}))
	defer ts.Close()
	res, err := http.Get(ts.URL)
	if err != nil {
		t.Error(err)
	}
	if len(res.TransferEncoding) > 0 {
		t.Errorf("expected no TransferEncoding; got %v", res.TransferEncoding)
	}
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		t.Error(err)
	}
	if len(body) > 0 {
		t.Errorf("got unexpected body %q", string(body))
	}
}

// TestHeadResponses verifies that responses to HEAD requests don't
// declare that they're chunking in their response headers, aren't
// allowed to produce output, and don't set a Content-Type since
// the real type of the body data cannot be inferred.
func TestHeadResponses(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte("Ignored body"))
		if err != http.ErrBodyNotAllowed {
			t.Errorf("on Write, expected ErrBodyNotAllowed, got %v", err)
		}

		// Also exercise the ReaderFrom path
		_, err = io.Copy(w, strings.NewReader("Ignored body"))
		if err != http.ErrBodyNotAllowed {
			t.Errorf("on Copy, expected ErrBodyNotAllowed, got %v", err)
		}
	}))
	defer ts.Close()
	res, err := http.Head(ts.URL)
	if err != nil {
		t.Error(err)
	}
	if len(res.TransferEncoding) > 0 {
		t.Errorf("expected no TransferEncoding; got %v", res.TransferEncoding)
	}
	ct := res.Header.Get("Content-Type")
	if ct != "" {
		t.Errorf("expected no Content-Type; got %s", ct)
	}
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		t.Error(err)
	}
	if len(body) > 0 {
		t.Errorf("got unexpected body %q", string(body))
	}
}

func TestTLSHandshakeTimeout(t *testing.T) {
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ts.Config.ReadTimeout = 250 * time.Millisecond
	ts.StartTLS()
	defer ts.Close()
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	goTimeout(t, 10*time.Second, func() {
		var buf [1]byte
		n, err := conn.Read(buf[:])
		if err == nil || n != 0 {
			t.Errorf("Read = %d, %v; want an error and no bytes", n, err)
		}
	})
}

type serverExpectTest struct {
	contentLength    int    // of request body
	expectation      string // e.g. "100-continue"
	readBody         bool   // whether handler should read the body (if false, sends StatusUnauthorized)
	expectedResponse string // expected substring in first line of http response
}

var serverExpectTests = []serverExpectTest{
	// Normal 100-continues, case-insensitive.
	{100, "100-continue", true, "100 Continue"},
	{100, "100-cOntInUE", true, "100 Continue"},

	// No 100-continue.
	{100, "", true, "200 OK"},

	// 100-continue but requesting client to deny us,
	// so it never reads the body.
	{100, "100-continue", false, "401 Unauthorized"},
	// Likewise without 100-continue:
	{100, "", false, "401 Unauthorized"},

	// Non-standard expectations are failures
	{0, "a-pony", false, "417 Expectation Failed"},

	// Expect-100 requested but no body
	{0, "100-continue", true, "400 Bad Request"},
}

// Tests that the server responds to the "Expect" request header
// correctly.
func TestServerExpect(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Note using r.FormValue("readbody") because for POST
		// requests that would read from r.Body, which we only
		// conditionally want to do.
		if strings.Contains(r.URL.RawQuery, "readbody=true") {
			ioutil.ReadAll(r.Body)
			w.Write([]byte("Hi"))
		} else {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer ts.Close()

	runTest := func(test serverExpectTest) {
		conn, err := net.Dial("tcp", ts.Listener.Addr().String())
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer conn.Close()
		sendf := func(format string, args ...interface{}) {
			_, err := fmt.Fprintf(conn, format, args...)
			if err != nil {
				t.Fatalf("On test %#v, error writing %q: %v", test, format, err)
			}
		}
		go func() {
			sendf("POST /?readbody=%v HTTP/1.1\r\n"+
				"Connection: close\r\n"+
				"Content-Length: %d\r\n"+
				"Expect: %s\r\nHost: foo\r\n\r\n",
				test.readBody, test.contentLength, test.expectation)
			if test.contentLength > 0 && strings.ToLower(test.expectation) != "100-continue" {
				body := strings.Repeat("A", test.contentLength)
				sendf(body)
			}
		}()
		bufr := bufio.NewReader(conn)
		line, err := bufr.ReadString('\n')
		if err != nil {
			t.Fatalf("ReadString: %v", err)
		}
		if !strings.Contains(line, test.expectedResponse) {
			t.Errorf("for test %#v got first line=%q", test, line)
		}
	}

	for _, test := range serverExpectTests {
		runTest(test)
	}
}

func TestTimeoutHandler(t *testing.T) {
	sendHi := make(chan bool, 1)
	writeErrors := make(chan error, 1)
	sayHi := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-sendHi
		_, werr := w.Write([]byte("hi"))
		writeErrors <- werr
	})
	timeout := make(chan time.Time, 1) // write to this to force timeouts
	ts := httptest.NewServer(NewTestTimeoutHandler(sayHi, timeout))
	defer ts.Close()

	// Succeed without timing out:
	sendHi <- true
	res, err := http.Get(ts.URL)
	if err != nil {
		t.Error(err)
	}
	if g, e := res.StatusCode, http.StatusOK; g != e {
		t.Errorf("got res.StatusCode %d; expected %d", g, e)
	}
	body, _ := ioutil.ReadAll(res.Body)
	if g, e := string(body), "hi"; g != e {
		t.Errorf("got body %q; expected %q", g, e)
	}
	if g := <-writeErrors; g != nil {
		t.Errorf("got unexpected Write error on first request: %v", g)
	}

	// Times out:
	timeout <- time.Time{}
	res, err = http.Get(ts.URL)
	if err != nil {
		t.Error(err)
	}
	if g, e := res.StatusCode, http.StatusServiceUnavailable; g != e {
		t.Errorf("got res.StatusCode %d; expected %d", g, e)
	}
	body, _ = ioutil.ReadAll(res.Body)
	if !strings.Contains(string(body), "<title>Timeout</title>") {
		t.Errorf("expected timeout body; got %q", string(body))
	}

	// Now make the previously-timed out handler speak again,
	// which verifies the panic is handled:
	sendHi <- true
	if g, e := <-writeErrors, http.ErrHandlerTimeout; g != e {
		t.Errorf("expected Write error of %v; got %v", e, g)
	}
}

// Verifies we don't path.Clean() on the wrong parts in redirects.
func TestRedirectMunging(t *testing.T) {
	req, _ := http.NewRequest("GET", "http://example.com/", nil)

	resp := standardhttptest.NewRecorder()
	http.Redirect(resp, req, "/foo?next=http://bar.com/", 302)
	if g, e := resp.Header().Get("Location"), "/foo?next=http://bar.com/"; g != e {
		t.Errorf("Location header was %q; want %q", g, e)
	}

	resp = standardhttptest.NewRecorder()
	http.Redirect(resp, req, "http://localhost:8080/_ah/login?continue=http://localhost:8080/", 302)
	if g, e := resp.Header().Get("Location"), "http://localhost:8080/_ah/login?continue=http://localhost:8080/"; g != e {
		t.Errorf("Location header was %q; want %q", g, e)
	}
}

// TestZeroLengthPostAndResponse exercises an optimization done by the Transport:
// when there is no body (either because the method doesn't permit a body, or an
// explicit Content-Length of zero is present), then the transport can re-use the
// connection immediately. But when it re-uses the connection, it typically closes
// the previous request's body, which is not optimal for zero-lengthed bodies,
// as the client would then see http.ErrBodyReadAfterClose and not 0, io.EOF.
func TestZeroLengthPostAndResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		all, err := ioutil.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("handler ReadAll: %v", err)
		}
		if len(all) != 0 {
			t.Errorf("handler got %d bytes; expected 0", len(all))
		}
		rw.Header().Set("Content-Length", "0")
	}))
	defer ts.Close()

	req, err := http.NewRequest("POST", ts.URL, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = 0

	var resp [5]*http.Response
	for i := range resp {
		resp[i], err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("client post #%d: %v", i, err)
		}
	}

	for i := range resp {
		all, err := ioutil.ReadAll(resp[i].Body)
		if err != nil {
			t.Fatalf("req #%d: client ReadAll: %v", i, err)
		}
		if len(all) != 0 {
			t.Errorf("req #%d: client got %d bytes; expected 0", i, len(all))
		}
	}
}

func TestHandlerPanic(t *testing.T) {
	testHandlerPanic(t, false)
}

func TestHandlerPanicWithHijack(t *testing.T) {
	testHandlerPanic(t, true)
}

func testHandlerPanic(t *testing.T, withHijack bool) {
	// Unlike the other tests that set the log output to ioutil.Discard
	// to quiet the output, this test uses a pipe.  The pipe serves three
	// purposes:
	//
	//   1) The log.Print from the http server (generated by the caught
	//      panic) will go to the pipe instead of stderr, making the
	//      output quiet.
	//
	//   2) We read from the pipe to verify that the handler
	//      actually caught the panic and logged something.
	//
	//   3) The blocking Read call prevents this TestHandlerPanic
	//      function from exiting before the HTTP server handler
	//      finishes crashing. If this text function exited too
	//      early (and its defer log.SetOutput(os.Stderr) ran),
	//      then the crash output could spill into the next test.
	pr, pw := io.Pipe()
	log.SetOutput(pw)
	defer log.SetOutput(os.Stderr)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if withHijack {
			rwc, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Logf("unexpected error: %v", err)
			}
			defer rwc.Close()
		}
		panic("intentional death for testing")
	}))
	defer ts.Close()

	// Do a blocking read on the log output pipe so its logging
	// doesn't bleed into the next test.  But wait only 5 seconds
	// for it.
	done := make(chan bool, 1)
	go func() {
		buf := make([]byte, 4<<10)
		_, err := pr.Read(buf)
		pr.Close()
		if err != nil {
			t.Fatal(err)
		}
		done <- true
	}()

	_, err := http.Get(ts.URL)
	if err == nil {
		t.Logf("expected an error")
	}

	select {
	case <-done:
		return
	case <-time.After(5 * time.Second):
		t.Fatal("expected server handler to log an error")
	}
}

func TestNoDate(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Date"] = nil
	}))
	defer ts.Close()
	res, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, present := res.Header["Date"]
	if present {
		t.Fatalf("Expected no Date header; got %v", res.Header["Date"])
	}
}

func TestStripPrefix(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Path", r.URL.Path)
	})
	ts := httptest.NewServer(http.StripPrefix("/foo", h))
	defer ts.Close()

	res, err := http.Get(ts.URL + "/foo/bar")
	if err != nil {
		t.Fatal(err)
	}
	if g, e := res.Header.Get("X-Path"), "/bar"; g != e {
		t.Errorf("test 1: got %s, want %s", g, e)
	}

	res, err = http.Get(ts.URL + "/bar")
	if err != nil {
		t.Fatal(err)
	}
	if g, e := res.StatusCode, 404; g != e {
		t.Errorf("test 2: got status %v, want %v", g, e)
	}
}

func TestRequestLimit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("didn't expect to get request in Handler")
	}))
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL, nil)
	var bytesPerHeader = len("header12345: val12345\r\n")
	for i := 0; i < ((http.DefaultMaxHeaderBytes+4096)/bytesPerHeader)+1; i++ {
		req.Header.Set(fmt.Sprintf("header%05d", i), fmt.Sprintf("val%05d", i))
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		// Some HTTP clients may fail on this undefined behavior (server replying and
		// closing the connection while the request is still being written), but
		// we do support it (at least currently), so we expect a response below.
		t.Fatalf("Do: %v", err)
	}
	if res.StatusCode != 413 {
		t.Fatalf("expected 413 response status; got: %d %s", res.StatusCode, res.Status)
	}
}

type neverEnding byte

func (b neverEnding) Read(p []byte) (n int, err error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

type countReader struct {
	r io.Reader
	n *int64
}

func (cr countReader) Read(p []byte) (n int, err error) {
	n, err = cr.r.Read(p)
	*cr.n += int64(n)
	return
}

func TestRequestBodyLimit(t *testing.T) {
	const limit = 1 << 20
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		n, err := io.Copy(ioutil.Discard, r.Body)
		if err == nil {
			t.Errorf("expected error from io.Copy")
		}
		if n != limit {
			t.Errorf("io.Copy = %d, want %d", n, limit)
		}
	}))
	defer ts.Close()

	nWritten := int64(0)
	req, _ := http.NewRequest("POST", ts.URL, io.LimitReader(countReader{neverEnding('a'), &nWritten}, limit*200))

	// Send the POST, but don't care it succeeds or not.  The
	// remote side is going to reply and then close the TCP
	// connection, and HTTP doesn't really define if that's
	// allowed or not.  Some HTTP clients will get the response
	// and some (like ours, currently) will complain that the
	// request write failed, without reading the response.
	//
	// But that's okay, since what we're really testing is that
	// the remote side hung up on us before we wrote too much.
	_, _ = http.DefaultClient.Do(req)

	if nWritten > limit*100 {
		t.Errorf("handler restricted the request body to %d bytes, but client managed to write %d",
			limit, nWritten)
	}
}

// TestClientWriteShutdown tests that if the client shuts down the write
// side of their TCP connection, the server doesn't send a 400 Bad Request.
func TestClientWriteShutdown(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	err = conn.(*net.TCPConn).CloseWrite()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	donec := make(chan bool)
	go func() {
		defer close(donec)
		bs, err := ioutil.ReadAll(conn)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		got := string(bs)
		if got != "" {
			t.Errorf("read %q from server; want nothing", got)
		}
	}()
	select {
	case <-donec:
	case <-time.After(10 * time.Second):
		t.Fatalf("timeout")
	}
}

// Tests that chunked server responses that write 1 byte at a time are
// buffered before chunk headers are added, not after chunk headers.
func TestServerBufferedChunking(t *testing.T) {
	if true {
		t.Logf("Skipping known broken test; see Issue 2357")
		return
	}
	conn := new(testConn)
	conn.readBuf.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	done := make(chan bool)
	ls := &oneConnListener{conn}
	go http.Serve(ls, http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		defer close(done)
		rw.Header().Set("Content-Type", "text/plain") // prevent sniffing, which buffers
		rw.Write([]byte{'x'})
		rw.Write([]byte{'y'})
		rw.Write([]byte{'z'})
	}))
	<-done
	if !bytes.HasSuffix(conn.writeBuf.Bytes(), []byte("\r\n\r\n3\r\nxyz\r\n0\r\n\r\n")) {
		t.Errorf("response didn't end with a single 3 byte 'xyz' chunk; got:\n%q",
			conn.writeBuf.Bytes())
	}
}

// TestContentLengthZero tests that for both an HTTP/1.0 and HTTP/1.1
// request (both keep-alive), when a Handler never writes any
// response, the net/http package adds a "Content-Length: 0" response
// header.
func TestContentLengthZero(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {}))
	defer ts.Close()

	for _, version := range []string{"HTTP/1.0", "HTTP/1.1"} {
		conn, err := net.Dial("tcp", ts.Listener.Addr().String())
		if err != nil {
			t.Fatalf("error dialing: %v", err)
		}
		_, err = fmt.Fprintf(conn, "GET / %v\r\nConnection: keep-alive\r\nHost: foo\r\n\r\n", version)
		if err != nil {
			t.Fatalf("error writing: %v", err)
		}
		req, _ := http.NewRequest("GET", "/", nil)
		res, err := http.ReadResponse(bufio.NewReader(conn), req)
		if err != nil {
			t.Fatalf("error reading response: %v", err)
		}
		if te := res.TransferEncoding; len(te) > 0 {
			t.Errorf("For version %q, Transfer-Encoding = %q; want none", version, te)
		}
		if cl := res.ContentLength; cl != 0 {
			t.Errorf("For version %q, Content-Length = %v; want 0", version, cl)
		}
		conn.Close()
	}
}

// goTimeout runs f, failing t if f takes more than ns to complete.
func goTimeout(t *testing.T, d time.Duration, f func()) {
	ch := make(chan bool, 2)
	timer := time.AfterFunc(d, func() {
		t.Errorf("Timeout expired after %v", d)
		ch <- true
	})
	defer timer.Stop()
	go func() {
		defer func() { ch <- true }()
		f()
	}()
	<-ch
}

type errorListener struct {
	errs []error
}

func (l *errorListener) Accept() (c net.Conn, err error) {
	if len(l.errs) == 0 {
		return nil, io.EOF
	}
	err = l.errs[0]
	l.errs = l.errs[1:]
	return
}

func (l *errorListener) Close() error {
	return nil
}

func (l *errorListener) Addr() net.Addr {
	return dummyAddr("test-address")
}

func TestAcceptMaxFds(t *testing.T) {
	log.SetOutput(ioutil.Discard) // is noisy otherwise
	defer log.SetOutput(os.Stderr)

	ln := &errorListener{[]error{
		&net.OpError{
			Op:  "accept",
			Err: syscall.EMFILE,
		}}}
	err := http.Serve(ln, http.HandlerFunc(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})))
	if err != io.EOF {
		t.Errorf("got error %v, want EOF", err)
	}
}

func BenchmarkClientServer(b *testing.B) {
	b.StopTimer()
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(rw, "Hello world.\n")
	}))
	defer ts.Close()
	b.StartTimer()

	for i := 0; i < b.N; i++ {
		res, err := http.Get(ts.URL)
		if err != nil {
			b.Fatal("Get:", err)
		}
		all, err := ioutil.ReadAll(res.Body)
		if err != nil {
			b.Fatal("ReadAll:", err)
		}
		body := string(all)
		if body != "Hello world.\n" {
			b.Fatal("Got body:", body)
		}
	}

	b.StopTimer()
}
