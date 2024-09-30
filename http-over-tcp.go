package httproxytcp

import (
	"bufio"
	"bytes"
	"fmt"
	cmap "github.com/orcaman/concurrent-map/v2"
	"github.com/thanhkaiba/httproxytcp/utils"
	"io"
	logger "log"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

type HTTPOverTCP struct {
	cfg             HTTPArgs
	proxy           HttpProxy // a http proxy to handle http traffic
	lockChn         chan bool
	isStop          bool
	log             *logger.Logger
	serverChannels  []*utils.ServerChannel
	userConnections cmap.ConcurrentMap[string, *net.Conn]
}

func NewHTTPProxyOverTCP() *HTTPOverTCP {
	return &HTTPOverTCP{
		cfg:             HTTPArgs{},
		lockChn:         make(chan bool, 1),
		isStop:          false,
		serverChannels:  []*utils.ServerChannel{},
		userConnections: cmap.New[*net.Conn](),
	}
}

func (s *HTTPOverTCP) StopService() {
	defer func() {
		e := recover()
		if e != nil {
			s.log.Printf("stop http(s) service crashed,%s", e)
		} else {
			s.log.Printf("service http(s) stoped")
		}
	}()
	s.isStop = true
}
func (s *HTTPOverTCP) Start(args HTTPArgs, proxy HttpProxy, log *logger.Logger) (err error) {
	s.log = log
	s.cfg = args
	s.proxy = proxy

	for _, addr := range strings.Split(s.cfg.Local, ",") {
		if addr != "" {
			host, port, _ := net.SplitHostPort(addr)
			p, _ := strconv.Atoi(port)
			sc := utils.NewServerChannel(host, p, s.log)
			err = sc.ListenTCP(s.callback)
			if err != nil {
				return
			}
			s.log.Printf("http proxy on %s", (*sc.Listener).Addr())
			s.serverChannels = append(s.serverChannels, &sc)
		}
	}
	return
}

type hijackableResponseWriter struct {
	inConn     net.Conn
	buf        *bufio.ReadWriter
	header     http.Header
	status     int
	written    bool
	isHijacked bool
}

func (w *hijackableResponseWriter) Header() http.Header {
	return w.header
}

func (w *hijackableResponseWriter) WriteHeader(status int) {
	if w.written {
		return
	}
	w.status = status
	w.written = true
}

func (w *hijackableResponseWriter) Write(b []byte) (int, error) {
	if !w.written {
		w.WriteHeader(http.StatusOK)
	}
	return w.buf.Write(b)
}

func (w *hijackableResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.isHijacked {
		return nil, nil, fmt.Errorf("connection already hijacked")
	}
	w.isHijacked = true
	return w.inConn, w.buf, nil
}

func (s *HTTPOverTCP) Clean() {
	s.StopService()
}
func (s *HTTPOverTCP) callback(inConn net.Conn) {
	defer func() {
		if err := recover(); err != nil {
			s.log.Printf("http(s) conn handler crashed with err : %s \nstack: %s", err, string(debug.Stack()))
		}
	}()
	s.check(inConn)

	req, err := utils.NewHTTPRequest(&inConn, 4096, s.log)
	if err != nil {
		if err != io.EOF {
			s.log.Printf("decoder error , from %s, ERR:%s", inConn.RemoteAddr(), err)
		}
		utils.CloseConn(&inConn)
		return
	}

	if req.Method != "SNI" {
		// Convert utils.HTTPRequest to http.Request
		httpReq, err := s.convertToHTTPRequest(&req)
		if err != nil {
			s.log.Printf("failed to convert to http.Request: %v", err)
			utils.CloseConn(&inConn)
			return
		}

		// Create a ResponseWriter and serve the request using goproxy
		w := &hijackableResponseWriter{
			inConn:     inConn,
			buf:        bufio.NewReadWriter(bufio.NewReader(inConn), bufio.NewWriter(inConn)),
			header:     make(http.Header),
			isHijacked: false,
		}
		s.proxy.ServeHTTP(w, httpReq)
	} else {
		// Create the CONNECT request
		connectReq, err := http.NewRequest("CONNECT", req.Host, nil)
		if err != nil {
			s.log.Printf("failed to create CONNECT request: %v", err)
			utils.CloseConn(&inConn)
			return
		}

		connectReq.URL, _ = url.Parse(req.URL)
		connectReq.Host = req.Host
		connectReq.RemoteAddr = inConn.RemoteAddr().String()
		connectReq.Header.Set("Host", req.Host)

		// Create a ResponseWriter and serve the CONNECT request using goproxy
		w := &hijackableResponseWriter{
			inConn:     inConn,
			buf:        bufio.NewReadWriter(bufio.NewReader(inConn), bufio.NewWriter(inConn)),
			header:     make(http.Header),
			isHijacked: false,
		}

		// Pass the CONNECT request to the proxy for handling
		s.proxy.ServeHTTP(w, connectReq)
	}
}

func (s *HTTPOverTCP) convertToHTTPRequest(req *utils.HTTPRequest) (*http.Request, error) {
	reader := bufio.NewReader(bytes.NewReader(req.HeadBuf))
	httpReq, err := http.ReadRequest(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read http request: %v", err)
	}
	httpReq.RemoteAddr = (*req.Conn).RemoteAddr().String()
	return httpReq, nil
}

func (s *HTTPOverTCP) OutToTCP(useProxy bool, address string, inConn *net.Conn, req *utils.HTTPRequest) (lbAddr string, err interface{}) {
	inAddr := (*inConn).RemoteAddr().String()
	inLocalAddr := (*inConn).LocalAddr().String()
	//防止死循环
	if s.IsDeadLoop(inLocalAddr, req.Host) {
		utils.CloseConn(inConn)
		err = fmt.Errorf("dead loop detected , %s", req.Host)
		return
	}
	var outConn net.Conn
	tryCount := 0
	maxTryCount := 5
	for {
		if s.isStop {
			return
		}
		if useProxy {
			outConn, err = s.GetParentConn(address)
		} else {
			outConn, err = utils.ConnectHost(address, s.cfg.Timeout)
		}
		tryCount++
		if err == nil || tryCount > maxTryCount {
			break
		} else {
			s.log.Printf("connect to %s , err:%s,retrying...", address, err)
			time.Sleep(time.Second * 2)
		}
	}
	if err != nil {
		s.log.Printf("connect to %s , err:%s", inAddr, err)
		utils.CloseConn(inConn)
		return
	}

	outAddr := outConn.RemoteAddr().String()
	//outLocalAddr := outConn.LocalAddr().String()
	if req.IsHTTPS() && !useProxy {
		//https无上级或者上级非代理,proxy需要响应connect请求,并直连目标
		err = req.HTTPSReply()
	} else {
		//https或者http,上级是代理,proxy需要转发
		outConn.SetDeadline(time.Now().Add(time.Millisecond * time.Duration(s.cfg.Timeout)))
		//直连目标或上级非代理或非SNI,,清理HTTP头部的代理头信息
		if !useProxy && !(req.Method == "SNI") {
			_, err = outConn.Write(utils.RemoveProxyHeaders(req.HeadBuf))
		} else {
			_, err = outConn.Write(req.HeadBuf)
		}
		outConn.SetDeadline(time.Time{})
		if err != nil {
			s.log.Printf("write to %s , err:%s", inAddr, err)
			utils.CloseConn(inConn)
			return
		}
	}

	utils.IoBind((*inConn), outConn, func(err interface{}) {
		s.log.Printf("conn %s - %s released [%s]", inAddr, outAddr, req.Host)
		s.userConnections.Remove(inAddr)
	}, s.log)
	s.log.Printf("conn %s - %s connected [%s]", inAddr, outAddr, req.Host)
	if c, ok := s.userConnections.Get(inAddr); ok {
		(*c).Close()
	}
	s.userConnections.Set(inAddr, inConn)
	return
}

func (s *HTTPOverTCP) GetParentConn(address string) (conn net.Conn, err error) {
	conn, err = utils.ConnectHost(address, s.cfg.Timeout)
	return
}

func (s *HTTPOverTCP) IsDeadLoop(inLocalAddr string, host string) bool {
	inIP, inPort, err := net.SplitHostPort(inLocalAddr)
	if err != nil {
		return false
	}
	outDomain, outPort, err := net.SplitHostPort(host)
	if err != nil {
		return false
	}
	if inPort == outPort {
		var outIPs []net.IP
		outIPs, err = net.LookupIP(outDomain)
		if err == nil {
			for _, ip := range outIPs {
				if ip.String() == inIP {
					return true
				}
			}
		}
		interfaceIPs, err := utils.GetAllInterfaceAddr()
		if err == nil {
			for _, localIP := range interfaceIPs {
				for _, outIP := range outIPs {
					if localIP.Equal(outIP) {
						return true
					}
				}
			}
		}
	}
	return false
}

func (s *HTTPOverTCP) GetDirectConn(address string, localAddr string) (conn net.Conn, err error) {
	return utils.ConnectHost(address, s.cfg.Timeout)
}

// SOCKADDR_IN representation (IPv4)
type SOCKADDR_IN struct {
	Family uint16 // AF_INET
	Port   uint16
	Addr   [4]byte
	_      [8]byte // Padding
}

// SOCKADDR_IN6 representation (IPv6)
type SOCKADDR_IN6 struct {
	Family   uint16 // AF_INET6
	Port     uint16
	FlowInfo uint32
	Addr     [16]byte
	ScopeID  uint32
}

// SOCKADDR_STORAGE is a generic structure
type SOCKADDR_STORAGE struct {
	Family uint16 // sa_family (2 bytes for address family)
	_      [126]byte
}

const (
	SIO_QUERY_WFP_CONNECTION_REDIRECT_CONTEXT = syscall.IOC_IN | syscall.IOC_VENDOR | 221
	ContextSize                               = 1024 // Adjust context size as needed
)

func (s *HTTPOverTCP) check(inConn net.Conn) {
	// Extract the file descriptor from the connection
	rawConn, err := inConn.(*net.TCPConn).SyscallConn()
	if err != nil {
		fmt.Println("Error getting raw connection:", err)
		return
	}

	var clientSock syscall.Handle
	if err = rawConn.Control(func(fd uintptr) {
		clientSock = syscall.Handle(fd)
	}); err != nil {
		fmt.Printf("Error accessing raw socket: %v\n", err)
		return
	}

	// Allocate redirect context buffer once
	redirectContext := make([]byte, ContextSize)
	var bytesReturned uint32

	// Call WSAIoctl with correct parameters
	err = syscall.WSAIoctl(
		clientSock,
		SIO_QUERY_WFP_CONNECTION_REDIRECT_CONTEXT,
		nil,
		0,
		&redirectContext[0],
		uint32(len(redirectContext)),
		&bytesReturned,
		nil,
		0,
	)

	if err != nil {
		s.log.Fatalf("WSAIoctl failed: %v\n", err)
	}

	const sockaddrStorageSize = uint32(unsafe.Sizeof(SOCKADDR_STORAGE{}))
	if bytesReturned < 2*sockaddrStorageSize {
		fmt.Printf("Insufficient data returned. Expected at least %d bytes but got %d.\n", 2*sockaddrStorageSize, bytesReturned)
		return
	}

	// Extract and handle the SOCKADDR_STORAGE entries
	firstSockAddr := (*SOCKADDR_STORAGE)(unsafe.Pointer(&redirectContext[0]))
	//secondSockAddr := (*SOCKADDR_STORAGE)(unsafe.Pointer(&redirectContext[sockaddrStorageSize]))

	fmt.Printf("WFP Redirect Context retrieved successfully. Bytes returned: %d\n", bytesReturned)

	// Encapsulated printIPAddress logic
	printIPAddress(firstSockAddr, "First")
	//printIPAddress(secondSockAddr, "Second")
}

// Helper function to print IP address
func printIPAddress(sockAddr *SOCKADDR_STORAGE, label string) {
	switch sockAddr.Family {
	case syscall.AF_INET:
		ipv4 := (*SOCKADDR_IN)(unsafe.Pointer(sockAddr))
		ip := net.IPv4(ipv4.Addr[0], ipv4.Addr[1], ipv4.Addr[2], ipv4.Addr[3])
		fmt.Printf("%s Address Family: AF_INET (IPv4) -> IP Address: %s\n", label, ip.String())
	case syscall.AF_INET6:
		ipv6 := (*SOCKADDR_IN6)(unsafe.Pointer(sockAddr))
		ip := net.IP(ipv6.Addr[:])
		fmt.Printf("%s Address Family: AF_INET6 (IPv6) -> IP Address: %s\n", label, ip.String())
	default:
		fmt.Printf("%s Address Family: Unknown (%d)\n", label, sockAddr.Family)
	}
}
