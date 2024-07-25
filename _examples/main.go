package main

import (
	"github.com/elazarl/goproxy"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

func main() {

	proxy := goproxy.NewProxyHttpServer()

	proxy.OnRequest(goproxy.DstHostIs("www.reddit.com:443")).DoFunc(
		func(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
			return r, goproxy.NewResponse(r,
				goproxy.ContentTypeText, http.StatusForbidden,
				"Don't waste your time!")
		})
	proxy.OnRequest().HandleConnect(goproxy.AlwaysMitm)
	p := NewHTTP()
	p.Start(HTTPArgs{
		Local:       ":8284",
		HTTPTimeout: 30,
		Timeout:     2000,
	}, proxy, log.New(os.Stdout, "\r\n", log.LstdFlags))
	Clean(p)
}
func Clean(s *HTTPOverTCP) {
	signalChan := make(chan os.Signal, 1)
	cleanupDone := make(chan bool)
	signal.Notify(signalChan,
		os.Interrupt,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	go func() {
		for _ = range signalChan {
			log.Println("Received an interrupt, stopping services...")
			if s != nil {
				s.Clean()
			}
			cleanupDone <- true
		}
	}()
	<-cleanupDone
}
