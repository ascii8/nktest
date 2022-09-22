package nktest

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"
)

// DefaultProxyReadSize is the default websocket proxy read size.
var DefaultProxyReadSize = 10 * 1024 * 1024

// DefaultProxyWriteSize is the default websocket proxy write size.
var DefaultProxyWriteSize = 10 * 1024 * 1024

// Proxy is a http and websocket logging proxy.
type Proxy struct {
	addr     string
	wsPath   string
	upgrader *websocket.Upgrader
	dialer   *websocket.Dialer
}

// NewProxy creates a new http and websocket logging proxy.
func NewProxy(opts ...ProxyOption) *Proxy {
	p := &Proxy{
		addr:   ":0",
		wsPath: "/ws",
	}
	for _, o := range opts {
		o(p)
	}
	if p.upgrader == nil {
		p.upgrader = &websocket.Upgrader{
			ReadBufferSize:  DefaultProxyReadSize,
			WriteBufferSize: DefaultProxyWriteSize,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		}
	}
	if p.dialer == nil {
		p.dialer = &websocket.Dialer{
			ReadBufferSize:  DefaultProxyWriteSize,
			WriteBufferSize: DefaultProxyReadSize,
		}
	}
	return p
}

// Run proxies requests to the url until the context is closed.
func (p *Proxy) Run(ctx context.Context, urlstr string) (string, error) {
	// determine remote
	u, err := url.Parse(urlstr)
	if err != nil {
		return "", fmt.Errorf("invalid url %q: %w", urlstr, err)
	}
	// determine schemes
	scheme, wsScheme := strings.ToLower(u.Scheme), "ws"
	switch scheme {
	case "http":
	case "https":
		wsScheme = "wss"
	default:
		return "", fmt.Errorf("unknown scheme %q", u.Scheme)
	}
	// listen
	l, err := (&net.ListenConfig{}).Listen(ctx, "tcp", p.addr)
	if err != nil {
		return "", fmt.Errorf("unable to listen on %s: %w", p.addr, err)
	}
	// run
	go p.run(ctx, l, scheme, wsScheme, u)
	return scheme + "://" + LocalAddr(l), nil
}

// Addr returns the listening address.
func (p *Proxy) Addr() string {
	return p.addr
}

// InternalError handles internal errors.
func (p Proxy) InternalError(ctx context.Context, res http.ResponseWriter, s string, v ...interface{}) {
	s = fmt.Sprintf(s, v...)
	Logf(ctx, s)
	http.Error(res, s, http.StatusInternalServerError)
}

func (p Proxy) DialError(ctx context.Context, inWriter io.Writer, w http.ResponseWriter, req *http.Request, res *http.Response, err error) {
	Logf(ctx, "% 16s: %v", "WS DIAL ERROR", err)
	if res == nil {
		return
	}
	defer res.Body.Close()
	Errf(ctx, "% 16s: %d %s", "WS DIAL ERROR STATUS", res.StatusCode, http.StatusText(res.StatusCode))
	body, err := httputil.DumpResponse(res, true)
	if err != nil {
		p.InternalError(ctx, w, "WS DIAL ERROR: %v", err)
		return
	}
	_, _ = inWriter.Write(body)
	// read body
	buf, err := io.ReadAll(res.Body)
	if err != nil {
		p.InternalError(ctx, w, "WS DIAL ERROR: unable to read body: %v", err)
		return
	}
	// emit
	w.WriteHeader(res.StatusCode)
	_, _ = w.Write(buf)
}

