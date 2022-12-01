package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/armon/go-socks5"
	"github.com/inverse-inc/go-utils/sharedutils"
	"github.com/inverse-inc/packetfence/go/chisel/share/cio"
	"github.com/inverse-inc/packetfence/go/chisel/share/cnet"
	"github.com/inverse-inc/packetfence/go/chisel/share/settings"
	"github.com/inverse-inc/packetfence/go/chisel/share/tunnel/radius_proxy"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// Config a Tunnel
type Config struct {
	*cio.Logger
	Inbound      bool
	Outbound     bool
	Socks        bool
	RadiusSecret string
	KeepAlive    time.Duration
	// The source IP for the packets that come into the remote
	SrcIP net.IP
}

// Tunnel represents an SSH tunnel with proxy capabilities.
// Both chisel client and server are Tunnels.
// chisel client has a single set of remotes, whereas
// chisel server has multiple sets of remotes (one set per client).
// Each remote has a 1:1 mapping to a proxy.
// Proxies listen, send data over ssh, and the other end of the ssh connection
// communicates with the endpoint and returns the response.
type Tunnel struct {
	Config
	//ssh connection
	activeConnMut  sync.RWMutex
	activatingConn waitGroup
	activeConn     ssh.Conn
	//proxies
	proxyCount int
	//internals
	connStats   cnet.ConnCount
	socksServer *socks5.Server

	connectionCtx context.Context

	IsRemoteConnector bool
	ConnectorID       string
	radiusProxy       *radius_proxy.Proxy
	k8ControllerDrop  chan struct{}
}

// New Tunnel from the given Config
func New(c Config) *Tunnel {
	c.Logger = c.Logger.Fork("tun")
	t := &Tunnel{
		Config: c,
	}
	radiusProxy, stop, err := radiusProxyFromKubernetes(t)

	if err != nil {
		t.Infof("Error getting pod info: %s", err.Error())
	} else {
		t.radiusProxy = radiusProxy
		t.k8ControllerDrop = stop
		go radiusProxy.Cleanup(stop)
		t.Infof("Radius Proxy setup is done")
	}

	t.activatingConn.Add(1)
	//setup socks server (not listening on any port!)
	extra := ""
	if c.Socks {
		sl := log.New(ioutil.Discard, "", 0)
		if t.Logger.Debug {
			sl = log.New(os.Stdout, "[socks]", log.Ldate|log.Ltime)
		}
		t.socksServer, _ = socks5.New(&socks5.Config{Logger: sl})
		extra += " (SOCKS enabled)"
	}
	t.Debugf("Created%s", extra)
	return t
}

func isPodReady(pod *v1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}

	for _, cond := range pod.Status.Conditions {
		if cond.Type == v1.PodReady {
			return cond.Status == v1.ConditionTrue
		}
	}

	return false
}

const radiusAuthK8Filter = "app=radiusd-auth"

func clientSetFromEnv() (*kubernetes.Clientset, error) {
	host := os.Getenv("K8S_MASTER_URI")
	if host == "" {
		return nil, errors.New("K8_MASTER_URI is not defined")
	}

	token := os.Getenv("K8S_MASTER_TOKEN")
	if token == "" {
		return nil, errors.New("K8_MASTER_TOKEN is not defined")
	}

	return kubernetes.NewForConfigAndClient(
		&rest.Config{
			Host:        host,
			BearerToken: token,
		},
		&http.Client{
			Transport: &http.Transport{
				TLSClientConfig: TLSConfigFromEnv(),
			},
		},
	)
}

func radiusProxyFromKubernetes(t *Tunnel) (*radius_proxy.Proxy, chan struct{}, error) {
	clientset, err := clientSetFromEnv()
	if err != nil {
		return nil, nil, err
	}

	data, err := os.ReadFile(os.Getenv("K8S_NAMESPACE_PATH"))
	if err != nil {
		return nil, nil, err
	}

	namespace := string(data)
	pods, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: radiusAuthK8Filter})
	if err != nil {
		return nil, nil, err
	}

	servers := []string{}
	for _, p := range pods.Items {
		servers = append(servers, p.Status.PodIP+":1812")
	}

	radiusProxy := radius_proxy.NewProxy(
		&radius_proxy.ProxyConfig{
			Secret:         []byte(t.Config.RadiusSecret),
			Addrs:          servers,
			SessionTimeout: 20 * time.Second,
			Logger:         t.Logger,
		},
	)

	watchlist := cache.NewFilteredListWatchFromClient(
		clientset.CoreV1().RESTClient(),
		string(v1.ResourcePods),
		namespace,
		func(opts *metav1.ListOptions) {
			opts.LabelSelector = radiusAuthK8Filter
		},
	)

	_, controller := cache.NewInformer( // also take a look at NewSharedIndexInformer
		watchlist,
		&v1.Pod{},
		0, //Duration is int64
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				if isPodReady(pod) {
					address := pod.Status.PodIP + ":1812"
					t.Infof("Adding %s", address)
					radiusProxy.AddBackend(address)
					return
				}
			},
			DeleteFunc: func(obj interface{}) {
				pod := obj.(*v1.Pod)
				address := pod.Status.PodIP + ":1812"
				t.Infof("Removing %s", address)
				radiusProxy.DeleteBackend(address)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				pod := newObj.(*v1.Pod)
				if isPodReady(pod) {
					address := pod.Status.PodIP + ":1812"
					t.Infof("Adding %s", address)
					radiusProxy.AddBackend(address)
					return
				}

				if pod.DeletionTimestamp != nil {
					address := pod.Status.PodIP + ":1812"
					t.Infof("%s is terminating removing", address)
					radiusProxy.DeleteBackend(address)
				}
			},
		},
	)
	stop := make(chan struct{})
	go controller.Run(stop)

	return radiusProxy, stop, nil
}

