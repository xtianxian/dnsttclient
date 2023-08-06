// dnstt-client is the client end of a DNS tunnel.
//
// Usage:
//
//	dnstt-client [-doh URL|-dot ADDR|-udp ADDR] -pubkey-file PUBKEYFILE DOMAIN LOCALADDR
//
// Examples:
//
//	dnstt-client -doh https://resolver.example/dns-query -pubkey-file server.pub t.example.com 127.0.0.1:7000
//	dnstt-client -dot resolver.example:853 -pubkey-file server.pub t.example.com 127.0.0.1:7000
//
// The program supports DNS over HTTPS (DoH), DNS over TLS (DoT), and UDP DNS.
// Use one of these options:
//
//	-doh https://resolver.example/dns-query
//	-dot resolver.example:853
//	-udp resolver.example:53
//
// You can give the server's public key as a file or as a hex string. Use
// "dnstt-server -gen-key" to get the public key.
//
//	-pubkey-file server.pub
//	-pubkey 0000111122223333444455556666777788889999aaaabbbbccccddddeeeeffff
//
// DOMAIN is the root of the DNS zone reserved for the tunnel. See README for
// instructions on setting it up.
//
// LOCALADDR is the TCP address that will listen for connections and forward
// them over the tunnel.
//
// In -doh and -dot modes, the program's TLS fingerprint is camouflaged with
// uTLS by default. The specific TLS fingerprint is selected randomly from a
// weighted distribution. You can set your own distribution (or specific single
// fingerprint) using the -utls option. The special value "none" disables uTLS.
//
//	-utls '3*Firefox,2*Chrome,1*iOS'
//	-utls Firefox
//	-utls none
package dnsttclient

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	shellwords "github.com/mattn/go-shellwords"
	utls "github.com/refraction-networking/utls"
	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
	"www.bamsoftware.com/git/dnstt.git/dns"
	"www.bamsoftware.com/git/dnstt.git/noise"
	"www.bamsoftware.com/git/dnstt.git/turbotunnel"
)

// smux streams will be closed after this much time without receiving data.
const idleTimeout = 2 * time.Minute

// dnsNameCapacity returns the number of bytes remaining for encoded data after
// including domain in a DNS name.
func dnsNameCapacity(domain dns.Name) int {
	// Names must be 255 octets or shorter in total length.
	// https://tools.ietf.org/html/rfc1035#section-2.3.4
	capacity := 255
	// Subtract the length of the null terminator.
	capacity -= 1
	for _, label := range domain {
		// Subtract the length of the label and the length octet.
		capacity -= len(label) + 1
	}
	// Each label may be up to 63 bytes long and requires 64 bytes to
	// encode.
	capacity = capacity * 63 / 64
	// Base32 expands every 5 bytes to 8.
	capacity = capacity * 5 / 8
	return capacity
}

// readKeyFromFile reads a key from a named file.
func readKeyFromFile(filename string) ([]byte, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return noise.ReadKey(f)
}

// sampleUTLSDistribution parses a weighted uTLS Client Hello ID distribution
// string of the form "3*Firefox,2*Chrome,1*iOS", matches each label to a
// utls.ClientHelloID from utlsClientHelloIDMap, and randomly samples one
// utls.ClientHelloID from the distribution.
func sampleUTLSDistribution(spec string) (*utls.ClientHelloID, error) {
	weights, labels, err := parseWeightedList(spec)
	if err != nil {
		return nil, err
	}
	ids := make([]*utls.ClientHelloID, 0, len(labels))
	for _, label := range labels {
		var id *utls.ClientHelloID
		if label == "none" {
			id = nil
		} else {
			id = utlsLookup(label)
			if id == nil {
				return nil, fmt.Errorf("unknown TLS fingerprint %q", label)
			}
		}
		ids = append(ids, id)
	}
	return ids[sampleWeighted(weights)], nil
}