// Run runs the proxy.
func (p *Proxy) run(ctx context.Context, l net.Listener, scheme, wsScheme string, u *url.URL) {
	outWriter, inWriter := PrefixedWriter(Stdout(ctx), DefaultPrefixOut), PrefixedWriter(Stdout(ctx), DefaultPrefixIn)
	mux := http.NewServeMux()
	// proxy websockets
	mux.HandleFunc(p.wsPath, func(w http.ResponseWriter, req *http.Request) {
		Logf(ctx, "% 16s: %s", "WS OPEN", req.RemoteAddr)
		// dump request
		buf, err := httputil.DumpRequest(req, true)
		if err != nil {
			p.InternalError(ctx, w, "WS ERROR: %v", err)
			return
		}
		_, _ = outWriter.Write(buf)
		// build url and request header
		urlstr := wsScheme + "://" + u.Host + p.wsPath
		if req.URL.RawQuery != "" {
			urlstr += "?" + req.URL.RawQuery
		}
		header := http.Header{}
		if s := req.Header.Get("Authorization"); s != "" {
			header.Set("Authorization", s)
		}
		// connect outgoing websocket
		Logf(ctx, "% 16s: %s", "WS DIAL", urlstr)
		out, pres, err := p.dialer.DialContext(ctx, urlstr, header)
		if err != nil {
			p.DialError(ctx, inWriter, w, req, pres, err)
			return
		}
		defer pres.Body.Close()
		defer out.Close()
		// upgrade incoming websocket
		in, err := p.upgrader.Upgrade(w, req, nil)
		if err != nil {
			p.InternalError(ctx, w, "WS UPGRADE ERROR: could not upgrade websocket %s: %v", req.RemoteAddr, err)
			return
		}
		defer in.Close()
		Logf(ctx, "% 16s: %s", "WS UPGRADED", req.RemoteAddr)
		errc := make(chan error, 1)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go p.ws(ctx, outWriter, in, out, errc)
		go p.ws(ctx, inWriter, out, in, errc)
		Logf(ctx, "% 16s: %s %v", "WS CLOSE", req.RemoteAddr, <-errc)
	})
	// proxy anything else
	prox := httputil.NewSingleHostReverseProxy(&url.URL{
		Scheme: scheme,
		Host:   u.Host,
	})
	prox.Transport = NewRoundTripper(outWriter, inWriter, prox.Transport)
	mux.Handle("/", prox)
	_ = http.Serve(l, mux)
}

// ws proxies in and out messages for a websocket connection, logging the
// message to the logger with the passed prefix. Any error encountered will be
// sent to errc.
func (p *Proxy) ws(ctx context.Context, w io.Writer, in, out *websocket.Conn, errc chan error) {
	for {
		var typ int
		var buf []byte
		var err error
		select {
		case <-ctx.Done():
			return
		default:
			if typ, buf, err = in.ReadMessage(); err != nil {
				errc <- err
				return
			}
			s, msg := "TXT", string(buf)
			switch typ {
			case 1:
			case 2:
				s, msg = "BIN", hex.EncodeToString(buf)
			default:
				s = fmt.Sprintf("(%d)", typ)
			}
			fmt.Fprintf(w, "%s: %s", s, msg)
			if err = out.WriteMessage(typ, buf); err != nil {
				errc <- err
				return
			}
		}
	}
}

// ProxyOption is a proxy option.
type ProxyOption func(*Proxy)

// WithAddr is a proxy option to set the listen address.
func WithAddr(addr string) ProxyOption {
	return func(p *Proxy) {
		p.addr = addr
	}
}

// WithWsPath is a proxy option to set the websocket remote path.
func WithWsPath(wsPath string) ProxyOption {
	return func(p *Proxy) {
		p.wsPath = wsPath
	}
}

// WithUpgrader is a proxy option to set the websocket upgrader.
func WithUpgrader(upgrader websocket.Upgrader) ProxyOption {
	return func(p *Proxy) {
		p.upgrader = &upgrader
	}
}

// WithDialer is a proxy option to set the websocket dialer.
func WithDialer(dialer websocket.Dialer) ProxyOption {
	return func(p *Proxy) {
		p.dialer = &dialer
	}
}

// LocalAddr returns the local address of the listener.
func LocalAddr(l net.Listener) string {
	addr := l.Addr().(*net.TCPAddr)
	ip := addr.IP.String()
	switch ip {
	case "", "::", "0.0.0.0":
		ip = "127.0.0.1"
	}
	return ip + ":" + strconv.Itoa(addr.Port)
}
