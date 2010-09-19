package server

import (
	"bufio"
	"bytes"
	"github.com/garyburd/twister/web"
	"http"
	"io"
	"log"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var ErrResponseBodyClosed = os.NewError("response body closed")
var ErrResponseStarted = os.NewError("response started")

type conn struct {
	atLeastHttp11      bool
	br                 *bufio.Reader
	bw                 *bufio.Writer
	chunked            bool
	closeAfterResponse bool
	hijacked           bool
	netConn            net.Conn
	req                *web.Request
	requestAvail       int
	requestErr         os.Error
	respondCalled      bool
	responseAvail      int
	responseErr        os.Error
	write100Continue   bool
}

func skipBytes(p []byte, f func(byte) bool) int {
	i := 0
	for ; i < len(p); i++ {
		if !f(byte(p[i])) {
			break
		}
	}
	return i
}

func trimWSLeft(p []byte) []byte {
	return p[skipBytes(p, web.IsSpaceByte):]
}

func trimWSRight(p []byte) []byte {
	var i int
	for i = len(p); i > 0; i-- {
		if !web.IsSpaceByte(p[i-1]) {
			break
		}
	}
	return p[0:i]
}

var requestLineRegexp = regexp.MustCompile("^([_A-Za-z0-9]+) ([^ ]+) HTTP/([0-9]+)\\.([0-9]+)$")

func readStatusLine(b *bufio.Reader, req *web.Request) os.Error {

	p, err := b.ReadSlice('\n')
	if err != nil {
		return err
	}

	p = trimWSRight(p)

	m := requestLineRegexp.FindSubmatch(p)
	if m == nil {
		return os.ErrorString("malformed request line")
	}

	req.Method = string(bytes.ToUpper(m[1]))

	req.ProtocolMajor, err = strconv.Atoi(string(m[3]))
	if err != nil {
		return os.ErrorString("bad major version")
	}

	req.ProtocolMinor, err = strconv.Atoi(string(m[4]))
	if err != nil {
		return os.ErrorString("bad minor version")
	}

	req.URL, err = http.ParseURL(string(m[2]))
	if err != nil {
		return os.ErrorString("bad url")
	}

	return nil
}

func readHeaders(b *bufio.Reader, header web.StringsMap) os.Error {

	const (
		// Max size for header line
		maxLineSize = 4096
		// Max size for header value
		maxValueSize = 4096
		// Maximum number of headers 
		maxHeaderCount = 256
	)

	lastKey := ""
	headerCount := 0

	for {
		p, err := b.ReadSlice('\n')
		if err != nil {
			return err
		}

		// remove line terminator
		if len(p) >= 2 && p[len(p)-2] == '\r' {
			// \r\n
			p = p[0 : len(p)-2]
		} else {
			// \n
			p = p[0 : len(p)-1]
		}

		// End of headers?
		if len(p) == 0 {
			break
		}

		// Don't allow huge header lines.
		if len(p) > maxLineSize {
			return os.ErrorString("header line too long")
		}

		if web.IsSpaceByte(p[0]) {

			if lastKey == "" {
				return os.ErrorString("header continuation before first header")
			}

			p = trimWSLeft(trimWSRight(p))

			if len(p) > 0 {
				values := header[lastKey]
				value := values[len(values)-1]
				value = value + " " + string(p)
				if len(value) > maxValueSize {
					return os.ErrorString("header value too long")
				}
				values[len(values)-1] = value
			}

		} else {

			// New header
			headerCount = headerCount + 1
			if headerCount > maxHeaderCount {
				return os.ErrorString("too many headers")
			}

			// Key
			i := skipBytes(p, web.IsTokenByte)
			if i < 1 {
				return os.ErrorString("missing header key")
			}
			key := web.HeaderNameBytes(p[0:i])
			p = p[i:]
			lastKey = key

			p = trimWSLeft(p)

			// Colon
			if p[0] != ':' {
				return os.ErrorString("header missing :")
			}
			p = p[1:]

			// Value 
			p = trimWSLeft(p)
			value := string(trimWSRight(p))
			header.Append(key, value)
		}
	}
	return nil
}

func (c *conn) prepare() os.Error {

	req := web.NewRequest()
	c.req = req

	if err := readStatusLine(c.br, req); err != nil {
		return err
	}

	if err := readHeaders(c.br, req.Header); err != nil {
		return err
	}

	if s, found := req.Header.Get(web.HeaderContentLength); found {
		var err os.Error
		req.ContentLength, err = strconv.Atoi(s)
		if err != nil {
			return os.ErrorString("bad content length")
		}
		c.requestAvail = req.ContentLength
	} else {
		req.ContentLength = -1
	}

	if s, found := req.Header.Get(web.HeaderExpect); found {
		c.write100Continue = strings.ToLower(s) == "100-continue"
	}

	c.atLeastHttp11 = req.ProtocolMajor > 1 || req.ProtocolMajor == 1 && req.ProtocolMinor >= 1
	if c.atLeastHttp11 {
		if s, found := req.Header.Get(web.HeaderConnection); found && strings.ToLower(s) == "close" {
			c.closeAfterResponse = true
		}
	} else {
		c.closeAfterResponse = true
	}

	req.Connection = c
	req.Body = requestReader{c}
	return nil
}

type requestReader struct {
	*conn
}

func (c requestReader) Read(p []byte) (int, os.Error) {
	if c.requestErr != nil {
		return 0, c.requestErr
	}
	if c.write100Continue {
		c.write100Continue = false
		io.WriteString(c.netConn, "HTTP/1.1 100 Continue\r\n\r\n")
	}
	if c.requestAvail <= 0 {
		c.requestErr = os.EOF
		return 0, c.requestErr
	}
	if len(p) > c.requestAvail {
		p = p[0:c.requestAvail]
	}
	var n int
	n, c.requestErr = c.br.Read(p)
	c.requestAvail -= n
	return n, c.requestErr
}

func (c *conn) Respond(status int, header web.StringsMap) (body web.ResponseBody) {
	if c.hijacked {
		log.Stderr("twister: Respond called on hijacked connection")
		return nil
	}
	if c.respondCalled {
		log.Stderr("twister: multiple calls to Respond")
		return nil
	}
	c.respondCalled = true
	c.chunked = true

	c.requestErr = ErrResponseStarted
	if c.requestAvail > 0 {
		c.closeAfterResponse = true
	}

	if status == web.StatusNotModified {
		header[web.HeaderContentType] = nil, false
		header[web.HeaderTransferEncoding] = nil, false
		c.chunked = false
	}

	if c.closeAfterResponse {
		header.Set(web.HeaderConnection, "close")
		c.chunked = false
	}

	c.responseAvail = 0
	if s, found := header.Get(web.HeaderContentLength); found {
		c.responseAvail, _ = strconv.Atoi(s)
		c.chunked = false
	}
	if _, found := header[web.HeaderTransferEncoding]; found {
		c.chunked = false
	}

	if c.chunked {
		header.Set(web.HeaderTransferEncoding, "chunked")
	}

	proto := "HTTP/1.0"
	if c.atLeastHttp11 {
		proto = "HTTP/1.1"
	}
	statusString := strconv.Itoa(status)
	text, found := web.StatusText[status]
	if !found {
		text = "status code " + statusString
	}

	var b bytes.Buffer
	b.WriteString(proto)
	b.WriteString(" ")
	b.WriteString(statusString)
	b.WriteString(" ")
	b.WriteString(text)
	b.WriteString("\r\n")
	for key, values := range header {
		for _, value := range values {
			b.WriteString(key)
			b.WriteString(": ")
			b.WriteString(value)
			b.WriteString("\r\n")
		}
	}
	b.WriteString("\r\n")

	if c.chunked {
		c.bw = bufio.NewWriter(chunkedWriter{c})
		_, c.responseErr = c.netConn.Write(b.Bytes())
	} else {
		c.bw = bufio.NewWriter(identityWriter{c})
		c.bw.Write(b.Bytes())
	}

	return c.bw
}

func (c *conn) Hijack() (rwc io.ReadWriteCloser, buf *bufio.ReadWriter, err os.Error) {
	return
}

// Finish the HTTP request
func (c *conn) finish() os.Error {
	if !c.respondCalled {
		c.req.Respond(web.StatusOK, web.HeaderContentType, "text/html charset=utf-8")
	}
	if c.responseAvail != 0 {
		c.closeAfterResponse = true
	}
	c.bw.Flush()
	if c.chunked {
		_, c.responseErr = io.WriteString(c.netConn, "0\r\n\r\n")
	}
	if c.responseErr == nil {
		c.responseErr = ErrResponseBodyClosed
	}
	c.netConn = nil
	c.br = nil
	c.bw = nil
	return nil
}

type identityWriter struct {
	*conn
}

func (c identityWriter) Write(p []byte) (int, os.Error) {
	if c.responseErr != nil {
		return 0, c.responseErr
	}
	var n int
	n, c.responseErr = c.netConn.Write(p)
	c.responseAvail -= n
	return n, c.responseErr
}

type chunkedWriter struct {
	*conn
}

func (c chunkedWriter) Write(p []byte) (int, os.Error) {
	if c.responseErr != nil {
		return 0, c.responseErr
	}
	if len(p) == 0 {
		return 0, nil
	}
	_, c.responseErr = io.WriteString(c.netConn, strconv.Itob(len(p), 16)+"\r\n")
	if c.responseErr != nil {
		return 0, c.responseErr
	}
	var n int
	n, c.responseErr = c.netConn.Write(p)
	if c.responseErr != nil {
		return n, c.responseErr
	}
	_, c.responseErr = io.WriteString(c.netConn, "\r\n")
	return 0, c.responseErr
}

func serveConnection(netConn net.Conn, handler web.Handler) {
	br := bufio.NewReader(netConn)
	for {
		c := conn{netConn: netConn, br: br}
		if err := c.prepare(); err != nil {
			break
		}
		handler.ServeWeb(c.req)
		if c.hijacked {
			return
		}
		if err := c.finish(); err != nil {
			break
		}
		if c.closeAfterResponse {
			break
		}
	}
	netConn.Close()
}

// Serve accepts incoming HTTP connections on the listener l, creating a new
// goroutine for each. The goroutines read requests and then call handler to
// reply to them.
func Serve(l net.Listener, handler web.Handler) os.Error {
	for {
		netConn, e := l.Accept()
		if e != nil {
			return e
		}
		go serveConnection(netConn, handler)
	}
	return nil
}

// ListenAndServe listens on the TCP network address addr and then calls Serve
// with handler to handle requests on incoming connections.  
func ListenAndServe(addr string, handler web.Handler) os.Error {
	l, e := net.Listen("tcp", addr)
	if e != nil {
		return e
	}
	defer l.Close()
	return Serve(l, handler)
}