func TLSConfigFromEnv_() rest.TLSClientConfig {
	caFile := sharedutils.EnvOrDefault("K8S_MASTER_CA_FILE", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	return rest.TLSClientConfig{
		CAFile: caFile,
	}
}

func TLSConfigFromEnv() *tls.Config {
	caCerts := []byte(sharedutils.ReadFromFileOrStr(sharedutils.EnvOrDefault("KUBERNETES_CA_PATH", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")))
	rootCAs, _ := x509.SystemCertPool()
	if rootCAs == nil {
		rootCAs = x509.NewCertPool()
	}

	if ok := rootCAs.AppendCertsFromPEM(caCerts); !ok {
		fmt.Println("No K8S CA cert appended, using system certs only")
	}

	return &tls.Config{
		RootCAs: rootCAs,
	}
}

// BindSSH provides an active SSH for use for tunnelling
func (t *Tunnel) BindSSH(ctx context.Context, c ssh.Conn, reqs <-chan *ssh.Request, chans <-chan ssh.NewChannel) error {
	//link ctx to ssh-conn
	t.connectionCtx = ctx
	go func() {
		<-ctx.Done()
		if c.Close() == nil {
			t.Debugf("SSH cancelled")
		}
		t.activatingConn.DoneAll()
	}()
	//mark active and unblock
	t.activeConnMut.Lock()
	if t.activeConn != nil {
		panic("double bind ssh")
	}
	t.activeConn = c
	t.activeConnMut.Unlock()
	t.activatingConn.Done()
	//optional keepalive loop against this connection
	if t.Config.KeepAlive > 0 {
		go t.keepAliveLoop(c)
	}
	//block until closed
	go t.handleSSHRequests(reqs)
	go t.handleSSHChannels(chans)
	t.Debugf("SSH connected")
	err := c.Wait()
	t.Debugf("SSH disconnected")
	//mark inactive and block
	t.activatingConn.Add(1)
	t.activeConnMut.Lock()
	t.activeConn = nil
	t.activeConnMut.Unlock()
	return err
}

// getSSH blocks while connecting
func (t *Tunnel) getSSH(ctx context.Context) ssh.Conn {
	//cancelled already?
	if isDone(ctx) {
		return nil
	}
	t.activeConnMut.RLock()
	c := t.activeConn
	t.activeConnMut.RUnlock()
	//connected already?
	if c != nil {
		return c
	}
	//connecting...
	select {
	case <-ctx.Done(): //cancelled
		return nil
	case <-time.After(settings.EnvDuration("SSH_WAIT", 35*time.Second)):
		return nil //a bit longer than ssh timeout
	case <-t.activatingConnWait():
		t.activeConnMut.RLock()
		c := t.activeConn
		t.activeConnMut.RUnlock()
		return c
	}
}

func (t *Tunnel) activatingConnWait() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		t.activatingConn.Wait()
		close(ch)
	}()
	return ch
}

// Bind remotes that are tied to the context of the SSH connection
func (t *Tunnel) BindDynamicRemotes(remotes []*settings.Remote) error {
	return t.BindRemotes(t.connectionCtx, remotes)
}

// BindRemotes converts the given remotes into proxies, and blocks
// until the caller cancels the context or there is a proxy error.
func (t *Tunnel) BindRemotes(ctx context.Context, remotes []*settings.Remote) error {
	if len(remotes) == 0 {
		return errors.New("no remotes")
	}
	if !t.Inbound {
		return errors.New("inbound connections blocked")
	}
	proxies := make([]*Proxy, len(remotes))
	for i, remote := range remotes {
		p, err := NewProxy(t.Logger, t, t.proxyCount, remote)
		if err != nil {
			return err
		}
		proxies[i] = p
		t.proxyCount++
	}
	//TODO: handle tunnel close
	eg, ctx := errgroup.WithContext(ctx)
	for _, proxy := range proxies {
		p := proxy
		eg.Go(func() error {
			return p.Run(ctx)
		})
	}
	t.Debugf("Bound proxies")
	err := eg.Wait()
	t.Debugf("Unbound proxies")
	return err
}

func (t *Tunnel) keepAliveLoop(sshConn ssh.Conn) {
	//ping forever
	for {
		time.Sleep(t.Config.KeepAlive)
		_, b, err := sshConn.SendRequest("ping", true, nil)
		if err != nil {
			break
		}
		if len(b) > 0 && !bytes.Equal(b, []byte("pong")) {
			t.Debugf("strange ping response")
			break
		}
	}
	//close ssh connection on abnormal ping
	sshConn.Close()
}

func (t *Tunnel) IsActive() bool {
	return t.activeConn != nil
}
