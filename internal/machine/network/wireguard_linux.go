//go:build linux

package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/psviderski/uncloud/internal/secret"
	"github.com/vishvananda/netlink"
	"go4.org/netipx"
	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

type WireGuardNetwork struct {
	link netlink.Link
	// peers is a map of peers indexed by their public key.
	peers map[string]*peer
	// watchers is a list of channels that are notified when the endpoints of the peers change.
	watchers []chan EndpointChangeEvent
	// running indicates whether the network control loop (Run) is currently running.
	running bool
	// mu synchronises concurrent network configuration changes.
	mu sync.Mutex
}

func NewWireGuardNetwork() (*WireGuardNetwork, error) {
	link, err := createOrGetLink(WireGuardInterfaceName)
	if err != nil {
		return nil, fmt.Errorf("create or get WireGuard link %q: %v", WireGuardInterfaceName, err)
	}
	return &WireGuardNetwork{link: link}, nil
}

// createOrGetLink creates a new WireGuard link with the given name if it doesn't already exist, otherwise it returns the existing link.
func createOrGetLink(name string) (netlink.Link, error) {
	link, err := netlink.LinkByName(name)
	if err == nil {
		slog.Info("Found existing WireGuard interface.", "name", name)
		return link, nil
	}
	//goland:noinspection GoTypeAssertionOnErrors
	if _, ok := err.(netlink.LinkNotFoundError); !ok {
		return nil, fmt.Errorf("find WireGuard link %q: %v", name, err)
	}
	link = &netlink.GenericLink{
		// TODO: figure out how to set the most appropriate MTU.
		LinkAttrs: netlink.LinkAttrs{Name: name},
		LinkType:  "wireguard",
	}
	if err = netlink.LinkAdd(link); err != nil {
		return nil, fmt.Errorf("create WireGuard link %q: %v", name, err)
	}
	slog.Info("Created WireGuard interface.", "name", name)

	// Refetch the link to get the most up-to-date information.
	link, err = netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("find created WireGuard link %q: %v", name, err)
	}
	return link, nil
}

// Configure applies the given configuration to the WireGuard network interface.
// It updates device and peers settings, subnet, and peer routes.
func (n *WireGuardNetwork) Configure(config Config) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if err := n.configureDevice(config); err != nil {
		return err
	}
	slog.Info("Configured WireGuard interface.", "name", n.link.Attrs().Name)

	machinePrefix := netip.PrefixFrom(MachineIP(config.Subnet), config.Subnet.Bits())
	managementPrefix, err := addrToSingleIPPrefix(config.ManagementIP)
	if err != nil {
		return fmt.Errorf("parse management IP: %w", err)
	}
	addrs := []netip.Prefix{managementPrefix, machinePrefix}
	if err = n.updateAddresses(addrs); err != nil {
		return err
	}
	slog.Info(
		"Updated addresses of the WireGuard interface.",
		"name", n.link.Attrs().Name, "addrs", addrs,
	)

	// Bring the WireGuard interface up if it's not already up.
	if n.link.Attrs().Flags&unix.IFF_UP != unix.IFF_UP {
		if err = netlink.LinkSetUp(n.link); err != nil {
			return fmt.Errorf("set WireGuard link %q up: %w", n.link.Attrs().Name, err)
		}
		slog.Info("Brought WireGuard interface up.", "name", n.link.Attrs().Name)
	}
	if err = n.updatePeerRoutes(); err != nil {
		return err
	}
	slog.Info(
		"Updated routes to peers via the WireGuard interface.",
		"name", n.link.Attrs().Name, "peers", len(n.peers),
	)

	return nil
}

