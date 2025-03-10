package functions

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/gravitl/netclient/config"
	"github.com/gravitl/netclient/daemon"
	"github.com/gravitl/netclient/local"
	"github.com/gravitl/netclient/ncutils"
	"github.com/gravitl/netclient/networking"
	"github.com/gravitl/netclient/nmproxy"
	proxy_cfg "github.com/gravitl/netclient/nmproxy/config"
	ncmodels "github.com/gravitl/netclient/nmproxy/models"
	"github.com/gravitl/netclient/nmproxy/stun"
	"github.com/gravitl/netclient/routes"
	"github.com/gravitl/netclient/wireguard"
	"github.com/gravitl/netmaker/logger"
	"github.com/gravitl/netmaker/models"
	"github.com/gravitl/netmaker/mq"
	"golang.org/x/exp/slog"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

const (
	lastNodeUpdate   = "lnu"
	lastDNSUpdate    = "ldu"
	lastALLDNSUpdate = "ladu"
)

var (
	Mqclient         mqtt.Client
	messageCache     = new(sync.Map)
	ProxyManagerChan = make(chan *models.HostPeerUpdate, 50)
	hostNatInfo      *ncmodels.HostInfo
)

type cachedMessage struct {
	Message  string
	LastSeen time.Time
}

func startProxy(wg *sync.WaitGroup) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	wg.Add(1)
	go nmproxy.Start(ctx, wg, ProxyManagerChan, hostNatInfo)
	return cancel
}

// Daemon runs netclient daemon
func Daemon() {
	slog.Info("starting netclient daemon", "version", config.Version)
	if err := ncutils.SavePID(); err != nil {
		slog.Error("unable to save PID on daemon startup", "error", err)
		os.Exit(1)
	}
	if err := local.SetIPForwarding(); err != nil {
		slog.Warn("unable to set IPForwarding", "error", err)
	}
	wg := sync.WaitGroup{}
	quit := make(chan os.Signal, 1)
	reset := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, os.Interrupt)
	signal.Notify(reset, syscall.SIGHUP)

	cancel := startGoRoutines(&wg)
	stopProxy := startProxy(&wg)
	//start httpserver on its own -- doesn't need to restart on reset
	httpctx, httpCancel := context.WithCancel(context.Background())
	httpWg := sync.WaitGroup{}
	httpWg.Add(1)
	go HttpServer(httpctx, &httpWg)
	for {
		select {
		case <-quit:
			slog.Info("shutting down netclient daemon")
			closeRoutines([]context.CancelFunc{
				cancel,
				stopProxy,
			}, &wg)
			httpCancel()
			httpWg.Wait()
			slog.Info("shutdown complete")
			return
		case <-reset:
			slog.Info("received reset")
			closeRoutines([]context.CancelFunc{
				cancel,
				stopProxy,
			}, &wg)
			slog.Info("resetting daemon")
			cleanUpRoutes()
			cancel = startGoRoutines(&wg)
			if !proxy_cfg.GetCfg().ProxyStatus {
				stopProxy = startProxy(&wg)
			}
		}
	}
}

func closeRoutines(closers []context.CancelFunc, wg *sync.WaitGroup) {
	for i := range closers {
		closers[i]()
	}
	if Mqclient != nil {
		Mqclient.Disconnect(250)
	}
	wg.Wait()
	slog.Info("closing netmaker interface")
	iface := wireguard.GetInterface()
	iface.Close()
}

// startGoRoutines starts the daemon goroutines
func startGoRoutines(wg *sync.WaitGroup) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := config.ReadNetclientConfig(); err != nil {
		slog.Error("error reading neclient config file", "error", err)
	}
	config.UpdateNetclient(*config.Netclient())
	if err := config.ReadServerConf(); err != nil {
		slog.Warn("error reading server map from disk", "error", err)
	}
	config.SetServerCtx()
	config.HostPublicIP, config.WgPublicListenPort = holePunchWgPort()
	slog.Info("wireguard public listen port: ", "port", config.WgPublicListenPort)
	setNatInfo()
	slog.Info("configuring netmaker wireguard interface")
	if len(config.Servers) == 0 {
		ProxyManagerChan <- &models.HostPeerUpdate{
			ProxyUpdate: models.ProxyManagerPayload{
				Action: models.ProxyDeleteAllPeers,
			},
		}
	}

	Pull(false)
	nc := wireguard.NewNCIface(config.Netclient(), config.GetNodes())
	nc.Create()
	nc.Configure()
	wireguard.SetPeers(true)
	server := config.GetServer(config.CurrServer)
	if server == nil {
		return cancel
	}
	logger.Log(1, "started daemon for server ", server.Name)
	networking.StoreServerAddresses(server)
	err := routes.SetNetmakerServerRoutes(config.Netclient().DefaultInterface, server)
	if err != nil {
		logger.Log(2, "failed to set route(s) for", server.Name, err.Error())
	}
	wg.Add(1)
	go messageQueue(ctx, wg, server)
	if err := routes.SetNetmakerPeerEndpointRoutes(config.Netclient().DefaultInterface); err != nil {
		slog.Warn("failed to set initial peer routes", "error", err.Error())
	}
	wg.Add(1)
	go Checkin(ctx, wg)
	wg.Add(1)
	go networking.StartIfaceDetection(ctx, wg, config.Netclient().ProxyListenPort)
	return cancel
}