func handle(logger Logger, local *net.TCPConn, sess *smux.Session, conv uint32) error {
	stream, err := sess.OpenStream()
	if err != nil {
		logger.Log("session " + fmt.Sprint(conv) + " opening stream: " + err.Error())
		return fmt.Errorf("session %08x opening stream: %v", conv, err)
	}
	defer func() {
		logger.Log("end stream " + fmt.Sprint(conv) + ":" + fmt.Sprint(stream.ID()))
		log.Printf("end stream %08x:%d", conv, stream.ID())
		stream.Close()
	}()
	logger.Log("begin stream " + fmt.Sprint(conv) + ":" + fmt.Sprint(stream.ID()))
	log.Printf("begin stream %08x:%d", conv, stream.ID())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := io.Copy(stream, local)
		if err == io.EOF {
			// smux Stream.Write may return io.EOF.
			err = nil
		}
		if err != nil && !errors.Is(err, io.ErrClosedPipe) {
			logger.Log("stream " + fmt.Sprint(conv) + ":" + fmt.Sprint(stream.ID()) + " copy local←stream: " + err.Error())
			log.Printf("stream %08x:%d copy stream←local: %v", conv, stream.ID(), err)
		}
		local.CloseRead()
		stream.Close()
	}()
	go func() {
		defer wg.Done()
		_, err := io.Copy(local, stream)
		if err == io.EOF {
			// smux Stream.WriteTo may return io.EOF.
			err = nil
		}
		if err != nil && !errors.Is(err, io.ErrClosedPipe) {
			logger.Log("stream " + fmt.Sprint(conv) + ":" + fmt.Sprint(stream.ID()) + " copy local←stream: " + err.Error())
			log.Printf("stream %08x:%d copy local←stream: %v", conv, stream.ID(), err)
		}
		local.CloseWrite()
	}()
	wg.Wait()

	return err
}

func run(logger Logger, pubkey []byte, domain dns.Name, localAddr *net.TCPAddr, remoteAddr net.Addr, pconn net.PacketConn) error {
	defer pconn.Close()
	var err error

	ln, err = net.ListenTCP("tcp", localAddr)
	if err != nil {
		logger.Log("opening local listener: " + err.Error())
		return fmt.Errorf("opening local listener: %v", err)
	}
	defer ln.Close()

	mtu := dnsNameCapacity(domain) - 8 - 1 - numPadding - 1 // clientid + padding length prefix + padding + data length prefix
	if mtu < 80 {
		logger.Log("domain " + fmt.Sprint(domain) + " leaves only " + fmt.Sprint(mtu) + " bytes for payload")
		return fmt.Errorf("domain %s leaves only %d bytes for payload", domain, mtu)
	}
	logger.Log("effective MTU " + fmt.Sprint(mtu))
	log.Printf("effective MTU %d", mtu)

	// Open a KCP conn on the PacketConn.
	conn, err = kcp.NewConn2(remoteAddr, nil, 0, 0, pconn)
	if err != nil {
		logger.Log("opening KCP conn: " + err.Error())
		return fmt.Errorf("opening KCP conn: %v", err)
	}
	defer func() {
		logger.Log("end session " + fmt.Sprint(conn.GetConv()))
		log.Printf("end session %08x", conn.GetConv())
		conn.Close()
	}()
	logger.Log("begin session " + fmt.Sprint(conn.GetConv()))
	log.Printf("begin session %08x", conn.GetConv())
	// Permit coalescing the payloads of consecutive sends.
	conn.SetStreamMode(true)
	// Disable the dynamic congestion window (limit only by the maximum of
	// local and remote static windows).
	conn.SetNoDelay(
		0, // default nodelay
		0, // default interval
		0, // default resend
		1, // nc=1 => congestion window off
	)
	conn.SetWindowSize(turbotunnel.QueueSize/2, turbotunnel.QueueSize/2)
	if rc := conn.SetMtu(mtu); !rc {
		panic(rc)
	}

	// Put a Noise channel on top of the KCP conn.
	rw, err = noise.NewClient(conn, pubkey)
	if err != nil {
		logger.Log(err.Error())
		return err
	}

	// Start a smux session on the Noise channel.
	smuxConfig := smux.DefaultConfig()
	smuxConfig.Version = 2
	smuxConfig.KeepAliveTimeout = idleTimeout
	smuxConfig.MaxStreamBuffer = 1 * 1024 * 1024 // default is 65536
	sess, err = smux.Client(rw, smuxConfig)
	if err != nil {
		logger.Log("opening smux session: " + err.Error())
		return fmt.Errorf("opening smux session: %v", err)
	}
	defer sess.Close()

	for {
		local, err := ln.Accept()
		if err != nil {
			if err, ok := err.(net.Error); ok && err.Temporary() {
				continue
			}
			return err
		}
		go func() {
			defer local.Close()
			err := handle(logger, local.(*net.TCPConn), sess, conn.GetConv())
			if err != nil {
				logger.Log("handle: " + err.Error())
				log.Printf("handle: %v", err)
			}
		}()
	}
}