func (n *WireGuardNetwork) configureDevice(config Config) error {
	wg, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("create WireGuard client: %w", err)
	}
	defer wg.Close()

	// Get the current WireGuard peers from the device which are required to reconstruct the local peers structs
	// and build a new config for the device.
	dev, err := wg.Device(n.link.Attrs().Name)
	if err != nil {
		return fmt.Errorf("get WireGuard device %q: %w", n.link.Attrs().Name, err)
	}
	if n.peers == nil {
		// This is the first time we configure this instance of the network. If the device has been configured earlier
		// and the daemon restarted, attempt to reconstruct the peers from the device to avoid unnecessary endpoint
		// rotations and connection disruptions.
		n.peers = make(map[string]*peer, len(config.Peers))
		wgPeers := make(map[string]*wgtypes.Peer, len(dev.Peers))
		for i := range dev.Peers {
			wgPeers[secret.Secret(dev.Peers[i].PublicKey[:]).String()] = &dev.Peers[i]
		}
		for _, pc := range config.Peers {
			wgPeer := wgPeers[pc.PublicKey.String()]
			n.peers[pc.PublicKey.String()] = newPeer(pc, wgPeer)
		}
	}

	// Create or update peer structs based on the config.
	newPeersSet := make(map[string]struct{}, len(config.Peers))
	for _, pc := range config.Peers {
		if p, ok := n.peers[pc.PublicKey.String()]; ok {
			p.updateConfig(pc)
		} else {
			// WireGuard peers should not be configured out of band so the current WG peer is provided only
			// for the first time configuration above.
			n.peers[pc.PublicKey.String()] = newPeer(pc, nil)
		}
		newPeersSet[pc.PublicKey.String()] = struct{}{}
	}
	// Delete peers that are no longer in the config.
	for k := range n.peers {
		if _, ok := newPeersSet[k]; !ok {
			delete(n.peers, k)
		}
	}

	wgConfig, err := config.toDeviceConfig(dev.Peers)
	if err != nil {
		return err
	}
	// Apply the new configuration to the WireGuard device.
	if err = wg.ConfigureDevice(n.link.Attrs().Name, wgConfig); err != nil {
		return fmt.Errorf("configure WireGuard device %q: %w", n.link.Attrs().Name, err)
	}

	return nil
}

// updateAddresses assigns addresses to the WireGuard interface and removes old ones.
// It also removes any other addresses that have been added out of band.
func (n *WireGuardNetwork) updateAddresses(addrs []netip.Prefix) error {
	for _, addr := range addrs {
		ipNet := prefixToIPNet(addr)
		if err := netlink.AddrAdd(n.link, &netlink.Addr{IPNet: &ipNet}); err != nil {
			if !errors.Is(err, unix.EEXIST) {
				return fmt.Errorf("add subnet address to WireGuard link %q: %w", n.link.Attrs().Name, err)
			}
		}
	}
	// Remove the old addresses or any other addresses that have been added out of band.
	linkAddrs, err := netlink.AddrList(n.link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("list addresses on WireGuard link %q: %w", n.link.Attrs().Name, err)
	}
	for _, linkAddr := range linkAddrs {
		if slices.ContainsFunc(
			addrs, func(a netip.Prefix) bool {
				return linkAddr.IPNet.String() == a.String()
			},
		) {
			continue
		}
		if err = netlink.AddrDel(n.link, &linkAddr); err != nil {
			return fmt.Errorf("remove address %q from WireGuard link %q: %w", linkAddr.IPNet, n.link.Attrs().Name, err)
		}
	}
	return nil
}