// sets up Message Queue and subsribes/publishes updates to/from server
// the client should subscribe to ALL nodes that exist on server locally
func messageQueue(ctx context.Context, wg *sync.WaitGroup, server *config.Server) {
	defer wg.Done()
	slog.Info("netclient message queue started for server:", "server", server.Name)
	err := setupMQTT(server)
	if err != nil {
		slog.Error("unable to connect to broker", "server", server.Broker, "error", err)
		return
	}
	defer func() {
		if Mqclient != nil {
			Mqclient.Disconnect(250)
		}
	}()
	<-ctx.Done()
	slog.Info("shutting down message queue", "server", server.Name)
}

// setupMQTT creates a connection to broker
func setupMQTT(server *config.Server) error {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(server.Broker)
	opts.SetUsername(server.MQUserName)
	opts.SetPassword(server.MQPassword)
	//opts.SetClientID(ncutils.MakeRandomString(23))
	opts.SetClientID(server.MQID.String())
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(time.Second << 2)
	opts.SetKeepAlive(time.Second * 10)
	opts.SetWriteTimeout(time.Minute)
	opts.SetOnConnectHandler(func(client mqtt.Client) {
		slog.Info("mqtt connect handler")
		nodes := config.GetNodes()
		for _, node := range nodes {
			node := node
			setSubscriptions(client, &node)
		}
		setHostSubscription(client, server.Name)
		checkin()
	})
	opts.SetOrderMatters(true)
	opts.SetResumeSubs(true)
	opts.SetConnectionLostHandler(func(c mqtt.Client, e error) {
		slog.Warn("detected broker connection lost for", "server", server.Broker)
		if ok := resetServerRoutes(); ok {
			slog.Info("detected default gateway change, reset server routes")
			if err := UpdateHostSettings(); err != nil {
				slog.Error("failed to update host settings", "error", err)
				return
			}

			handlePeerInetGateways(
				!config.GW4PeerDetected && !config.GW6PeerDetected,
				config.IsHostInetGateway(), false,
				nil,
			)
		}
	})
	Mqclient = mqtt.NewClient(opts)
	var connecterr error
	for count := 0; count < 3; count++ {
		connecterr = nil
		if token := Mqclient.Connect(); !token.WaitTimeout(30*time.Second) || token.Error() != nil {
			logger.Log(0, "unable to connect to broker, retrying ...")
			if token.Error() == nil {
				connecterr = errors.New("connect timeout")
			} else {
				connecterr = token.Error()
			}
		}
	}
	if connecterr != nil {
		slog.Error("unable to connect to broker", "server", server.Broker, "error", connecterr)
		return connecterr
	}
	if err := PublishHostUpdate(server.Name, models.Acknowledgement); err != nil {
		slog.Error("failed to send initial ACK to server", "server", server.Name, "error", err)
	} else {
		slog.Info("successfully requested ACK on server", "server", server.Name)
	}
	// send register signal with turn to server
	if server.UseTurn {
		if err := PublishHostUpdate(server.Server, models.RegisterWithTurn); err != nil {
			slog.Error("failed to publish host turn register signal to server", "server", server.Server, "error", err)
		} else {
			slog.Info("published host turn register signal to server", "server", server.Server)
		}
	}

	return nil
}

