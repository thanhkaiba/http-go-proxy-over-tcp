package utils

import (
	"bytes"
	"fmt"
	"github.com/miekg/dns"
	cmap "github.com/orcaman/concurrent-map/v2"
	"github.com/thanhkaiba/httproxytcp/utils/sni"
	"io"
	logger "log"
	"net"
	"net/url"
	"strings"
	"time"
)

type HTTPRequest struct {
	HeadBuf   []byte
	Conn      *net.Conn
	Host      string
	Method    string
	URL       string
	hostOrURL string
	log       *logger.Logger
}

func NewHTTPRequest(inConn *net.Conn, bufSize int, log *logger.Logger, header ...[]byte) (req HTTPRequest, err error) {
	buf := make([]byte, bufSize)
	n := 0
	req = HTTPRequest{
		Conn: inConn,
		log:  log,
	}
	if header != nil && len(header) == 1 && len(header[0]) > 1 {
		buf = header[0]
		n = len(header[0])
	} else {
		n, err = (*inConn).Read(buf[:])
		if err != nil {
			if err != io.EOF {
				err = fmt.Errorf("http decoder read err:%s", err)
			}
			CloseConn(inConn)
			return
		}
	}

	req.HeadBuf = buf[:n]
	//fmt.Println(string(req.HeadBuf))
	//try sni
	serverName, err0 := sni.ServerNameFromBytes(req.HeadBuf)
	if err0 == nil {
		//sni success
		req.Method = "SNI"
		req.hostOrURL = "https://" + serverName + ":443"
	} else {
		//sni fail , try http
		index := bytes.IndexByte(req.HeadBuf, '\n')
		if index == -1 {
			err = fmt.Errorf("http decoder data line err:%s", SubStr(string(req.HeadBuf), 0, 50))
			CloseConn(inConn)
			return
		}
		fmt.Sscanf(string(req.HeadBuf[:index]), "%s%s", &req.Method, &req.hostOrURL)
	}
	if req.Method == "" || req.hostOrURL == "" {
		err = fmt.Errorf("http decoder data err:%s", SubStr(string(req.HeadBuf), 0, 50))
		CloseConn(inConn)
		return
	}
	req.Method = strings.ToUpper(req.Method)
	log.Printf("%s:%s", req.Method, req.hostOrURL)

	if req.IsHTTPS() {
		err = req.HTTPS()
	} else {
		err = req.HTTP()
	}
	return
}
func (req *HTTPRequest) HTTP() (err error) {
	req.URL = req.getHTTPURL()
	var u *url.URL
	u, err = url.Parse(req.URL)
	if err != nil {
		return
	}
	req.Host = u.Host
	req.addPortIfNot()
	return
}
func (req *HTTPRequest) HTTPS() (err error) {
	req.Host = req.hostOrURL
	req.addPortIfNot()
	return
}
func (req *HTTPRequest) HTTPSReply() (err error) {
	_, err = fmt.Fprint(*req.Conn, "HTTP/1.1 200 Connection established\r\n\r\n")
	return
}
func (req *HTTPRequest) IsHTTPS() bool {
	return req.Method == "CONNECT"
}

func (req *HTTPRequest) getHTTPURL() (URL string) {
	if !strings.HasPrefix(req.hostOrURL, "/") {
		return req.hostOrURL
	}
	_host := req.getHeader("host")
	if _host == "" {
		return
	}
	URL = fmt.Sprintf("http://%s%s", _host, req.hostOrURL)
	return
}
func (req *HTTPRequest) getHeader(key string) (val string) {
	key = strings.ToUpper(key)
	lines := strings.Split(string(req.HeadBuf), "\r\n")
	//log.Println(lines)
	for _, line := range lines {
		hline := strings.SplitN(strings.Trim(line, "\r\n "), ":", 2)
		if len(hline) == 2 {
			k := strings.ToUpper(strings.Trim(hline[0], " "))
			v := strings.Trim(hline[1], " ")
			if key == k {
				val = v
				return
			}
		}
	}
	return
}

