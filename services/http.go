package services

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	logger "log"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/snail007/goproxy/utils"
)

type HTTP struct {
	cfg            HTTPArgs
	lockChn        chan bool
	isStop         bool
	log            *logger.Logger
	serverChannels []*utils.ServerChannel
	userConns      utils.ConcurrentMap
}

func NewHTTP() *HTTP {
	return &HTTP{
		cfg:            HTTPArgs{},
		lockChn:        make(chan bool, 1),
		isStop:         false,
		serverChannels: []*utils.ServerChannel{},
		userConns:      utils.NewConcurrentMap(),
	}
}

func (s *HTTP) StopService() {
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
func (s *HTTP) Start(args interface{}, log *logger.Logger) (err error) {
	s.log = log
	s.cfg = args.(HTTPArgs)

	for _, addr := range strings.Split(s.cfg.Local, ",") {
		if addr != "" {
			host, port, _ := net.SplitHostPort(addr)
			p, _ := strconv.Atoi(port)
			sc := utils.NewServerChannel(host, p, s.log)
			err = sc.ListenTCP(s.callback)
			if err != nil {
				return
			}
			s.log.Printf("%s http(s) proxy on %s", s.cfg.LocalType, (*sc.Listener).Addr())
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

func (s *HTTP) Clean() {
	s.StopService()
}
func (s *HTTP) callback(inConn net.Conn) {
	defer func() {
		if err := recover(); err != nil {
			s.log.Printf("http(s) conn handler crashed with err : %s \nstack: %s", err, string(debug.Stack()))
		}
	}()

	req, err := utils.NewHTTPRequest(&inConn, 4096, s.log)
	if err != nil {
		if err != io.EOF {
			s.log.Printf("decoder error , from %s, ERR:%s", inConn.RemoteAddr(), err)
		}
		utils.CloseConn(&inConn)
		return
	}

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
	s.cfg.Proxy.ServeHTTP(w, httpReq)
}

func (s *HTTP) convertToHTTPRequest(req *utils.HTTPRequest) (*http.Request, error) {
	reader := bufio.NewReader(bytes.NewReader(req.HeadBuf))
	httpReq, err := http.ReadRequest(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to read http request: %v", err)
	}
	httpReq.RemoteAddr = (*req.Conn).RemoteAddr().String()
	return httpReq, nil
}

func (s *HTTP) OutToTCP(address string, inConn *net.Conn, req *utils.HTTPRequest) (err interface{}) {
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
		outConn, err = utils.ConnectHost(address, s.cfg.Timeout)
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
	if req.IsHTTPS() {
		//https无上级或者上级非代理,proxy需要响应connect请求,并直连目标
		err = req.HTTPSReply()
	} else {
		//https或者http,上级是代理,proxy需要转发
		outConn.SetDeadline(time.Now().Add(time.Millisecond * time.Duration(s.cfg.Timeout)))
		//直连目标或上级非代理,清理HTTP头部的代理头信息
		_, err = outConn.Write(utils.RemoveProxyHeaders(req.HeadBuf))
		outConn.SetDeadline(time.Time{})
		if err != nil {
			s.log.Printf("write to %s , err:%s", inAddr, err)
			utils.CloseConn(inConn)
			return
		}
	}

	utils.IoBind((*inConn), outConn, func(err interface{}) {
		s.log.Printf("conn %s - %s released [%s]", inAddr, outAddr, req.Host)
		s.userConns.Remove(inAddr)
	}, s.log)
	s.log.Printf("conn %s - %s connected [%s]", inAddr, outAddr, req.Host)
	if c, ok := s.userConns.Get(inAddr); ok {
		(*c.(*net.Conn)).Close()
	}
	s.userConns.Set(inAddr, inConn)
	return
}

func (s *HTTP) IsDeadLoop(inLocalAddr string, host string) bool {
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
