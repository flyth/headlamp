package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/prometheus/common/log"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// Needed for Port-Forwarding / Exec

const GroupName = ""

var SchemeGroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1"}

type k8sExecConn struct {
	io.Writer
	io.Reader
	exec       remotecommand.Executor
	target     string
	cancelFunc func()
}

type ProxyRequest struct {
	Namespace string   `json:"namespace"`
	Pod       string   `json:"pod"`
	Cluster   string   `json:"cluster"`
	Command   []string `json:"command"`
	Container string   `json:"container"`
}

func (r *ProxyRequest) URL() string {
	cmd, _ := json.Marshal(r.Command)
	return fmt.Sprintf("ws://%s.%s.%s.pod.%s/cmd/?cmd=%s", r.Container, r.Pod, r.Namespace, r.Cluster, url.QueryEscape(string(cmd)))
}

// ParseURL takes a request in the form of ws://CONTAINERNAME.PODNAME.NAMESPACE.pod.CLUSTERNAME/cmd/?cmd=[]
// and returns a ProxyRequest. The cmd parameter must be a JSON encoded array of strings.
func ParseURL(u string) (*ProxyRequest, error) {
	p, err := url.Parse(u)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL")
	}
	hostParts := strings.Split(p.Host, ".")
	if len(hostParts) != 5 {
		return nil, fmt.Errorf("invalid proxy host")
	}
	if hostParts[3] != "pod" {
		return nil, fmt.Errorf("not a valid container address")
	}

	var cmd []string
	if err := json.Unmarshal([]byte(p.Query().Get("cmd")), &cmd); err != nil {
		return nil, fmt.Errorf("invalid command")
	}

	return &ProxyRequest{
		Namespace: hostParts[2],
		Pod:       hostParts[1],
		Cluster:   hostParts[4],
		Command:   cmd,
		Container: hostParts[0],
	}, nil
}

func proxyWebSocket(clientConn, serverConn io.ReadWriteCloser) {
	log.Debugf("running")

	var wg sync.WaitGroup
	wg.Add(2)

	copy := func(dst io.Writer, src io.Reader, done chan<- bool) {
		_, _ = io.Copy(dst, src)
		done <- true
	}

	clientToServerDone := make(chan bool)
	serverToClientDone := make(chan bool)

	go func() {
		copy(serverConn, clientConn, clientToServerDone)
		wg.Done()
	}()

	go func() {
		copy(clientConn, serverConn, serverToClientDone)
		wg.Done()
	}()

	log.Debugf("done")

	wg.Wait()
	_ = clientConn.Close()
	_ = serverConn.Close()
}

func (c *HeadlampConfig) setupProxyRoutes(r *mux.Router) {
	r.HandleFunc("/proxy/ws", func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true // TODO: FIX, this allows CORS from anywhere
			},
		}
		var hdr http.Header
		clientConn, err := upgrader.Upgrade(w, r, hdr)
		if err != nil {
			log.Debugf("upgrading %v", err)
			http.Error(w, "Error upgrading client connection", http.StatusInternalServerError)
			return
		}

		defer clientConn.Close()

		target := r.URL.Query().Get("proxyInfo")
		log.Debugf("forwarding to %s", target)

		preq, err := ParseURL(target)
		if err != nil {
			log.Debugf("could not parse url: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}

		log.Debugf("%+v", preq)

		dialer := &websocket.Dialer{
			Proxy:            http.ProxyFromEnvironment,
			HandshakeTimeout: 45 * time.Second,
			NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return c.NewK8SExecConn(ctx, preq, time.Second*30)
			},
		}

		serverConn, _, err := dialer.Dial("ws://127.0.0.1/ws", nil)
		if err != nil {
			log.Debugf("connecting %v", err)
			http.Error(w, "Error connecting to target server", http.StatusBadGateway)
			return
		}
		defer serverConn.Close()

		// Block until done
		proxyWebSocket(clientConn.UnderlyingConn(), serverConn.UnderlyingConn())
	})
}

