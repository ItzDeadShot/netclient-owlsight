package nmproxy

import (
	"context"
	"fmt"

	"github.com/gravitl/netclient/nmproxy/config"
	"github.com/gravitl/netclient/nmproxy/manager"
	"github.com/gravitl/netclient/nmproxy/models"
	"github.com/gravitl/netclient/nmproxy/peer"
	"github.com/gravitl/netclient/nmproxy/server"
	"github.com/gravitl/netclient/nmproxy/stun"
	"github.com/gravitl/netmaker/logger"
)

// Start - setups the global cfg for proxy and starts the proxy server
func Start(ctx context.Context, mgmChan chan *models.ProxyManagerPayload, stunAddr string, stunPort int, fromServer bool) {

	if config.GetCfg().IsProxyRunning() {
		logger.Log(1, "Proxy is running already...")
		return
	}
	logger.Log(1, "Starting Proxy...")
	if stunAddr == "" || stunPort == 0 {
		logger.Log(1, "stun config values cannot be empty")
		return
	}
	config.InitializeCfg()
	config.GetCfg().SetIsHostNetwork(!fromServer)
	config.GetCfg().SetHostInfo(stun.GetHostInfo(stunAddr, stunPort))
	logger.Log(0, fmt.Sprintf("HOSTINFO: %+v", config.GetCfg().GetHostInfo()))
	config.GetCfg().SetNATStatus()
	// start the netclient proxy server
	err := server.NmProxyServer.CreateProxyServer(config.GetCfg().GetHostInfo().PrivPort, 0, config.GetCfg().GetHostInfo().PrivIp.String())
	if err != nil {
		logger.FatalLog("failed to create proxy: ", err.Error())
	}
	config.GetCfg().SetsServerConn(server.NmProxyServer.Server)
	go manager.Start(ctx, mgmChan)
	go peer.StartMetricsCollectionForNoProxyPeers(ctx)
	server.NmProxyServer.Listen(ctx)

}
