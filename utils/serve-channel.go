package utils

import (
	"fmt"
	logger "log"
	"net"
	"runtime/debug"
)

type ServerChannel struct {
	ip               string
	port             int
	Listener         *net.Listener
	UDPListener      *net.UDPConn
	errAcceptHandler func(err error)
	log              *logger.Logger
}

func NewServerChannel(ip string, port int, log *logger.Logger) ServerChannel {
	return ServerChannel{
		ip:   ip,
		port: port,
		log:  log,
		errAcceptHandler: func(err error) {
			log.Printf("accept error , ERR:%s", err)
		},
	}
}
func (sc *ServerChannel) ListenTCP(fn func(conn net.Conn)) (err error) {
	var l net.Listener
	l, err = net.Listen("tcp", fmt.Sprintf("%s:%d", sc.ip, sc.port))
	if err == nil {
		sc.Listener = &l
		go func() {
			defer func() {
				if e := recover(); e != nil {
					sc.log.Printf("ListenTCP crashed , err : %s , \ntrace:%s", e, string(debug.Stack()))
				}
			}()
			for {
				var conn net.Conn
				conn, err = (*sc.Listener).Accept()
				if err == nil {
					go func() {
						defer func() {
							if e := recover(); e != nil {
								sc.log.Printf("tcp connection handler crashed , err : %s , \ntrace:%s", e, string(debug.Stack()))
							}
						}()
						fn(conn)
					}()
				} else {
					sc.errAcceptHandler(err)
					break
				}
			}
		}()
	}
	return
}