// updatePeerRoutes adds routes to the peers via the WireGuard interface and removes old routes to peers
// that are no longer in the configuration.
func (n *WireGuardNetwork) updatePeerRoutes() error {
	// Build a set of compacted IP ranges for all peers.
	var ipsetBuilder netipx.IPSetBuilder
	for _, p := range n.peers {
		prefixes, err := p.config.prefixes()
		if err != nil {
			return fmt.Errorf("get peer addresses: %w", err)
		}
		for _, pref := range prefixes {
			ipsetBuilder.AddPrefix(pref)
		}
	}
	ipset, err := ipsetBuilder.IPSet()
	if err != nil {
		return fmt.Errorf("build list of IP ranges for peers: %w", err)
	}

	// Add routes to the computed IP ranges via the WireGuard link.
	for _, prefix := range ipset.Prefixes() {
		dst := prefixToIPNet(prefix)
		if err = netlink.RouteAdd(
			&netlink.Route{
				LinkIndex: n.link.Attrs().Index,
				Scope:     netlink.SCOPE_LINK,
				Dst:       &dst,
			},
		); err != nil {
			if !errors.Is(err, unix.EEXIST) {
				return fmt.Errorf("add route to WireGuard link %q: %w", n.link.Attrs().Name, err)
			}
		} else {
			slog.Debug(
				"Added route to peer(s) via WireGuard interface.",
				"name", n.link.Attrs().Name, "dst", prefix,
			)
		}
	}

	// Remove old routes to IP ranges that are no longer in the configuration.
	addedRoutes := ipset.Prefixes()
	routes, err := netlink.RouteList(n.link, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("list routes on WireGuard link %q: %w", n.link.Attrs().Name, err)
	}
	for _, route := range routes {
		routePrefix, pErr := ipNetToPrefix(*route.Dst)
		if pErr != nil {
			return fmt.Errorf("parse route destination: %w", pErr)
		}
		if slices.Contains(addedRoutes, routePrefix) {
			continue
		}
		if err = netlink.RouteDel(&route); err != nil {
			return fmt.Errorf("remove route %q from WireGuard link %q: %w", route.Dst, n.link.Attrs().Name, err)
		}
		slog.Debug(
			"Removed route to peer(s) via WireGuard interface.",
			"name", n.link.Attrs().Name, "dst", routePrefix,
		)
	}
	return nil
}

func (n *WireGuardNetwork) Run(ctx context.Context) error {
	wg, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("create WireGuard client: %w", err)
	}
	defer wg.Close()

	n.mu.Lock()
	if n.running {
		n.mu.Unlock()
		return errors.New("network is already running")
	}
	n.running = true
	n.mu.Unlock()

	ticker := time.NewTicker(1 * time.Second)
	for {
		select {
		case <-ticker.C:
			n.mu.Lock()
			if err = n.updatePeersFromDevice(ctx); err != nil {
				slog.Error("Failed to update peers status from WireGuard interface.",
					"name", n.link.Attrs().Name, "err", err)
			}
			if err = n.changeWireGuardEndpoints(ctx); err != nil {
				slog.Error("Failed to update peer endpoints on WireGuard interface.",
					"name", n.link.Attrs().Name, "err", err)
			}
			n.mu.Unlock()
		case <-ctx.Done():
			for _, ch := range n.watchers {
				close(ch)
			}

			n.mu.Lock()
			n.running = false
			n.mu.Unlock()

			return nil
		}
	}
}

// WatchEndpoints returns a channel that receives endpoint change events for the WireGuard peers.
func (n *WireGuardNetwork) WatchEndpoints() <-chan EndpointChangeEvent {
	ch := make(chan EndpointChangeEvent)
	n.watchers = append(n.watchers, ch)
	return ch
}

// updatePeersFromDevice updates the peers status from the WireGuard device peers.
// mu lock must be held before calling this method.
func (n *WireGuardNetwork) updatePeersFromDevice(ctx context.Context) error {
	wg, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("create WireGuard client: %w", err)
	}
	defer wg.Close()

	dev, err := wg.Device(n.link.Attrs().Name)
	if err != nil {
		return fmt.Errorf("get WireGuard device %q: %w", n.link.Attrs().Name, err)
	}

	var events []EndpointChangeEvent
	for _, wgPeer := range dev.Peers {
		publicKey := secret.Secret(wgPeer.PublicKey[:])
		if p, ok := n.peers[publicKey.String()]; ok {
			endpointChanged := p.updateFromDevice(wgPeer)
			if endpointChanged {
				events = append(events, EndpointChangeEvent{
					PublicKey: publicKey,
					Endpoint:  *p.config.Endpoint,
				})
			}
		} else {
			// Assume that WG peers are not updated out of band so they should always be in sync with the config.
			slog.Warn("Found WireGuard peer that is not in the configuration.", "public_key", publicKey)
		}
	}

	if len(events) > 0 {
		if err = n.notifyWatchers(ctx, events); err != nil {
			// As of October 2024, the machine is the only watcher to persist changed endpoints in the machine
			// state which can tolerate missed events. Therefore, we can ignore the error here.
			slog.Error("Failed to notify watchers about a peer endpoint change.", "err", err)
		}
	}
	return nil
}

