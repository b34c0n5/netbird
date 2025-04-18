//go:build !android

package routemanager

import (
	"context"
	"fmt"
	"net/netip"
	"sync"

	log "github.com/sirupsen/logrus"

	firewall "github.com/netbirdio/netbird/client/firewall/manager"
	"github.com/netbirdio/netbird/client/internal/peer"
	"github.com/netbirdio/netbird/client/internal/routemanager/iface"
	"github.com/netbirdio/netbird/route"
)

type serverRouter struct {
	mux            sync.Mutex
	ctx            context.Context
	routes         map[route.ID]*route.Route
	firewall       firewall.Manager
	wgInterface    iface.WGIface
	statusRecorder *peer.Status
}

func newServerRouter(ctx context.Context, wgInterface iface.WGIface, firewall firewall.Manager, statusRecorder *peer.Status) (*serverRouter, error) {
	return &serverRouter{
		ctx:            ctx,
		routes:         make(map[route.ID]*route.Route),
		firewall:       firewall,
		wgInterface:    wgInterface,
		statusRecorder: statusRecorder,
	}, nil
}

func (m *serverRouter) updateRoutes(routesMap map[route.ID]*route.Route) error {
	serverRoutesToRemove := make([]route.ID, 0)

	for routeID := range m.routes {
		update, found := routesMap[routeID]
		if !found || !update.Equal(m.routes[routeID]) {
			serverRoutesToRemove = append(serverRoutesToRemove, routeID)
		}
	}

	for _, routeID := range serverRoutesToRemove {
		oldRoute := m.routes[routeID]
		err := m.removeFromServerNetwork(oldRoute)
		if err != nil {
			log.Errorf("Unable to remove route id: %s, network %s, from server, got: %v",
				oldRoute.ID, oldRoute.Network, err)
		}
		delete(m.routes, routeID)
	}

	// If routing is to be disabled, do it after routes have been removed
	// If routing is to be enabled, do it before adding new routes; addToServerNetwork needs routing to be enabled
	if len(routesMap) > 0 {
		if err := m.firewall.EnableRouting(); err != nil {
			return fmt.Errorf("enable routing: %w", err)
		}
	} else {
		if err := m.firewall.DisableRouting(); err != nil {
			return fmt.Errorf("disable routing: %w", err)
		}
	}

	for id, newRoute := range routesMap {
		_, found := m.routes[id]
		if found {
			continue
		}

		err := m.addToServerNetwork(newRoute)
		if err != nil {
			log.Errorf("Unable to add route %s from server, got: %v", newRoute.ID, err)
			continue
		}
		m.routes[id] = newRoute
	}

	return nil
}

func (m *serverRouter) removeFromServerNetwork(route *route.Route) error {
	if m.ctx.Err() != nil {
		log.Infof("Not removing from server network because context is done")
		return m.ctx.Err()
	}

	m.mux.Lock()
	defer m.mux.Unlock()

	routerPair, err := routeToRouterPair(route)
	if err != nil {
		return fmt.Errorf("parse prefix: %w", err)
	}

	err = m.firewall.RemoveNatRule(routerPair)
	if err != nil {
		return fmt.Errorf("remove routing rules: %w", err)
	}

	delete(m.routes, route.ID)

	routeStr := route.Network.String()
	if route.IsDynamic() {
		routeStr = route.Domains.SafeString()
	}
	m.statusRecorder.RemoveLocalPeerStateRoute(routeStr)

	return nil
}

func (m *serverRouter) addToServerNetwork(route *route.Route) error {
	if m.ctx.Err() != nil {
		log.Infof("Not adding to server network because context is done")
		return m.ctx.Err()
	}

	m.mux.Lock()
	defer m.mux.Unlock()

	routerPair, err := routeToRouterPair(route)
	if err != nil {
		return fmt.Errorf("parse prefix: %w", err)
	}

	err = m.firewall.AddNatRule(routerPair)
	if err != nil {
		return fmt.Errorf("insert routing rules: %w", err)
	}

	m.routes[route.ID] = route

	routeStr := route.Network.String()
	if route.IsDynamic() {
		routeStr = route.Domains.SafeString()
	}

	m.statusRecorder.AddLocalPeerStateRoute(routeStr, route.GetResourceID())

	return nil
}

func (m *serverRouter) cleanUp() {
	m.mux.Lock()
	defer m.mux.Unlock()
	for _, r := range m.routes {
		routerPair, err := routeToRouterPair(r)
		if err != nil {
			log.Errorf("Failed to convert route to router pair: %v", err)
			continue
		}

		err = m.firewall.RemoveNatRule(routerPair)
		if err != nil {
			log.Errorf("Failed to remove cleanup route: %v", err)
		}

	}

	m.statusRecorder.CleanLocalPeerStateRoutes()
}

func routeToRouterPair(route *route.Route) (firewall.RouterPair, error) {
	// TODO: add ipv6
	source := getDefaultPrefix(route.Network)

	destination := route.Network.Masked()
	if route.IsDynamic() {
		// TODO: add ipv6 additionally
		destination = getDefaultPrefix(destination)
	}

	return firewall.RouterPair{
		ID:          route.ID,
		Source:      source,
		Destination: destination,
		Masquerade:  route.Masquerade,
	}, nil
}

func getDefaultPrefix(prefix netip.Prefix) netip.Prefix {
	if prefix.Addr().Is6() {
		return netip.PrefixFrom(netip.IPv6Unspecified(), 0)
	}
	return netip.PrefixFrom(netip.IPv4Unspecified(), 0)
}
