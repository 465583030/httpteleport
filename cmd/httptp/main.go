package main

import (
	"crypto/sha256"
	"crypto/tls"
	"expvar"
	"flag"
	"fmt"
	"github.com/valyala/fasthttp"
	"github.com/valyala/httpteleport"
	"github.com/valyala/tcplisten"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

var (
	reusePort = flag.Bool("reusePort", false, "Whether to enable SO_REUSEPORT on -in if -inType is http or teleport")

	in     = flag.String("in", "127.0.0.1:8080", "-inType address to listen to for incoming requests")
	inType = flag.String("inType", "http", "Type of -in address. Supported values:\n"+
		"\thttp - accept http requests over TCP, e.g. -in=127.0.0.1:8080\n"+
		"\thttps - accept https requests over TCP, e.g. -in=127.0.0.1:443\n"+
		"\tunix - accept http requests over unix socket, e.g. -in=/var/httptp/sock.unix\n"+
		"\tteleport - accept httpteleport connections over TCP, e.g. -in=127.0.0.1:8043\n"+
		"\tteleports - accept httpteleport connections over encrypted TCP, e.g. -in=127.0.0.1:8443")
	inDelay    = flag.Duration("inDelay", 0, "How long to wait before sending batched responses back if -inType=teleport")
	inCompress = flag.String("inCompress", "flate", "Which compression to use for responses if -inType=teleport.\n"+
		"\tSupported values:\n"+
		"\tnone - responses aren't compressed. Low CPU usage at the cost of high network bandwidth\n"+
		"\tflate - responses are compressed using flate algorithm. Low network bandwidth at the cost of high CPU usage\n"+
		"\tsnappy - responses are compressed using snappy algorithm. Balance between network bandwidth and CPU usage")

	inGetOnly       = flag.Bool("inGetOnly", false, "Accept only GET -in requests if set to true")
	inMaxHeaderSize = flag.Int("inMaxHeaderSize", 4*1024, "Maximum header size for -in requests")
	inMaxBodySize   = flag.Int("inMaxBodySize", fasthttp.DefaultMaxRequestBodySize, "Maximum body size for -in requests")
	inAllowIP       = flag.String("inAllowIP", "", "Comma-separated list of IP addresses allowed for establishing connections to -in.\n"+
		"\tAll IP addresses are allowed if empty")
	inTLSCert = flag.String("inTLSCert", "/etc/ssl/certs/ssl-cert-snakeoil.pem",
		"Path to TLS certificate file if -inType=https or teleports")
	inTLSKey = flag.String("inTLSKey", "/etc/ssl/private/ssl-cert-snakeoil.key",
		"Path to TLS key file if -inType=https or teleports")
	inTLSSessionTicketKey = flag.String("inTLSSessionTicketKey", "", "TLS sesssion ticket key if -inType=https or teleports. "+
		"Automatically generated if empty.\n"+
		"\tSee https://blog.cloudflare.com/tls-session-resumption-full-speed-and-secure/ for details")

	out = flag.String("out", "127.0.0.1:8043", "Comma-separated list of -outType addresses to forward requests to.\n"+
		"\tEach request is forwarded to the least loaded address")
	outType = flag.String("outType", "teleport", "Type of -out address. Supported values:\n"+
		"\thttp - forward requests to http servers on TCP, e.g. -out=127.0.0.1:80\n"+
		"\thttps - forward requests to https servers on TCP, e.g -out=127.0.0.1:443\n"+
		"\tunix - forward requests to http servers on unix socket, e.g. -out=/var/nginx/sock.unix\n"+
		"\tteleport - forward requests to httpteleport servers over TCP, e.g. -out=127.0.0.1:8043\n"+
		"\ttepelorts - forward requests to httpteleport servers over encrypted TCP, e.g. -out=127.0.0.1:8043. "+
		"Server must properly set -inTLS* flags in order to accept encrypted TCP connections")
	outDelay    = flag.Duration("outDelay", 0, "How long to wait before forwarding incoming requests to -out if -outType=teleport")
	outCompress = flag.String("outCompress", "flate", "Which compression to use for requests if -outType=teleport.\n"+
		"\tSupported values:\n"+
		"\tnone - requests aren't compressed. Low CPU usage at the cost of high network bandwidth\n"+
		"\tflate - requests are compressed using flate algorithm. Low network bandwidth at the cost of high CPU usage\n"+
		"\tsnappy - requests are compressed using snappy algorithm. Balance between network bandwidth and CPU usage")

	outMaxHeaderSize = flag.Int("outMaxHeaderSize", 4*1024, "Maximum header size for -out responses")
	outTimeout       = flag.Duration("outTimeout", 3*time.Second, "The maximum duration for waiting responses from -out server")
	outConnsPerAddr  = flag.Int("outConnsPerAddr", 1, "How many connections must be established per each -out server if -outType=teleport.\n"+
		"\tUsually a single connection is enough. Increase this value if the compression\n"+
		"\ton the connection occupies 100% of a single CPU core.\n"+
		"\tAlternatively, -inCompress and/or -outCompress may be set to snappy or none in order to reduce CPU load")

	concurrency = flag.Int("concurrency", 100000, "The maximum number of concurrent requests httptp may process.\n"+
		"\tThis also limits the maximum number of open connections per -out address if -outType=http or https")
	clientIPHeader = flag.String("clientIPHeader", "", "HTTP request header for sending the original client ip.\n"+
		"\tFor instance, -clientIPHeader=X-Forwarded-For. Empty -clientIPHeader disables sending client ip in request headers")

	logAllErrors = flag.Bool("logAllErrors", false, "Log all the error while serving clients. This option may be useful for debugging")
)

func main() {
	flag.Parse()

	initExpvarServer()

	var err error
	if allowedInIPs, err = parseAllowedIPs(*inAllowIP); err != nil {
		log.Fatalf("cannot parse -inAllowIP: %s", err)
	}
	if allowedInIPs != nil {
		log.Printf("allowing incoming connections from -inAllowIP=%s", *inAllowIP)
	}

	outs := strings.Split(*out, ",")
	switch *outType {
	case "http":
		initHTTPClients(outs)
	case "https":
		initHTTPSClients(outs)
	case "unix":
		initUnixClients(outs)
	case "teleport":
		initTeleportClients(outs)
	case "teleports":
		initTeleportsClients(outs)
	default:
		log.Fatalf("unknown -outType=%q. Supported values are: http, https, unix, teleport, teleports", *outType)
	}

	switch *inType {
	case "http":
		serveHTTP()
	case "https":
		serveHTTPS()
	case "unix":
		serveUnix()
	case "teleport":
		serveTeleport()
	case "teleports":
		serveTeleports()
	default:
		log.Fatalf("unknown -inType=%q. Supported values are: http, https, unix, teleport, teleports", *inType)
	}
}

func initHTTPClients(outs []string) {
	initHTTPClientsExt(outs, false)
}

func initHTTPSClients(outs []string) {
	initHTTPClientsExt(outs, true)
}

func initHTTPClientsExt(outs []string, isTLS bool) {
	connsPerAddr := (*concurrency + len(outs) - 1) / len(outs)
	var cc []fasthttp.BalancingClient
	for _, addr := range outs {
		c := newHTTPClient(fasthttp.Dial, addr, connsPerAddr, isTLS)
		cc = append(cc, c)
	}
	upstreamClients.Clients = cc
	tlsSuffix := ""
	if isTLS {
		tlsSuffix = "s"
	}
	log.Printf("forwarding requests to http%s servers at %q", tlsSuffix, outs)
}

func initUnixClients(outs []string) {
	connsPerAddr := (*concurrency + len(outs) - 1) / len(outs)
	var cc []fasthttp.BalancingClient
	for _, addr := range outs {
		verifyUnixAddr(addr)
		c := newHTTPClient(dialUnix, addr, connsPerAddr, false)
		cc = append(cc, c)
	}
	upstreamClients.Clients = cc
	log.Printf("forwarding requests to http servers at unix:%q", outs)
}

func verifyUnixAddr(addr string) {
	fi, err := os.Stat(addr)
	if err != nil {
		log.Fatalf("error when accessing unix:%q: %s", addr, err)
	}
	mode := fi.Mode()
	if (mode & os.ModeSocket) == 0 {
		log.Fatalf("the %q must be unix socket", addr)
	}
}

func initTeleportClients(outs []string) {
	initTeleportClientsExt(outs, false)
}

func initTeleportsClients(outs []string) {
	initTeleportClientsExt(outs, true)
}

func initTeleportClientsExt(outs []string, isTLS bool) {
	concurrencyPerAddr := (*concurrency + len(outs) - 1) / len(outs)
	concurrencyPerAddr = (concurrencyPerAddr + *outConnsPerAddr - 1) / *outConnsPerAddr
	outCompressType := compressType(*outCompress, "outCompress")
	var cc []fasthttp.BalancingClient
	for _, addr := range outs {
		c := &httpteleport.Client{
			Addr:               addr,
			Dial:               newExpvarDial(fasthttp.Dial),
			MaxBatchDelay:      *outDelay,
			MaxPendingRequests: concurrencyPerAddr,
			ReadTimeout:        120 * time.Second,
			WriteTimeout:       5 * time.Second,
			CompressType:       outCompressType,
			ReadBufferSize:     *outMaxHeaderSize,
		}
		if isTLS {
			serverName, _, err := net.SplitHostPort(addr)
			if err != nil {
				log.Fatalf("cannot extract teleport server name from %q: %s", addr, err)
			}
			c.TLSConfig = &tls.Config{
				ServerName: serverName,
			}
		}
		cc = append(cc, c)
	}

	var cs []fasthttp.BalancingClient
	for i := 0; i < *outConnsPerAddr; i++ {
		cs = append(cs, cc...)
	}
	upstreamClients.Clients = cs
	secureStr := ""
	if isTLS {
		secureStr = "encrypted "
	}
	log.Printf("forwarding %srequests to httpteleport servers at %q", secureStr, outs)
}

func compressType(ct, name string) httpteleport.CompressType {
	switch ct {
	case "none":
		return httpteleport.CompressNone
	case "flate":
		return httpteleport.CompressFlate
	case "snappy":
		return httpteleport.CompressSnappy
	default:
		log.Fatalf("unknown -%s: %q. Supported values: none, flate, snappy", name, ct)
	}
	panic("unreached")
}

func newHTTPClient(dial fasthttp.DialFunc, addr string, connsPerAddr int, isTLS bool) fasthttp.BalancingClient {
	c := &fasthttp.HostClient{
		Addr:           addr,
		Dial:           newExpvarDial(dial),
		MaxConns:       connsPerAddr,
		ReadTimeout:    *outTimeout * 5,
		WriteTimeout:   *outTimeout,
		ReadBufferSize: *outMaxHeaderSize,
	}
	if isTLS {
		serverName, _, err := net.SplitHostPort(addr)
		if err != nil {
			log.Fatalf("cannot extract http server name from %q: %s", addr, err)
		}
		c.IsTLS = true
		c.TLSConfig = &tls.Config{
			ServerName: serverName,
		}
	}
	return c
}

func dialUnix(addr string) (net.Conn, error) {
	return net.Dial("unix", addr)
}

func serveHTTP() {
	ln := newTCPListener()
	s := newHTTPServer()

	log.Printf("listening for http requests on %q", *in)
	if err := s.Serve(ln); err != nil {
		log.Fatalf("error in fasthttp server: %s", err)
	}
}

func serveHTTPS() {
	ln := newTCPListener()
	tlsConfig := newInTLSConfig()
	lnTLS := tls.NewListener(ln, tlsConfig)
	s := newHTTPServer()

	log.Printf("listening for https requests on %q", *in)
	if err := s.Serve(lnTLS); err != nil {
		log.Fatalf("error in fasthttp server: %s", err)
	}
}

func serveUnix() {
	addr := *in
	if _, err := os.Stat(addr); err == nil {
		verifyUnixAddr(addr)
		if err := os.Remove(addr); err != nil {
			log.Fatalf("cannot remove %q: %s", addr, err)
		}
	}

	ln, err := net.Listen("unix", addr)
	if err != nil {
		log.Fatalf("cannot listen to -in=%q: %s", addr, err)
	}
	s := newHTTPServer()

	log.Printf("listening for http requests on unix:%q", addr)
	if err := s.Serve(ln); err != nil {
		log.Fatalf("error in fasthttp server: %s", err)
	}
}

func serveTeleport() {
	serveTeleportExt(false)
}

func serveTeleports() {
	serveTeleportExt(true)
}

func serveTeleportExt(isTLS bool) {
	ln := newTCPListener()
	var tlsConfig *tls.Config
	if isTLS {
		tlsConfig = newInTLSConfig()
	}
	inCompressType := compressType(*inCompress, "inCompress")
	s := httpteleport.Server{
		Handler:           httpteleportRequestHandler,
		Concurrency:       *concurrency,
		MaxBatchDelay:     *inDelay,
		TLSConfig:         tlsConfig,
		ReduceMemoryUsage: true,
		ReadTimeout:       120 * time.Second,
		WriteTimeout:      5 * time.Second,
		CompressType:      inCompressType,
		ReadBufferSize:    *inMaxHeaderSize,
	}

	secureStr := ""
	if isTLS {
		secureStr = "encrypted "
	}
	log.Printf("listening for %shttpteleport connections on %q", secureStr, *in)
	if err := s.Serve(ln); err != nil {
		log.Fatalf("error in fasthttp server: %s", err)
	}
}

func newTCPListener() net.Listener {
	cfg := tcplisten.Config{
		ReusePort: *reusePort,
	}
	ln, err := cfg.NewListener("tcp4", *in)
	if err != nil {
		log.Fatalf("cannot listen to -in=%q: %s", *in, err)
	}

	if allowedInIPs != nil {
		ln = &ipCheckListener{
			Listener:   ln,
			allowedIPs: allowedInIPs,
		}
	}
	return &expvarListener{
		Listener: ln,
	}
}

var allowedInIPs map[uint32]struct{}

func newHTTPServer() *fasthttp.Server {
	return &fasthttp.Server{
		Handler:            httpRequestHandler,
		Concurrency:        *concurrency,
		LogAllErrors:       *logAllErrors,
		MaxRequestBodySize: *inMaxBodySize,
		GetOnly:            *inGetOnly,
		ReduceMemoryUsage:  true,
		ReadTimeout:        120 * time.Second,
		WriteTimeout:       5 * time.Second,
		ReadBufferSize:     *inMaxHeaderSize,
	}
}

var (
	inRequestStart        = expvar.NewInt("inRequestStart")
	inRequestSuccess      = expvar.NewInt("inRequestSuccess")
	inRequestNon200       = expvar.NewInt("inRequestNon200")
	inRequestTimeoutError = expvar.NewInt("inRequestTimeoutError")
	inRequestOtherError   = expvar.NewInt("inRequestOtherError")
)

func httpRequestHandler(ctx *fasthttp.RequestCtx) {
	if len(*clientIPHeader) > 0 {
		var buf [16]byte
		ip := fasthttp.AppendIPv4(buf[:0], ctx.RemoteIP())
		ctx.Request.Header.SetBytesV(*clientIPHeader, ip)
	}
	commonRequestHandler("http", ctx)
}

func httpteleportRequestHandler(ctx *fasthttp.RequestCtx) {
	commonRequestHandler("httpteleport", ctx)
}

func commonRequestHandler(proxyType string, ctx *fasthttp.RequestCtx) {
	inRequestStart.Add(1)

	// Reset 'Connection: close' request header in order to prevent
	// from closing keep-alive connections to -out servers.
	ctx.Request.Header.ResetConnectionClose()

	err := upstreamClients.DoTimeout(&ctx.Request, &ctx.Response, *outTimeout)
	if err == nil {
		inRequestSuccess.Add(1)
		if ctx.Response.StatusCode() != fasthttp.StatusOK {
			inRequestNon200.Add(1)
		}
		return
	}

	ctx.ResetBody()
	fmt.Fprintf(ctx, "%s proxying error: %s", proxyType, err)
	if err == fasthttp.ErrTimeout {
		inRequestTimeoutError.Add(1)
		ctx.SetStatusCode(fasthttp.StatusGatewayTimeout)
	} else {
		inRequestOtherError.Add(1)
		ctx.SetStatusCode(fasthttp.StatusBadGateway)
	}
}

var upstreamClients fasthttp.LBClient

func newInTLSConfig() *tls.Config {
	cert, err := tls.LoadX509KeyPair(*inTLSCert, *inTLSKey)
	if err != nil {
		log.Fatalf("cannot load TLS certificate from -inTLSCert=%q and -inTLSKey=%q: %s", *inTLSCert, *inTLSKey, err)
	}
	tlsConfig := &tls.Config{
		Certificates:             []tls.Certificate{cert},
		PreferServerCipherSuites: true,
	}
	if len(*inTLSSessionTicketKey) > 0 {
		tlsConfig.SessionTicketKey = sha256.Sum256([]byte(*inTLSSessionTicketKey))
	}
	return tlsConfig
}
