package proxy

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/fatedier/frp/client/event"
	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/msg"
	"github.com/fatedier/frp/pkg/util/xlog"

	"github.com/fatedier/golib/errors"
)

// proxyManager：管理所有的 proxy
type Manager struct {
	sendCh  chan (msg.Message) //proxy 启动的时候，需要发送 remote_port 等信息给 frps，这些信息写入 sendCh， control 那边会读取 sendCh，通过 conn 发送给 frps
	proxies map[string]*Wrapper

	closed bool
	mu     sync.RWMutex

	clientCfg config.ClientCommonConf

	// The UDP port that the server is listening on
	serverUDPPort int

	ctx context.Context
}

func NewManager(ctx context.Context, msgSendCh chan (msg.Message), clientCfg config.ClientCommonConf, serverUDPPort int) *Manager {
	return &Manager{
		sendCh:        msgSendCh,
		proxies:       make(map[string]*Wrapper),
		closed:        false,
		clientCfg:     clientCfg,
		serverUDPPort: serverUDPPort,
		ctx:           ctx,
	}
}

func (pm *Manager) StartProxy(name string, remoteAddr string, serverRespErr string) error {
	pm.mu.RLock()
	pxy, ok := pm.proxies[name]
	pm.mu.RUnlock()
	if !ok {
		return fmt.Errorf("proxy [%s] not found", name)
	}

	err := pxy.SetRunningStatus(remoteAddr, serverRespErr)
	if err != nil {
		return err
	}
	return nil
}

func (pm *Manager) Close() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for _, pxy := range pm.proxies {
		pxy.Stop()
	}
	pm.proxies = make(map[string]*Wrapper)
}

func (pm *Manager) HandleWorkConn(name string, workConn net.Conn, m *msg.StartWorkConn) {
	pm.mu.RLock()
	pw, ok := pm.proxies[name]
	pm.mu.RUnlock()
	if ok {
		pw.InWorkConn(workConn, m)
	} else {
		workConn.Close()
	}
}

/**

这个函数的作用：
	frpc 中的 proxy 配置要发送给 frps，让frps 开始监听配置的端口
*/
func (pm *Manager) HandleEvent(evType event.Type, payload interface{}) error {
	var m msg.Message
	switch e := payload.(type) {
	case *event.StartProxyPayload:
		m = e.NewProxyMsg
	case *event.CloseProxyPayload:
		m = e.CloseProxyMsg
	default:
		return event.ErrPayloadType
	}

	err := errors.PanicToError(func() {
		pm.sendCh <- m
	})
	return err
}

func (pm *Manager) GetAllProxyStatus() []*WorkingStatus {
	ps := make([]*WorkingStatus, 0)
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	for _, pxy := range pm.proxies {
		ps = append(ps, pxy.GetStatus())
	}
	return ps
}

func (pm *Manager) Reload(pxyCfgs map[string]config.ProxyConf) {
	xl := xlog.FromContextSafe(pm.ctx)
	pm.mu.Lock()
	defer pm.mu.Unlock()

	delPxyNames := make([]string, 0)
	for name, pxy := range pm.proxies {
		del := false
		cfg, ok := pxyCfgs[name]
		if !ok {
			del = true
		} else {
			if !pxy.Cfg.Compare(cfg) {
				del = true
			}
		}

		if del {
			delPxyNames = append(delPxyNames, name)
			delete(pm.proxies, name)

			pxy.Stop() //这里停止 proxy，对应的 frps 的端口也会被关闭
		}
	}
	if len(delPxyNames) > 0 {
		xl.Info("proxy removed: %v", delPxyNames)
	}

	//到这里 pxyCfgs 为需要进行 proxy 的配置
	addPxyNames := make([]string, 0)
	for name, cfg := range pxyCfgs {
		if _, ok := pm.proxies[name]; !ok {
			pxy := NewWrapper(pm.ctx, cfg, pm.clientCfg, pm.HandleEvent, pm.serverUDPPort)
			pm.proxies[name] = pxy
			addPxyNames = append(addPxyNames, name)

			pxy.Start()
		}
	}
	if len(addPxyNames) > 0 {
		xl.Info("proxy added: %v", addPxyNames)
	}
}
