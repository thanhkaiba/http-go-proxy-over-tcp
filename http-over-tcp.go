package httproxytcp

import (
	"fmt"
	cmap "github.com/orcaman/concurrent-map/v2"
	"github.com/thanhkaiba/httproxytcp/utils"
	"io"
	logger "log"
	"net"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

type HTTPOverTCP struct {
	outPool        utils.OutConn
	cfg            HTTPArgs
	lockChn        chan bool
	isStop         bool
	domainResolver utils.DomainResolver
	serverChannels []*utils.ServerChannel
	userConns      cmap.ConcurrentMap[string, *net.Conn]
	log            *logger.Logger
}

func NewHTTPProxyOverTCP() *HTTPOverTCP {
	return &HTTPOverTCP{
		outPool:        utils.OutConn{},
		cfg:            HTTPArgs{},
		lockChn:        make(chan bool, 1),
		isStop:         false,
		serverChannels: []*utils.ServerChannel{},
		userConns:      cmap.New[*net.Conn](),
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
	for _, sc := range s.serverChannels {
		if sc.Listener != nil && *sc.Listener != nil {
			(*sc.Listener).Close()
		}
	}
}
func (s *HTTPOverTCP) Start(args HTTPArgs, log *logger.Logger) (err error) {
	s.log = log
	s.cfg = args

	if s.cfg.Parent != "" {
		s.log.Printf("use http parent %s", s.cfg.Parent)
		s.InitOutConnPool()
	}

	for _, addr := range strings.Split(s.cfg.Local, ",") {
		if addr != "" {
			host, port, _ := net.SplitHostPort(addr)
			p, _ := strconv.Atoi(port)
			sc := utils.NewServerChannel(host, p, s.log)
			err = sc.ListenTCP(s.callback)
			if err != nil {
				return
			}
			s.log.Printf("http(s) proxy on %s", (*sc.Listener).Addr())
			s.serverChannels = append(s.serverChannels, &sc)
		}
	}
	return
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

	var err interface{}
	var req utils.HTTPRequest
	req, err = utils.NewHTTPRequest(&inConn, 4096, s.log)
	if err != nil {
		if err != io.EOF {
			s.log.Printf("decoder error , from %s, ERR:%s", inConn.RemoteAddr(), err)
		}
		utils.CloseConn(&inConn)
		return
	}
	address := req.Host
	host, _, _ := net.SplitHostPort(address)
	useProxy := s.cfg.Parent != "" && !utils.IsIternalIP(host)

	s.log.Printf("use proxy : %v, %s", useProxy, address)

	err = s.OutToTCP(useProxy, address, &inConn, &req)
	if err != nil {
		if s.cfg.Parent == "" {
			s.log.Printf("connect to %s fail, ERR:%s", address, err)
		} else {
			s.log.Printf("connect to parent %s fail", s.cfg.Parent)
		}
		utils.CloseConn(&inConn)
	}
}
func (s *HTTPOverTCP) OutToTCP(useProxy bool, address string, inConn *net.Conn, req *utils.HTTPRequest) (err interface{}) {
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
			outConn, err = s.outPool.Get()
		} else {
			outConn, err = utils.ConnectHost(s.Resolve(address), s.cfg.Timeout)
		}
		tryCount++
		if err == nil || tryCount > maxTryCount {
			break
		} else {
			s.log.Printf("connect to %s , err:%s,retrying...", s.cfg.Parent, err)
			time.Sleep(time.Second * 2)
		}
	}
	if err != nil {
		s.log.Printf("connect to %s , err:%s", s.cfg.Parent, err)
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
		//直连目标或上级非代理,清理HTTP头部的代理头信息
		if !useProxy {
			_, err = outConn.Write(utils.RemoveProxyHeaders(req.HeadBuf))
		} else {
			_, err = outConn.Write(req.HeadBuf)
		}
		outConn.SetDeadline(time.Time{})
		if err != nil {
			s.log.Printf("write to %s , err:%s", s.cfg.Parent, err)
			utils.CloseConn(inConn)
			return
		}
	}

	utils.IoBind(*inConn, outConn, func(err interface{}) {
		s.log.Printf("conn %s - %s released [%s]", inAddr, outAddr, req.Host)
		s.userConns.Remove(inAddr)
	}, s.log)
	s.log.Printf("conn %s - %s connected [%s]", inAddr, outAddr, req.Host)
	if c, ok := s.userConns.Get(inAddr); ok {
		(*c).Close()
	}
	s.userConns.Set(inAddr, inConn)
	return
}

func (s *HTTPOverTCP) InitOutConnPool() {
	s.outPool = utils.NewOutConn(
		s.Resolve(s.cfg.Parent),
		s.cfg.Timeout,
	)
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
		if s.cfg.DNSAddress != "" {
			outIPs = []net.IP{net.ParseIP(s.Resolve(outDomain))}
		} else {
			outIPs, err = net.LookupIP(outDomain)
		}
		outIPs, err = net.LookupIP(outDomain)
		if err == nil {
			for _, ip := range outIPs {
				if ip.String() == inIP {
					return true
				}
			}
		}
		interfaceIPs, err := utils.GetAllInterfaceAddr()
		/*for _, ip := range *s.cfg.LocalIPS {
			interfaceIPs = append(interfaceIPs, net.ParseIP(ip).To4())
		}*/

		interfaceIPs = append(interfaceIPs, net.ParseIP(s.cfg.Parent).To4())
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

func (s *HTTPOverTCP) Resolve(address string) string {
	if s.cfg.DNSAddress == "" {
		return address
	}
	ip, err := s.domainResolver.Resolve(address)
	if err != nil {
		s.log.Printf("dns error %s , ERR:%s", address, err)
	}
	return ip
}
