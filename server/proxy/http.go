package proxy

import (
	"bufio"
	"crypto/tls"
	"github.com/cnlh/nps/bridge"
	"github.com/cnlh/nps/lib/common"
	"github.com/cnlh/nps/lib/conn"
	"github.com/cnlh/nps/lib/file"
	"github.com/cnlh/nps/lib/lg"
	"github.com/cnlh/nps/vender/github.com/astaxie/beego"
	"net/http"
	"net/http/httputil"
	"path/filepath"
	"strconv"
	"sync"
)

type httpServer struct {
	server
	httpPort  int //http端口
	httpsPort int //https监听端口
	pemPath   string
	keyPath   string
	stop      chan bool
}

func NewHttp(bridge *bridge.Bridge, c *file.Tunnel) *httpServer {
	httpPort, _ := beego.AppConfig.Int("httpProxyPort")
	httpsPort, _ := beego.AppConfig.Int("httpsProxyPort")
	pemPath := beego.AppConfig.String("pemPath")
	keyPath := beego.AppConfig.String("keyPath")
	return &httpServer{
		server: server{
			task:   c,
			bridge: bridge,
			Mutex:  sync.Mutex{},
		},
		httpPort:  httpPort,
		httpsPort: httpsPort,
		pemPath:   pemPath,
		keyPath:   keyPath,
		stop:      make(chan bool),
	}
}

func (s *httpServer) Start() error {
	var err error
	var http, https *http.Server
	if s.errorContent, err = common.ReadAllFromFile(filepath.Join(common.GetRunPath(), "web", "static", "page", "error.html")); err != nil {
		s.errorContent = []byte("easyProxy 404")
	}

	if s.httpPort > 0 {
		http = s.NewServer(s.httpPort)
		go func() {
			lg.Println("Start http listener, port is", s.httpPort)
			err := http.ListenAndServe()
			if err != nil {
				lg.Fatalln(err)
			}
		}()
	}
	if s.httpsPort > 0 {
		if !common.FileExists(s.pemPath) {
			lg.Fatalf("ssl certFile %s is not exist", s.pemPath)
		}
		if !common.FileExists(s.keyPath) {
			lg.Fatalf("ssl keyFile %s exist", s.keyPath)
		}
		https = s.NewServer(s.httpsPort)
		go func() {
			lg.Println("Start https listener, port is", s.httpsPort)
			err := https.ListenAndServeTLS(s.pemPath, s.keyPath)
			if err != nil {
				lg.Fatalln(err)
			}
		}()
	}
	select {
	case <-s.stop:
		if http != nil {
			http.Close()
		}
		if https != nil {
			https.Close()
		}
	}
	return nil
}

func (s *httpServer) Close() {
	s.stop <- true
}

func (s *httpServer) handleTunneling(w http.ResponseWriter, r *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}
	c, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	}
	s.process(conn.NewConn(c), r)
}

func (s *httpServer) process(c *conn.Conn, r *http.Request) {
	//多客户端域名代理
	var (
		isConn   = true
		lk       *conn.Link
		host     *file.Host
		tunnel   *conn.Conn
		lastHost *file.Host
		err      error
	)
	if host, err = file.GetCsvDb().GetInfoByHost(r.Host, r); err != nil {
		lg.Printf("the url %s %s can't be parsed!", r.Host, r.RequestURI)
		goto end
	} else {
		lastHost = host
	}
	for {
	start:
		if isConn {
			//流量限制
			if host.Client.Flow.FlowLimit > 0 && (host.Client.Flow.FlowLimit<<20) < (host.Client.Flow.ExportFlow+host.Client.Flow.InletFlow) {
				break
			}
			host.Client.Cnf.CompressDecode, host.Client.Cnf.CompressEncode = common.GetCompressType(host.Client.Cnf.Compress)
			//权限控制
			if err = s.auth(r, c, host.Client.Cnf.U, host.Client.Cnf.P); err != nil {
				break
			}
			lk = conn.NewLink(host.Client.GetId(), common.CONN_TCP, host.GetRandomTarget(), host.Client.Cnf.CompressEncode, host.Client.Cnf.CompressDecode, host.Client.Cnf.Crypt, c, host.Flow, nil, host.Client.Rate, nil)
			if tunnel, err = s.bridge.SendLinkInfo(host.Client.Id, lk, c.Conn.RemoteAddr().String()); err != nil {
				lg.Println(err)
				break
			}
			lk.Run(true)
			isConn = false
		} else {
			r, err = http.ReadRequest(bufio.NewReader(c))
			if err != nil {
				break
			}
			if host, err = file.GetCsvDb().GetInfoByHost(r.Host, r); err != nil {
				lg.Printf("the url %s %s is not found !", r.Host, r.RequestURI)
				break
			} else if host != lastHost {
				lastHost = host
				isConn = true
				goto start
			}
		}
		//根据设定，修改header和host
		common.ChangeHostAndHeader(r, host.HostChange, host.HeaderChange, c.Conn.RemoteAddr().String())
		b, err := httputil.DumpRequest(r, true)
		if err != nil {
			break
		}
		host.Flow.Add(len(b), 0)
		if _, err := tunnel.SendMsg(b, lk); err != nil {
			c.Close()
			break
		}
		<-lk.StatusCh
	}
end:
	if isConn {
		s.writeConnFail(c.Conn)
	} else {
		tunnel.SendMsg([]byte(common.IO_EOF), lk)
	}
	c.Close()
}

func (s *httpServer) NewServer(port int) *http.Server {
	return &http.Server{
		Addr: ":" + strconv.Itoa(port),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handleTunneling(w, r)
		}),
		// Disable HTTP/2.
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}
}
