package services

import "github.com/elazarl/goproxy"

type HTTPArgs struct {
	Local       string
	HTTPTimeout int
	LocalType   string
	Timeout     int
	Proxy       *goproxy.ProxyHttpServer
}