func Start(logger Logger, args []string) error {
	flag := flag.NewFlagSet("client", flag.ContinueOnError)
	var dohURL string
	var dotAddr string
	var pubkeyFilename string
	var pubkeyString string
	var udpAddr string
	var utlsDistribution string

	flag.StringVar(&dohURL, "doh", "", "URL of DoH resolver")
	flag.StringVar(&dotAddr, "dot", "", "address of DoT resolver")
	flag.StringVar(&pubkeyString, "pubkey", "", fmt.Sprintf("server public key (%d hex digits)", noise.KeyLen*2))
	flag.StringVar(&pubkeyFilename, "pubkey-file", "", "read server public key from file")
	flag.StringVar(&udpAddr, "udp", "", "address of UDP DNS resolver")
	flag.StringVar(&utlsDistribution, "utls",
		"3*Firefox_65,1*Firefox_63,1*iOS_12_1",
		"choose TLS fingerprint from weighted distribution")
	flag.Parse(args)

	log.SetFlags(log.LstdFlags | log.LUTC)

	domain, err := dns.ParseName(flag.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid domain %+q: %v\n", flag.Arg(0), err)
		logger.Log("invalid domain " + flag.Arg(0) + ":" + err.Error())
		return errors.New("invalid domain " + flag.Arg(0) + ":" + err.Error())
	}
	localAddr, err := net.ResolveTCPAddr("tcp", flag.Arg(1))
	if err != nil {
		return err
	}

	var pubkey []byte
	if pubkeyFilename != "" && pubkeyString != "" {
		logger.Log("only one of -pubkey and -pubkey-file may be used")
		return errors.New("only one of -pubkey and -pubkey-file may be used")
	} else if pubkeyFilename != "" {
		var err error
		pubkey, err = readKeyFromFile(pubkeyFilename)
		if err != nil {
			logger.Log("cannot read pubkey from file: " + err.Error())
			return errors.New("cannot read pubkey from file: " + err.Error())
		}
	} else if pubkeyString != "" {
		var err error
		pubkey, err = noise.DecodeKey(pubkeyString)
		if err != nil {
			logger.Log("pubkey format error: " + err.Error())
			return errors.New("pubkey format error: " + err.Error())
		}
	}
	if len(pubkey) == 0 {
		fmt.Fprintf(os.Stderr, "the -pubkey or -pubkey-file option is required\n")
		logger.Log("the -pubkey or -pubkey-file option is required")
		return errors.New("the -pubkey or -pubkey-file option is required")
	}

	utlsClientHelloID, err := sampleUTLSDistribution(utlsDistribution)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parsing -utls: %v\n", err)
		logger.Log("parsing -utls: " + err.Error())
		return errors.New("parsing -utls: " + err.Error())
	}

	if utlsClientHelloID != nil {
		logger.Log("uTLS fingerprint " + utlsClientHelloID.Client + " " + utlsClientHelloID.Version)
		log.Printf("uTLS fingerprint %s %s", utlsClientHelloID.Client, utlsClientHelloID.Version)
	}

	// Iterate over the remote resolver address options and select one and
	// only one.
	var remoteAddr net.Addr
	var pconn net.PacketConn
	for _, opt := range []struct {
		s string
		f func(string) (net.Addr, net.PacketConn, error)
	}{
		// -doh
		{dohURL, func(s string) (net.Addr, net.PacketConn, error) {
			addr := turbotunnel.DummyAddr{}
			var rt http.RoundTripper
			if utlsClientHelloID == nil {
				transport := http.DefaultTransport.(*http.Transport).Clone()
				// Disable DefaultTransport's default Proxy =
				// ProxyFromEnvironment setting, for conformity
				// with utlsRoundTripper and with DoT mode,
				// which do not take a proxy from the
				// environment.
				transport.Proxy = nil
				rt = transport
			} else {
				rt = NewUTLSRoundTripper(nil, utlsClientHelloID)
			}
			pconn, err := NewHTTPPacketConn(rt, dohURL, 32)
			return addr, pconn, err
		}},
		// -dot
		{dotAddr, func(s string) (net.Addr, net.PacketConn, error) {
			addr := turbotunnel.DummyAddr{}
			var dialTLSContext func(ctx context.Context, network, addr string) (net.Conn, error)
			if utlsClientHelloID == nil {
				dialTLSContext = (&tls.Dialer{}).DialContext
			} else {
				dialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					return utlsDialContext(ctx, network, addr, nil, utlsClientHelloID)
				}
			}
			pconn, err := NewTLSPacketConn(dotAddr, dialTLSContext)
			return addr, pconn, err
		}},
		// -udp
		{udpAddr, func(s string) (net.Addr, net.PacketConn, error) {
			addr, err := net.ResolveUDPAddr("udp", s)
			if err != nil {
				logger.Log(err.Error())
				return nil, nil, err
			}
			pconn, err := net.ListenUDP("udp", nil)
			return addr, pconn, err
		}},
	} {
		if opt.s == "" {
			continue
		}
		if pconn != nil {
			logger.Log("only one of -doh, -dot, and -udp may be given")
			return errors.New("only one of -doh, -dot, and -udp may be given\n")
		}
		var err error
		remoteAddr, pconn, err = opt.f(opt.s)
		if err != nil {
			return err
		}
	}
	if pconn == nil {
		logger.Log("one of -doh, -dot, or -udp is required")
		return errors.New("one of -doh, -dot, or -udp is required")
	}

	pconn = NewDNSPacketConn(pconn, remoteAddr, domain)
	err = run(logger, pubkey, domain, localAddr, remoteAddr, pconn)
	if err != nil {
		return err
	}

	return nil
}