func (req *HTTPRequest) addPortIfNot() (newHost string) {
	//newHost = req.Host
	port := "80"
	if req.IsHTTPS() {
		port = "443"
	}
	if (!strings.HasPrefix(req.Host, "[") && strings.Index(req.Host, ":") == -1) || (strings.HasPrefix(req.Host, "[") && strings.HasSuffix(req.Host, "]")) {
		//newHost = req.Host + ":" + port
		//req.headBuf = []byte(strings.Replace(string(req.headBuf), req.Host, newHost, 1))
		req.Host = req.Host + ":" + port
	}
	return
}

type OutConn struct {
	address string
	timeout int
}

func NewOutConn(address string, timeout int) (op OutConn) {
	return OutConn{
		address: address,
		timeout: timeout,
	}
}
func (op *OutConn) Get() (conn net.Conn, err error) {
	conn, err = ConnectHost(op.address, op.timeout)
	return
}

type DomainResolver struct {
	ttl         int
	dnsAddrress string
	data        cmap.ConcurrentMap[string, *DomainResolverItem]
	log         *logger.Logger
}
type DomainResolverItem struct {
	ip        string
	domain    string
	expiredAt int64
}

func NewDomainResolver(dnsAddrress string, ttl int, log *logger.Logger) DomainResolver {
	return DomainResolver{
		ttl:         ttl,
		dnsAddrress: dnsAddrress,
		data:        cmap.New[*DomainResolverItem](),
		log:         log,
	}
}
func (a *DomainResolver) MustResolve(address string) (ip string) {
	ip, _ = a.Resolve(address)
	return
}
func (a *DomainResolver) Resolve(address string) (ip string, err error) {
	domain := address
	port := ""
	fromCache := "false"
	defer func() {
		if port != "" {
			ip = net.JoinHostPort(ip, port)
		}
		a.log.Printf("dns:%s->%s,cache:%s", address, ip, fromCache)
		//a.PrintData()
	}()
	if strings.Contains(domain, ":") {
		domain, port, err = net.SplitHostPort(domain)
		if err != nil {
			return
		}
	}
	if net.ParseIP(domain) != nil {
		ip = domain
		fromCache = "ip ignore"
		return
	}
	item, ok := a.data.Get(domain)
	if ok {
		//log.Println("find ", domain)
		if (*item).expiredAt > time.Now().Unix() {
			ip = (*item).ip
			fromCache = "true"
			//log.Println("from cache ", domain)
			return
		}
	} else {
		item = &DomainResolverItem{
			domain: domain,
		}

	}
	c := new(dns.Client)
	c.DialTimeout = time.Millisecond * 5000
	c.ReadTimeout = time.Millisecond * 5000
	c.WriteTimeout = time.Millisecond * 5000
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(domain), dns.TypeA)
	m.RecursionDesired = true
	r, _, err := c.Exchange(m, a.dnsAddrress)
	if r == nil {
		return
	}
	if r.Rcode != dns.RcodeSuccess {
		err = fmt.Errorf(" *** invalid answer name %s after A query for %s", domain, a.dnsAddrress)
		return
	}
	for _, answer := range r.Answer {
		if answer.Header().Rrtype == dns.TypeA {
			info := strings.Fields(answer.String())
			if len(info) >= 5 {
				ip = info[4]
				_item := item
				(*_item).expiredAt = time.Now().Unix() + int64(a.ttl)
				(*_item).ip = ip
				a.data.Set(domain, item)
				return
			}
		}
	}
	return
}
func (a *DomainResolver) PrintData() {
	for k, item := range a.data.Items() {
		d := item
		a.log.Printf("%s:ip[%s],domain[%s],expired at[%d]\n", k, (*d).ip, (*d).domain, (*d).expiredAt)
	}
}
