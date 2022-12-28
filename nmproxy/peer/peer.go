package peer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gravitl/netclient/nmproxy/config"
	"github.com/gravitl/netclient/nmproxy/metrics"
	"github.com/gravitl/netclient/nmproxy/models"
	"github.com/gravitl/netclient/nmproxy/packet"
	"github.com/gravitl/netclient/nmproxy/proxy"
	"github.com/gravitl/netclient/nmproxy/wg"
	"github.com/gravitl/netmaker/logger"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// AddNew - adds new peer to proxy config and starts proxying the peer
func AddNew(network string, peer *wgtypes.PeerConfig, peerConf models.PeerConf,
	isRelayed bool, relayTo *net.UDPAddr) error {

	if peer.PersistentKeepaliveInterval == nil {
		d := models.DefaultPersistentKeepaliveInterval
		peer.PersistentKeepaliveInterval = &d
	}
	c := models.Proxy{
		LocalKey:            config.GetCfg().GetDevicePubKey(),
		RemoteKey:           peer.PublicKey,
		IsExtClient:         peerConf.IsExtClient,
		PeerConf:            peer,
		PersistentKeepalive: peer.PersistentKeepaliveInterval,
		Network:             network,
		ListenPort:          int(peerConf.PublicListenPort),
	}
	p := proxy.New(c)
	peerPort := int(peerConf.PublicListenPort)
	if peerPort == 0 {
		peerPort = models.NmProxyPort
	}
	if peerConf.IsExtClient && peerConf.IsAttachedExtClient {
		peerPort = peer.Endpoint.Port

	}
	peerEndpointIP := peer.Endpoint.IP
	if isRelayed {
		//go server.NmProxyServer.KeepAlive(peer.Endpoint.IP.String(), common.NmProxyPort)
		if relayTo == nil {
			return errors.New("relay endpoint is nil")
		}
		peerEndpointIP = relayTo.IP
	}
	peerEndpoint, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", peerEndpointIP, peerPort))
	if err != nil {
		return err
	}
	p.Config.PeerEndpoint = peerEndpoint

	logger.Log(0, "Starting proxy for Peer: %s\n", peer.PublicKey.String())
	err = p.Start()
	if err != nil {
		return err
	}

	connConf := models.Conn{
		Mutex:               &sync.RWMutex{},
		Key:                 peer.PublicKey,
		IsRelayed:           isRelayed,
		RelayedEndpoint:     relayTo,
		IsAttachedExtClient: peerConf.IsAttachedExtClient,
		Config:              p.Config,
		StopConn:            p.Close,
		ResetConn:           p.Reset,
		LocalConn:           p.LocalConn,
	}
	rPeer := models.RemotePeer{
		Network:             network,
		Interface:           config.GetCfg().GetIface().Name,
		PeerKey:             peer.PublicKey.String(),
		IsExtClient:         peerConf.IsExtClient,
		Endpoint:            peerEndpoint,
		IsAttachedExtClient: peerConf.IsAttachedExtClient,
		LocalConn:           p.LocalConn,
	}
	config.GetCfg().SavePeer(network, &connConf)
	config.GetCfg().SavePeerByHash(&rPeer)

	if peerConf.IsAttachedExtClient {
		config.GetCfg().SaveExtClientInfo(&rPeer)
		//add rules to router
		routingInfo := &config.Routing{
			InternalIP: peerConf.ExtInternalIp,
			ExternalIP: peerConf.Address,
		}
		config.GetCfg().SaveRoutingInfo(routingInfo)

	}
	return nil
}

// SetPeersEndpointToProxy - sets peer endpoints to local addresses connected to proxy
func SetPeersEndpointToProxy(network string, peers []wgtypes.PeerConfig) []wgtypes.PeerConfig {
	logger.Log(1, "Setting peers endpoints to proxy: ", network)
	if !config.GetCfg().ProxyStatus {
		return peers
	}
	for i := range peers {
		proxyPeer, found := config.GetCfg().GetPeer(network, peers[i].PublicKey.String())
		if found {
			proxyPeer.Mutex.RLock()
			peers[i].Endpoint = proxyPeer.Config.LocalConnAddr
			proxyPeer.Mutex.RUnlock()
		}
	}
	return peers
}

// StartMetricsCollectionForNoProxyPeers - starts metrics collection for non proxied peers
func StartMetricsCollectionForNoProxyPeers(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			noProxyPeers := config.GetCfg().GetNoProxyPeers()
			for peerPubKey, peerInfo := range noProxyPeers {
				go collectMetricsForNoProxyPeer(peerPubKey, *peerInfo)
			}
		}
	}
}

func collectMetricsForNoProxyPeer(peerKey string, peerInfo models.RemotePeer) {

	devPeer, err := wg.GetPeer(peerInfo.Interface, peerKey)
	if err != nil {
		return
	}
	connectionStatus := metrics.PeerConnectionStatus(peerInfo.Address.String())
	metric := models.Metric{
		LastRecordedLatency: 999,
		ConnectionStatus:    connectionStatus,
	}
	metric.TrafficRecieved = float64(devPeer.ReceiveBytes) / (1 << 20) // collected in MB
	metric.TrafficSent = float64(devPeer.TransmitBytes) / (1 << 20)    // collected in MB
	metrics.UpdateMetric(peerInfo.Network, peerInfo.PeerKey, &metric)
	pkt, err := packet.CreateMetricPacket(uuid.New().ID(), peerInfo.Network, config.GetCfg().GetDevicePubKey(), devPeer.PublicKey)
	if err == nil {
		conn := config.GetCfg().GetServerConn()
		if conn != nil {
			_, err = conn.WriteToUDP(pkt, peerInfo.Endpoint)
			if err != nil {
				logger.Log(1, "Failed to send to metric pkt: ", err.Error())
			}
		}

	}
}