// func setMQTTSingenton creates a connection to broker for single use (ie to publish a message)
// only to be called from cli (eg. connect/disconnect, join, leave) and not from daemon ---
func setupMQTTSingleton(server *config.Server, publishOnly bool) error {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(server.Broker)
	opts.SetUsername(server.MQUserName)
	opts.SetPassword(server.MQPassword)
	opts.SetClientID(server.MQID.String())
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(time.Second << 2)
	opts.SetKeepAlive(time.Minute >> 1)
	opts.SetWriteTimeout(time.Minute)
	opts.SetOnConnectHandler(func(client mqtt.Client) {
		if !publishOnly {
			slog.Info("mqtt connect handler")
			nodes := config.GetNodes()
			for _, node := range nodes {
				node := node
				setSubscriptions(client, &node)
			}
			setHostSubscription(client, server.Name)
		}
		slog.Info("successfully connected to", "server", server.Broker)
	})
	opts.SetOrderMatters(true)
	opts.SetResumeSubs(true)
	opts.SetConnectionLostHandler(func(c mqtt.Client, e error) {
		slog.Warn("detected broker connection lost for", "server", server.Broker)
	})
	Mqclient = mqtt.NewClient(opts)

	var connecterr error
	if token := Mqclient.Connect(); !token.WaitTimeout(30*time.Second) || token.Error() != nil {
		logger.Log(0, "unable to connect to broker,", server.Broker+",", "retrying...")
		if token.Error() == nil {
			connecterr = errors.New("connect timeout")
		} else {
			connecterr = token.Error()
		}
	}
	return connecterr
}

// setHostSubscription sets MQ client subscriptions for host
// should be called for each server host is registered on.
func setHostSubscription(client mqtt.Client, server string) {
	hostID := config.Netclient().ID
	slog.Info("subscribing to host updates for", "host", hostID, "server", server)
	if token := client.Subscribe(fmt.Sprintf("peers/host/%s/%s", hostID.String(), server), 0, mqtt.MessageHandler(HostPeerUpdate)); token.Wait() && token.Error() != nil {
		slog.Error("unable to subscribe to host peer updates", "host", hostID, "server", server, "error", token.Error)
		return
	}
	slog.Info("subscribing to host updates for", "host", hostID, "server", server)
	if token := client.Subscribe(fmt.Sprintf("host/update/%s/%s", hostID.String(), server), 0, mqtt.MessageHandler(HostUpdate)); token.Wait() && token.Error() != nil {
		slog.Error("unable to subscribe to host updates", "host", hostID, "server", server, "error", token.Error)
		return
	}
	slog.Info("subscribing to dns updates for", "host", hostID, "server", server)
	if token := client.Subscribe(fmt.Sprintf("dns/update/%s/%s", hostID.String(), server), 0, mqtt.MessageHandler(dnsUpdate)); token.Wait() && token.Error() != nil {
		slog.Error("unable to subscribe to dns updates", "host", hostID, "server", server, "error", token.Error)
		return
	}
	slog.Info("subscribing to all dns updates for", "host", hostID, "server", server)
	if token := client.Subscribe(fmt.Sprintf("dns/all/%s/%s", hostID.String(), server), 0, mqtt.MessageHandler(dnsAll)); token.Wait() && token.Error() != nil {
		slog.Error("unable to subscribe to all dns updates", "host", hostID, "server", server, "error", token.Error)
		return
	}
}

// setSubcriptions sets MQ client subscriptions for a specific node config
// should be called for each node belonging to a given server
func setSubscriptions(client mqtt.Client, node *config.Node) {
	if token := client.Subscribe(fmt.Sprintf("node/update/%s/%s", node.Network, node.ID), 0, mqtt.MessageHandler(NodeUpdate)); token.WaitTimeout(mq.MQ_TIMEOUT*time.Second) && token.Error() != nil {
		if token.Error() == nil {
			slog.Error("unable to subscribe to updates for node ", "node", node.ID, "error", "connection timeout")
		} else {
			slog.Error("unable to subscribe to updates for node ", "node", node.ID, "error", token.Error)
		}
		return
	}
	slog.Info("subscribed to updates for node", "node", node.ID, "network", node.Network)
}

// should only ever use node client configs
func decryptMsg(serverName string, msg []byte) ([]byte, error) {
	if len(msg) <= 24 { // make sure message is of appropriate length
		return nil, fmt.Errorf("received invalid message from broker %v", msg)
	}
	host := config.Netclient()
	// setup the keys
	diskKey, err := ncutils.ConvertBytesToKey(host.TrafficKeyPrivate)
	if err != nil {
		return nil, err
	}

	server := config.GetServer(serverName)
	if server == nil {
		return nil, errors.New("nil server for " + serverName)
	}
	serverPubKey, err := ncutils.ConvertBytesToKey(server.TrafficKey)
	if err != nil {
		return nil, err
	}
	return DeChunk(msg, serverPubKey, diskKey)
}

func read(network, which string) string {
	val, isok := messageCache.Load(fmt.Sprintf("%s%s", network, which))
	if isok {
		var readMessage = val.(cachedMessage) // fetch current cached message
		if readMessage.LastSeen.IsZero() {
			return ""
		}
		if time.Now().After(readMessage.LastSeen.Add(time.Hour * 24)) { // check if message has been there over a minute
			messageCache.Delete(fmt.Sprintf("%s%s", network, which)) // remove old message if expired
			return ""
		}
		return readMessage.Message // return current message if not expired
	}
	return ""
}