func ValidPubKey(pubkeyString string) bool {
	pubkey, err := noise.DecodeKey(pubkeyString)
	if err != nil {
		return false
	}
	pubkey = nil
	_ = pubkey
	return true
}

var ln *net.TCPListener
var conn *kcp.UDPSession
var rw io.ReadWriteCloser
var sess *smux.Session
var isRunning = false

func StopDnstt(logger Logger) error {
	logger.Log("Stopping Dnstt")
	var err error
	isRunning = false

	if sess != nil {
		err = sess.Close()
		if err != nil {
			logger.Log(err.Error())
			return err
		}
	}
	if rw != nil {
		err = rw.Close()
		if err != nil {
			logger.Log(err.Error())
			return err
		}
	}
	if conn != nil {
		err = conn.Close()
		if err != nil {
			logger.Log(err.Error())
			return err
		}
	}
	if ln != nil {
		err = ln.Close()
		if err != nil {
			logger.Log(err.Error())
			return err
		}
	}

	return err
}

func StartDnstt(logger Logger, str string) error {
	logger.Log("Starting Dnstt")

	if isRunning {
		logger.Log("Dnstt is still running")
		StopDnstt(logger)
	}

	var args []string
	var err error

	isRunning = true
	args, err = shellwords.Parse(str)
	if err != nil {
		return err
	}
	err = Start(logger, args)

	return err
}

type Logger interface {
	Log(str string)
}