// changeWireGuardEndpoints rotates the endpoints of the WireGuard peers with 'down' status
// in an attempt to find a working one.
// mu lock must be held before calling this method.
func (n *WireGuardNetwork) changeWireGuardEndpoints(ctx context.Context) error {
	var wgPeerConfigs []wgtypes.PeerConfig
	var events []EndpointChangeEvent
	for _, p := range n.peers {
		newEndpoint, ok := p.shouldChangeEndpoint()
		if !ok {
			continue
		}
		newConfig := p.config
		newConfig.Endpoint = &newEndpoint
		p.updateConfig(newConfig)

		publicKey, err := wgtypes.NewKey(p.config.PublicKey)
		if err != nil {
			return fmt.Errorf("parse peer public key: %w", err)
		}
		wgPeerConfigs = append(wgPeerConfigs, wgtypes.PeerConfig{
			PublicKey:  publicKey,
			UpdateOnly: true,
			Endpoint: &net.UDPAddr{
				IP:   p.config.Endpoint.Addr().AsSlice(),
				Port: int(p.config.Endpoint.Port()),
			},
		})

		events = append(events, EndpointChangeEvent{
			PublicKey: p.config.PublicKey,
			Endpoint:  *p.config.Endpoint,
		})
	}
	if len(wgPeerConfigs) == 0 {
		// No changes to the endpoints.
		return nil
	}

	wg, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("create WireGuard client: %w", err)
	}
	defer wg.Close()

	wgConfigPatch := wgtypes.Config{
		ReplacePeers: false,
		Peers:        wgPeerConfigs,
	}
	// Apply the configuration patch to the WireGuard device.
	if err = wg.ConfigureDevice(n.link.Attrs().Name, wgConfigPatch); err != nil {
		return fmt.Errorf("configure WireGuard device %q with endpoint changes: %w", n.link.Attrs().Name, err)
	}
	for _, pc := range wgPeerConfigs {
		publicKey := secret.Secret(pc.PublicKey[:])
		slog.Info("Changed peer endpoint on WireGuard interface.",
			"name", n.link.Attrs().Name, "public_key", publicKey, "endpoint", pc.Endpoint,
			"status", n.peers[publicKey.String()].status)
	}

	if err = n.notifyWatchers(ctx, events); err != nil {
		// As of October 2024, the machine is the only watcher to persist changed endpoints in the machine
		// state which can tolerate missed events. Therefore, we can ignore the error here.
		slog.Error("Failed to notify watchers about a peer endpoint change.", "err", err)
	}

	return nil
}

// notifyWatchers notifies the watchers about peer endpoint changes.
func (n *WireGuardNetwork) notifyWatchers(ctx context.Context, events []EndpointChangeEvent) error {
	for _, ch := range n.watchers {
		for _, e := range events {
			select {
			case ch <- e:
			// Use a timeout to avoid blocking the network control loop.
			case <-time.After(1 * time.Second):
				return errors.New("timeout 1 second")
			case <-ctx.Done():
				return nil
			}
		}
	}
	return nil
}

// Cleanup deletes the WireGuard link. The network must not be running when this method is called.
func (n *WireGuardNetwork) Cleanup() error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.running {
		return errors.New("network is still running, stop it before cleanup")
	}

	// Delete the WireGuard link.
	name := n.link.Attrs().Name
	if err := netlink.LinkDel(n.link); err != nil {
		return fmt.Errorf("delete WireGuard link %q: %w", name, err)
	}
	n.link = nil
	slog.Info("Deleted WireGuard interface.", "name", name)

	return nil
}