func insert(network, which, cache string) {
	var newMessage = cachedMessage{
		Message:  cache,
		LastSeen: time.Now(),
	}
	messageCache.Store(fmt.Sprintf("%s%s", network, which), newMessage)
}

// on a delete usually, pass in the nodecfg to unsubscribe client broker communications
// for the node in nodeCfg
func unsubscribeNode(client mqtt.Client, node *config.Node) {
	var ok = true
	if token := client.Unsubscribe(fmt.Sprintf("node/update/%s/%s", node.Network, node.ID)); token.WaitTimeout(mq.MQ_TIMEOUT*time.Second) && token.Error() != nil {
		if token.Error() == nil {
			slog.Error("unable to unsubscribe from updates for node ", "node", node.ID, "error", "connection timeout")
		} else {
			slog.Error("unable to unsubscribe from updates for node ", "node", node.ID, "error", token.Error)
		}
		ok = false
	} // peer updates belong to host now

	if ok {
		slog.Info("unsubscribed from updates for node", "node", node.ID, "network", node.Network)
	}
}

// unsubscribe client broker communications for host topics
func unsubscribeHost(client mqtt.Client, server string) {
	hostID := config.Netclient().ID
	slog.Info("removing subscription for host peer updates", "host", hostID, "server", server)
	if token := client.Unsubscribe(fmt.Sprintf("peers/host/%s/%s", hostID.String(), server)); token.WaitTimeout(mq.MQ_TIMEOUT*time.Second) && token.Error() != nil {
		slog.Error("unable to unsubscribe from host peer updates", "host", hostID, "server", server, "error", token.Error)
		return
	}
	slog.Info("removing subscription for host updates", "host", hostID, "server", server)
	if token := client.Unsubscribe(fmt.Sprintf("host/update/%s/%s", hostID.String(), server)); token.WaitTimeout(mq.MQ_TIMEOUT*time.Second) && token.Error() != nil {
		slog.Error("unable to unsubscribe from host updates", "host", hostID, "server", server, "error", token.Error)
		return
	}
}

// UpdateKeys -- updates private key and returns new publickey
func UpdateKeys() error {
	var err error
	slog.Info("received message to update wireguard keys")
	host := config.Netclient()
	host.PrivateKey, err = wgtypes.GeneratePrivateKey()
	if err != nil {
		slog.Error("error generating privatekey ", "error", err)
		return err
	}
	host.PublicKey = host.PrivateKey.PublicKey()
	if err := config.WriteNetclientConfig(); err != nil {
		slog.Error("error saving netclient config:", "error", err)
	}
	PublishHostUpdate(config.CurrServer, models.UpdateHost)
	daemon.Restart()
	return nil
}

func holePunchWgPort() (pubIP net.IP, pubPort int) {
	for _, server := range config.Servers {
		portToStun := config.Netclient().ListenPort
		pubIP, pubPort = stun.HolePunch(server.StunList, portToStun)
		if pubPort == 0 || pubIP == nil || pubIP.IsUnspecified() {
			continue
		}
		break
	}
	return
}

func setNatInfo() {
	portToStun, err := ncutils.GetFreePort(config.Netclient().ProxyListenPort)
	if err != nil {
		slog.Error("failed to get freeport for proxy: ", "error", err)
		return
	}
	for _, server := range config.Servers {
		server := server
		if hostNatInfo == nil {
			hostNatInfo = stun.GetHostNatInfo(
				server.StunList,
				config.Netclient().EndpointIP.String(),
				portToStun,
			)
		}
	}
}

func cleanUpRoutes() {
	gwAddr := config.GW4Addr
	if gwAddr.IP == nil {
		gwAddr = config.GW6Addr
	}
	if err := routes.CleanUp(config.Netclient().DefaultInterface, &gwAddr); err != nil {
		slog.Error("routes not completely cleaned up", "error", err)
	}
}

func resetServerRoutes() bool {
	if routes.HasGatewayChanged() {
		cleanUpRoutes()
		server := config.GetServer(config.CurrServer)
		if err := routes.SetNetmakerServerRoutes(config.Netclient().DefaultInterface, server); err != nil {
			logger.Log(2, "failed to set route(s) for", server.Name, err.Error())
		}
		if err := routes.SetNetmakerPeerEndpointRoutes(config.Netclient().DefaultInterface); err != nil {
			logger.Log(2, "failed to set route(s) for", server.Name, err.Error())
		}
		return true
	}
	return false
}