// NewK8SExecConn connects to a Pod using the Kubernetes API Server and launches a command
func (c *HeadlampConfig) NewK8SExecConn(ctx context.Context, p *ProxyRequest, timeout time.Duration) (net.Conn, error) {
	// Mostly copied from headlamp.go, might need to be adjusted
	ctxtProxy, ok := c.contextProxies[p.Cluster] // TODO: This map needs to be guarded by a mutex to be safe!
	if !ok {
		return nil, fmt.Errorf("cluster %s not found", p.Cluster)
	}
	var caData []byte
	var err error
	if caData, err = ctxtProxy.context.cluster.getCAData(); err != nil {
		return nil, fmt.Errorf("failed to get CA data: %v", err)
	}

	rConf := &rest.Config{
		Host:        ctxtProxy.context.cluster.config.Server,
		BearerToken: ctxtProxy.context.authInfo.Token,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caData,
		},
		APIPath: "/api",
	}

	if ctxtProxy.context.authInfo != nil {
		if ctxtProxy.context.authInfo.ClientKey != "" {
			rConf.TLSClientConfig.KeyFile = ctxtProxy.context.authInfo.ClientKey
		}

		if ctxtProxy.context.authInfo.ClientCertificate != "" {
			rConf.TLSClientConfig.CertFile = ctxtProxy.context.authInfo.ClientCertificate
		}

		if ctxtProxy.context.authInfo.ClientKeyData != nil {
			rConf.TLSClientConfig.KeyData = ctxtProxy.context.authInfo.ClientKeyData
		}

		if ctxtProxy.context.authInfo.ClientCertificateData != nil {
			rConf.TLSClientConfig.CertData = ctxtProxy.context.authInfo.ClientCertificateData
		}
	}
	// End of copy/paste

	// Necessary information for exec
	rConf.GroupVersion = &SchemeGroupVersion
	rConf.NegotiatedSerializer = serializer.WithoutConversionCodecFactory{CodecFactory: scheme.Codecs}

	// Set timeout as well
	rConf.Timeout = timeout

	readerExt, writer := io.Pipe()
	reader, writerExt := io.Pipe()
	conn := &k8sExecConn{
		Writer: writer,
		Reader: reader,
	}

	conn.target = fmt.Sprintf("pod://%s/%s", p.Cluster, p.Pod)

	restClient, err := rest.RESTClientFor(rConf)
	if err != nil {
		return nil, err
	}

	req := restClient.Post().
		Resource("pods").
		Name(p.Pod).
		Namespace(p.Namespace).
		SubResource("exec").
		Param("container", p.Container).
		VersionedParams(&v1.PodExecOptions{
			Container: p.Container,
			Command:   p.Command,
			Stdin:     true,
			Stdout:    true,
			Stderr:    false,
			TTY:       false,
		}, scheme.ParameterCodec)

	log.Debugf("url is %s", req.URL().String())

	exec, err := remotecommand.NewSPDYExecutor(rConf, "POST", req.URL())
	if err != nil {
		return nil, err
	}
	conn.exec = exec

	ctx, cancelFunc := context.WithCancel(context.Background())
	conn.cancelFunc = cancelFunc

	go func() {
		err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdin:             readerExt,
			Stdout:            writerExt,
			Stderr:            nil,
			Tty:               false,
			TerminalSizeQueue: nil,
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Debugf("connecting to command on pod %q: %v", p.Pod, err)
		}
	}()
	return conn, nil
}

func (k *k8sExecConn) Close() error {
	k.cancelFunc()
	return nil
}

func (k *k8sExecConn) LocalAddr() net.Addr {
	return nil
}

func (k *k8sExecConn) RemoteAddr() net.Addr {
	return nil
}

func (k *k8sExecConn) SetDeadline(t time.Time) error {
	return nil
}

func (k *k8sExecConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (k *k8sExecConn) SetWriteDeadline(t time.Time) error {
	return nil
}
