package nktest

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/gorilla/websocket"
	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"
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
	logger   Logger
}

// NewProxy creates a new http and websocket logging proxy.
func NewProxy(opts ...ProxyOption) *Proxy {
	p := &Proxy{
		addr:   ":0",
		wsPath: "/ws",
		logger: NewLogger(nil),
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
	return scheme + "://" + l.Addr().(*net.TCPAddr).String(), nil
}

// Addr returns the listening address.
func (p *Proxy) Addr() string {
	return p.addr
}

// Run runs the proxy.
func (p *Proxy) run(ctx context.Context, l net.Listener, scheme, wsScheme string, u *url.URL) {
	mux := http.NewServeMux()
	// proxy websockets
	mux.HandleFunc(p.wsPath, func(res http.ResponseWriter, req *http.Request) {
		p.logger.Logf("WS OPEN: %s", req.RemoteAddr)
		urlstr := wsScheme + "://" + u.Host + req.URL.Path
		// connect outgoing websocket
		p.logger.Logf("WS DIAL: %s", urlstr)
		out, pres, err := p.dialer.DialContext(ctx, urlstr, nil)
		if err != nil {
			defer pres.Body.Close()
			s := fmt.Sprintf("could not connect to %s: %v", urlstr, err)
			p.logger.Errf("WS DIAL ERROR: " + s)
			buf, err := ioutil.ReadAll(pres.Body)
			if err != nil {
				p.logger.Errf("WS DIAL ERROR UNABLE TO READ BODY: %v", err)
				http.Error(res, s, http.StatusInternalServerError)
				return
			}
			p.logger.Errf("WS DIAL ERROR STATUS: %d %s", pres.StatusCode, http.StatusText(pres.StatusCode))
			res.WriteHeader(pres.StatusCode)
			keys := maps.Keys(pres.Header)
			slices.Sort(keys)
			for _, k := range keys {
				p.logger.Errf("WS DIAL ERROR HEADER: %s: %s", k, strings.Join(pres.Header[k], " "))
				res.Header()[k] = pres.Header[k]
			}
			p.logger.Errf("WS DIAL ERROR BODY: %s", string(buf))
			_, _ = res.Write(buf)
			return
		}
		defer pres.Body.Close()
		defer out.Close()
		p.logger.Logf("WS CONNECTED: %s", urlstr)
		// connect incoming websocket
		in, err := p.upgrader.Upgrade(res, req, nil)
		if err != nil {
			s := fmt.Sprintf("could not upgrade websocket %s: %v", req.RemoteAddr, err)
			p.logger.Errf("WS UPGRADE ERROR: " + s)
			http.Error(res, s, http.StatusInternalServerError)
			return
		}
		defer in.Close()
		p.logger.Logf("WS UPGRADED: %s", req.RemoteAddr)
		errc := make(chan error, 1)
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		defer close(errc)
		go p.ws(ctx, DefaultInPrefix, in, out, errc)
		go p.ws(ctx, DefaultOutPrefix, out, in, errc)
		p.logger.Logf("WS CLOSE: %s %v", req.RemoteAddr, <-errc)
	})
	// proxy anything else
	prox := httputil.NewSingleHostReverseProxy(&url.URL{
		Scheme: scheme,
		Host:   u.Host,
	})
	prox.Transport = p.logger.Transport(nil)
	mux.Handle("/", prox)
	_ = http.Serve(l, mux)
}

// ws proxies in and out messages for a websocket connection, logging the
// message to the logger with the passed prefix. Any error encountered will be
// sent to errc.
func (p *Proxy) ws(ctx context.Context, prefix string, in, out *websocket.Conn, errc chan error) {
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
			p.logger.Logf(prefix + string(buf))
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

// WithLogger is a proxy option to set logger.
func WithLogger(logger Logger) ProxyOption {
	return func(p *Proxy) {
		p.logger = logger
	}
}

// WithLogf is a proxy option to set a wrapped logger. Useful with *testing.T.
func WithLogf(f func(string, ...interface{})) ProxyOption {
	return func(p *Proxy) {
		p.logger = NewLogger(f)
	}
}
