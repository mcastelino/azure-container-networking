// Copyright 2017 Microsoft. All rights reserved.
// MIT License

package ipam

import (
	"encoding/json"
	"net"
	"strconv"

	"github.com/Azure/azure-container-networking/cni"
	"github.com/Azure/azure-container-networking/common"
	"github.com/Azure/azure-container-networking/ipam"
	"github.com/Azure/azure-container-networking/log"
	"github.com/Azure/azure-container-networking/platform"

	cniSkel "github.com/containernetworking/cni/pkg/skel"
	cniTypes "github.com/containernetworking/cni/pkg/types"
	cniTypesImpl "github.com/containernetworking/cni/pkg/types/020"
)

const (
	// Plugin name.
	name = "azure-vnet-ipam"
)

var (
	// Azure VNET pre-allocated host IDs.
	ipv4DefaultGatewayHostId = net.ParseIP("::1")
	ipv4DnsPrimaryHostId     = net.ParseIP("::2")
	ipv4DnsSecondaryHostId   = net.ParseIP("::3")

	ipv4DefaultRouteDstPrefix = net.IPNet{net.IPv4zero, net.IPv4Mask(0, 0, 0, 0)}
)

// IpamPlugin represents the CNI IPAM plugin.
type ipamPlugin struct {
	*cni.Plugin
	am ipam.AddressManager
}

// NewPlugin creates a new ipamPlugin object.
func NewPlugin(config *common.PluginConfig) (*ipamPlugin, error) {
	// Setup base plugin.
	plugin, err := cni.NewPlugin(name, config.Version)
	if err != nil {
		return nil, err
	}

	// Setup address manager.
	am, err := ipam.NewAddressManager()
	if err != nil {
		return nil, err
	}

	// Create IPAM plugin.
	ipamPlg := &ipamPlugin{
		Plugin: plugin,
		am:     am,
	}

	config.IpamApi = ipamPlg

	return ipamPlg, nil
}

// Starts the plugin.
func (plugin *ipamPlugin) Start(config *common.PluginConfig) error {
	// Initialize base plugin.
	err := plugin.Initialize(config)
	if err != nil {
		log.Printf("[cni-ipam] Failed to initialize base plugin, err:%v.", err)
		return err
	}

	// Log platform information.
	log.Printf("[cni-ipam] Plugin %v version %v.", plugin.Name, plugin.Version)
	log.Printf("[cni-ipam] Running on %v", platform.GetOSInfo())

	// Initialize address manager.
	err = plugin.am.Initialize(config, plugin.Options)
	if err != nil {
		log.Printf("[cni-ipam] Failed to initialize address manager, err:%v.", err)
		return err
	}

	log.Printf("[cni-ipam] Plugin started.")

	return nil
}

// Stops the plugin.
func (plugin *ipamPlugin) Stop() {
	plugin.am.Uninitialize()
	plugin.Uninitialize()
	log.Printf("[cni-ipam] Plugin stopped.")
}

// Configure parses and applies the given network configuration.
func (plugin *ipamPlugin) Configure(stdinData []byte) (*cni.NetworkConfig, error) {
	// Parse network configuration from stdin.
	nwCfg, err := cni.ParseNetworkConfig(stdinData)
	if err != nil {
		return nil, err
	}

	log.Printf("[cni-ipam] Read network configuration %+v.", nwCfg)

	// Apply IPAM configuration.

	// Set deployment environment.
	if nwCfg.Ipam.Environment == "" {
		nwCfg.Ipam.Environment = common.OptEnvironmentAzure
	}
	plugin.SetOption(common.OptEnvironment, nwCfg.Ipam.Environment)

	// Set query interval.
	if nwCfg.Ipam.QueryInterval != "" {
		i, _ := strconv.Atoi(nwCfg.Ipam.QueryInterval)
		plugin.SetOption(common.OptIpamQueryInterval, i)
	}

	err = plugin.am.StartSource(plugin.Options)
	if err != nil {
		return nil, err
	}

	// Set default address space if not specified.
	if nwCfg.Ipam.AddrSpace == "" {
		nwCfg.Ipam.AddrSpace = ipam.LocalDefaultAddressSpaceId
	}

	return nwCfg, nil
}

//
// CNI implementation
// https://github.com/containernetworking/cni/blob/master/SPEC.md
//

// Add handles CNI add commands.
func (plugin *ipamPlugin) Add(args *cniSkel.CmdArgs) error {
	log.Printf("[cni-ipam] Processing ADD command with args {ContainerID:%v Netns:%v IfName:%v Args:%v Path:%v}.",
		args.ContainerID, args.Netns, args.IfName, args.Args, args.Path)

	// Parse network configuration from stdin.
	nwCfg, err := plugin.Configure(args.StdinData)
	if err != nil {
		return plugin.Errorf("Failed to parse network configuration: %v", err)
	}

	var poolId string
	var subnet string
	var ipv4Address *net.IPNet
	var result cniTypes.Result
	var resultImpl *cniTypesImpl.Result
	var apInfo *ipam.AddressPoolInfo

	// Check if an address pool is specified.
	if nwCfg.Ipam.Subnet == "" {
		// Select the requested interface.
		options := make(map[string]string)
		options[ipam.OptInterface] = nwCfg.Master

		// Allocate an address pool.
		poolId, subnet, err = plugin.am.RequestPool(nwCfg.Ipam.AddrSpace, "", "", options, false)
		if err != nil {
			return plugin.Errorf("Failed to allocate pool: %v.", err)
		}

		nwCfg.Ipam.Subnet = subnet
		log.Printf("[cni-ipam] Allocated address poolId %v with subnet %v.", poolId, subnet)
	}

	// Allocate an address for the endpoint.
	address, err := plugin.am.RequestAddress(nwCfg.Ipam.AddrSpace, nwCfg.Ipam.Subnet, nwCfg.Ipam.Address, nil)
	if err != nil {
		err = plugin.Errorf("Failed to allocate address: %v", err)
		goto Rollback
	}

	log.Printf("[cni-ipam] Allocated address %v.", address)

	// Parse IP address.
	ipv4Address, err = ipam.ConvertAddressToIPNet(address)
	if err != nil {
		err = plugin.Errorf("Failed to parse address: %v", err)
		goto Rollback
	}

	// Query pool information for gateways and DNS servers.
	apInfo, err = plugin.am.GetPoolInfo(nwCfg.Ipam.AddrSpace, nwCfg.Ipam.Subnet)
	if err != nil {
		err = plugin.Errorf("Failed to get pool information: %v", err)
		goto Rollback
	}

	// Populate IP configuration.
	resultImpl = &cniTypesImpl.Result{
		IP4: &cniTypesImpl.IPConfig{
			IP:      *ipv4Address,
			Gateway: apInfo.Gateway,
			Routes: []cniTypes.Route{
				cniTypes.Route{
					Dst: ipv4DefaultRouteDstPrefix,
					GW:  apInfo.Gateway,
				},
			},
		},
	}

	// Populate DNS servers.
	for _, ip := range apInfo.DnsServers {
		resultImpl.DNS.Nameservers = append(resultImpl.DNS.Nameservers, ip.String())
	}

	// Convert result to the requested CNI version.
	result, err = resultImpl.GetAsVersion(nwCfg.CniVersion)
	if err != nil {
		err = plugin.Errorf("Failed to convert result: %v", err)
		goto Rollback
	}

	// Output the result.
	if nwCfg.Ipam.Type == cni.Internal {
		// Called via the internal interface. Pass output back in args.
		args.StdinData, _ = json.Marshal(result)
	} else {
		// Called via the executable interface. Print output to stdout.
		result.Print()
	}

	log.Printf("[cni-ipam] ADD succeeded with output %+v.", result)

	return nil

Rollback:
	// Roll back allocations made during this call.
	log.Printf("[cni-ipam] ADD failed, err:%v.", err)

	if address != "" {
		log.Printf("[cni-ipam] Releasing address %v.", address)
		plugin.am.ReleaseAddress(nwCfg.Ipam.AddrSpace, nwCfg.Ipam.Subnet, address)
	}

	if poolId != "" {
		log.Printf("[cni-ipam] Releasing pool %v.", poolId)
		plugin.am.ReleasePool(nwCfg.Ipam.AddrSpace, poolId)
	}

	return err
}

// Delete handles CNI delete commands.
func (plugin *ipamPlugin) Delete(args *cniSkel.CmdArgs) error {
	log.Printf("[cni-ipam] Processing DEL command with args {ContainerID:%v Netns:%v IfName:%v Args:%v Path:%v}.",
		args.ContainerID, args.Netns, args.IfName, args.Args, args.Path)

	// Parse network configuration from stdin.
	nwCfg, err := plugin.Configure(args.StdinData)
	if err != nil {
		plugin.Errorf("Failed to parse network configuration: %v", err)
	}

	// If an address is specified, release that address. Otherwise, release the pool.
	if nwCfg.Ipam.Address != "" {
		// Release the address.
		err := plugin.am.ReleaseAddress(nwCfg.Ipam.AddrSpace, nwCfg.Ipam.Subnet, nwCfg.Ipam.Address)
		if err != nil {
			plugin.Errorf("Failed to release address: %v", err)
		}
	} else {
		// Release the pool.
		err := plugin.am.ReleasePool(nwCfg.Ipam.AddrSpace, nwCfg.Ipam.Subnet)
		if err != nil {
			plugin.Errorf("Failed to release pool: %v", err)
		}
	}

	log.Printf("[cni-ipam] DEL succeeded.")

	return nil
}
